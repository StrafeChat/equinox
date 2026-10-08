package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/discover"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// The Discover directory across instances: every instance serves what it lists at
// GET /discover, and each one asks its peers for theirs on the maintenance timer (see
// discover.Service.RefreshRemote - this file is only the wire).
//
// Reading a peer's directory needs nothing but a signature from an instance the local
// policy allows: the same trust boundary as messages and spaces. A blocked instance is
// refused by the middleware before it reaches here.

// DirectorySource is the local Discover module, as this engine needs it. Set from route
// setup; nil leaves the endpoints answering "nothing listed", which is also what an
// instance with DISCOVER_FEDERATION off wants.
type DirectorySource interface {
	// SharedListings is what this instance tells a peer it lists.
	SharedListings(ctx context.Context) ([]discover.RemoteListing, error)
	// SharesSpace reports whether a space hosted here is listed and shared, which is what
	// lets a peer's user join it without an invite.
	SharesSpace(ctx context.Context, spaceID int64) (bool, error)
}

func (s *Service) SetDirectory(d DirectorySource) { s.directory = d }

// DirectoryReply: GET /discover - the spaces this instance lists and shares.
type DirectoryReply struct {
	Spaces []DirectorySpace `json:"spaces"`
}

// DirectorySpace is one card. Ids are the serving instance's own.
type DirectorySpace struct {
	SpaceID     string   `json:"space_id"`
	Name        string   `json:"name"`
	NameAcronym string   `json:"name_acronym,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Banner      string   `json:"banner,omitempty"`
	Description string   `json:"description,omitempty"`
	Tagline     string   `json:"tagline,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	MemberCount int      `json:"member_count"`
	OnlineCount int      `json:"online_count"`
}

// SpaceJoinListedRequest: POST /spaces/join_listed - a peer's user joining a space this
// instance lists publicly. No invite: the listing is the permission.
type SpaceJoinListedRequest struct {
	Space    SpaceRef             `json:"space"`
	User     Profile              `json:"user"`
	Presence *auth.PublicPresence `json:"presence,omitempty"`
}

// Directory GET /discover - serves this instance's shared listings to a peer.
func (h *Handler) Directory(c fiber.Ctx) error {
	reply := DirectoryReply{Spaces: []DirectorySpace{}}
	if h.svc.directory == nil {
		return c.JSON(reply)
	}
	listings, err := h.svc.directory.SharedListings(c.Context())
	if err != nil {
		return fail(c, err, map[string]any{"stage": "directory"})
	}
	for i := range listings {
		l := listings[i]
		reply.Spaces = append(reply.Spaces, DirectorySpace{
			SpaceID:     id.Format(l.SpaceID),
			Name:        l.Name,
			NameAcronym: l.NameAcronym,
			Icon:        l.Icon,
			Banner:      l.Banner,
			Description: l.Description,
			Tagline:     l.Tagline,
			Tags:        l.Tags,
			MemberCount: l.MemberCount,
			OnlineCount: l.OnlineCount,
		})
	}
	return c.JSON(reply)
}

// SpaceJoinListed POST /spaces/join_listed - add a peer's user to a space listed here.
// The listing is checked again on this side: a peer holding a stale card, or inventing
// one, cannot use it to get into a space that is not publicly listed.
func (h *Handler) SpaceJoinListed(c fiber.Ctx) error {
	var body SpaceJoinListedRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	if _, domain, err := ParseFID(body.User.FID); err != nil || domain != requester {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": errForeignUser.Error()})
	}
	if !h.svc.IsLocalServer(body.Space.OriginDomain) {
		return spaceFail(c, errNotSpaceOrigin, nil)
	}
	spaceID, err := id.Parse(body.Space.OriginSpaceID)
	if err != nil {
		return spaceFail(c, ErrInvalidFID, nil)
	}
	ctx := WithExcludedPeer(c.Context(), requester)
	// Not listed (or no longer shared): say so plainly. The peer may be holding a card
	// from before the managers changed their mind, and "not listed" is the honest answer
	// to pass back to whoever clicked join - spaceFail would make it a 500.
	notListed := func() error {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": discover.ErrNotApproved.Error()})
	}
	if h.svc.directory == nil {
		return notListed()
	}
	shared, err := h.svc.directory.SharesSpace(ctx, spaceID)
	if err != nil {
		return fail(c, err, map[string]any{"space_id": spaceID})
	}
	if !shared {
		return notListed()
	}
	user, err := h.svc.EnsureShadow(ctx, requester, body.User)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	if body.Presence != nil {
		h.svc.applyRelayedPresence(ctx, user, *body.Presence)
	}
	sp, err := h.svc.spaceSvc.JoinListed(ctx, user.ID, spaceID)
	if err != nil {
		return spaceFail(c, err, map[string]any{"peer": requester, "space_id": spaceID})
	}
	// From now on this peer mirrors the space, exactly as an invite join would record.
	h.svc.ensureHosted(ctx, sp.ID)
	h.svc.addSpacePeer(ctx, sp.ID, requester)
	snap, err := h.svc.snapshot(ctx, sp)
	if err != nil {
		return fail(c, err, map[string]any{"space_id": sp.ID})
	}
	return c.JSON(snap)
}

// ---- the outbound half (discover.Federator) --------------------------------------------

// PeerDomains is every instance this one may talk to: the ones it has met and the ones
// configured statically, minus anything the allow/block policy refuses.
func (s *Service) PeerDomains(ctx context.Context) []string {
	if !s.fcfg.Enabled {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(domain string) {
		if domain == "" || seen[domain] || s.IsLocalServer(domain) || !s.fcfg.IsAllowedPeer(domain) {
			return
		}
		seen[domain] = true
		out = append(out, domain)
	}
	for domain := range s.fcfg.StaticPeers {
		add(domain)
	}
	peers, err := s.repo.ListPeers(ctx)
	if err != nil {
		logger.Err("federation", err, map[string]any{"stage": "peer_domains"})
		return out
	}
	for _, p := range peers {
		if p.Blocked {
			continue
		}
		add(p.Domain)
	}
	return out
}

// FetchDirectory asks one peer what it lists.
func (s *Service) FetchDirectory(ctx context.Context, domain string) ([]discover.RemoteListing, error) {
	var reply DirectoryReply
	if _, err := s.client.Do(ctx, domain, http.MethodGet, "/discover", nil, &reply); err != nil {
		return nil, err
	}
	out := make([]discover.RemoteListing, 0, len(reply.Spaces))
	for _, sp := range reply.Spaces {
		spaceID, err := id.Parse(sp.SpaceID)
		if err != nil {
			continue
		}
		out = append(out, discover.RemoteListing{
			Domain:      domain,
			SpaceID:     spaceID,
			Name:        sp.Name,
			NameAcronym: sp.NameAcronym,
			Icon:        sp.Icon,
			Banner:      sp.Banner,
			Description: sp.Description,
			Tagline:     sp.Tagline,
			Tags:        sp.Tags,
			MemberCount: sp.MemberCount,
			OnlineCount: sp.OnlineCount,
			FetchedAt:   time.Now().UTC(),
		})
	}
	return out, nil
}

// JoinListedSpace joins a space `domain` lists publicly, on behalf of a local user, and
// builds this instance's mirror of it from the answer - the same path an invite join takes.
func (s *Service) JoinListedSpace(ctx context.Context, domain string, originSpaceID, userID int64) (*spaces.Space, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user == nil || user.IsRemote() {
		return nil, ErrRemoteUserNotFound
	}
	presence := auth.ToPublicPresence(user.Presence, true)
	req := SpaceJoinListedRequest{
		Space:    SpaceRef{OriginDomain: domain, OriginSpaceID: id.Format(originSpaceID)},
		User:     s.ProfileOf(user),
		Presence: &presence,
	}
	var snap SpaceSnapshot
	if _, err := s.client.Do(ctx, domain, http.MethodPost, "/spaces/join_listed", req, &snap); err != nil {
		return nil, spaceOriginError(err, domain, "/spaces/join_listed")
	}
	return s.applyJoinSnapshot(ctx, domain, user, &snap)
}
