package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
)

// The instances in a space, and its members by the page.
//
// The origin keeps the list of instances mirroring a space and hands it to every mirror
// (in the snapshot, then /spaces/peers as it changes). A mirror stores it under its local
// space id in the same federated_space_peers table the origin uses, which is what lets a
// user's home instance send their presence and profile changes to every instance in the
// space itself: no instance ever relays what another instance's user is doing, so the
// "a peer speaks for its own users only" rule of /users/presence and /users/update holds
// with any number of instances.
//
// Members are served in pages so a space with tens of thousands of members can be
// mirrored: the snapshot carries the first page and a cursor, /spaces/members/list the
// rest.

var peerDomainRe = regexp.MustCompile(`^[a-z0-9.\-]+(?::\d+)?$`)

func validPeerDomain(d string) bool {
	return d != "" && len(d) <= 253 && peerDomainRe.MatchString(d)
}

// ---- origin ---------------------------------------------------------------------------

// addSpacePeer records an instance as mirroring a hosted space and tells the other
// mirrors, so their users' presence and profile changes reach it directly.
func (s *Service) addSpacePeer(ctx context.Context, spaceID int64, domain string) {
	known, err := s.repo.IsSpacePeer(ctx, spaceID, domain)
	if err != nil {
		logger.Err("federation", err, map[string]any{"space_id": spaceID, "peer": domain})
		return
	}
	if known {
		return
	}
	if err := s.repo.AddSpacePeer(ctx, spaceID, domain); err != nil {
		logger.Err("federation", err, map[string]any{"space_id": spaceID, "peer": domain})
		return
	}
	others := s.spacePeers(ctx, spaceID)
	delete(others, domain)
	if len(others) == 0 {
		return
	}
	s.fanOut(ctx, others, http.MethodPost, "/spaces/peers", SpacePeerEvent{Space: s.spaceRef(ctx, spaceID), Action: "add", Domain: domain})
}

// removeSpacePeer is the reverse: the last member from that instance is gone.
func (s *Service) removeSpacePeer(ctx context.Context, spaceID int64, domain string) {
	if err := s.repo.RemoveSpacePeer(ctx, spaceID, domain); err != nil {
		logger.Err("federation", err, map[string]any{"space_id": spaceID, "peer": domain})
		return
	}
	others := s.spacePeers(ctx, spaceID)
	if len(others) == 0 {
		return
	}
	s.fanOut(ctx, others, http.MethodPost, "/spaces/peers", SpacePeerEvent{Space: s.spaceRef(ctx, spaceID), Action: "remove", Domain: domain})
}

// memberPage is the page size this instance serves (FEDERATION_MEMBER_PAGE, capped).
func (s *Service) memberPage() int {
	n := s.fcfg.MemberPage
	if n <= 0 || n > maxMemberPage {
		n = maxMemberPage
	}
	return n
}

// membersPage is one page of a hosted space's members after a cursor, in the origin's
// id order, with the cursor for the page after it ("" when this was the last).
func (s *Service) membersPage(ctx context.Context, spaceID, after int64) ([]SpaceMemberWire, string, error) {
	list, next, err := s.spaceSvc.MembersPage(ctx, spaceID, after, s.memberPage())
	if err != nil {
		return nil, "", err
	}
	out := make([]SpaceMemberWire, 0, len(list))
	for i := range list {
		out = append(out, s.memberWire(&list[i].Member, list[i].User))
	}
	cursor := ""
	if next != 0 {
		cursor = id.Format(next)
	}
	return out, cursor, nil
}

// SpaceMembersList POST /spaces/members/list - a mirror fetches the next page of members.
func (h *Handler) SpaceMembersList(c fiber.Ctx) error {
	var body SpaceMembersQuery
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sp, err := h.hostedSpace(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	var after int64
	if body.After != "" {
		if after, err = id.Parse(body.After); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid cursor"})
		}
	}
	members, next, err := h.svc.membersPage(c.Context(), sp.ID, after)
	if err != nil {
		return fail(c, err, map[string]any{"space_id": sp.ID})
	}
	return c.JSON(SpaceMembersReply{Members: members, Next: next})
}

// ---- mirror ---------------------------------------------------------------------------

// completeSnapshot fetches the member pages a snapshot did not carry. If a page cannot be
// fetched the cursor stays set and the mirror is built from the members it has: a
// reconcile then adds but never removes members (MirrorSpec.MembersComplete).
func (s *Service) completeSnapshot(ctx context.Context, origin string, snap *SpaceSnapshot) {
	for snap.MembersNext != "" {
		if len(snap.Members) >= maxMirrorMembers {
			logger.Warn("federation", "space %s on %s has more than %d members; the mirror stops there", snap.Space.OriginSpaceID, origin, maxMirrorMembers)
			return
		}
		var reply SpaceMembersReply
		q := SpaceMembersQuery{Space: snap.Space, After: snap.MembersNext}
		if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/members/list", q, &reply); err != nil {
			logger.Err("federation", err, map[string]any{"peer": origin, "space": snap.Space.OriginSpaceID, "stage": "members"})
			return
		}
		if len(reply.Members) > maxMemberPage || reply.Next == snap.MembersNext {
			logger.Warn("federation", "bad member page from %s for space %s", origin, snap.Space.OriginSpaceID)
			return
		}
		snap.Members = append(snap.Members, reply.Members...)
		snap.MembersNext = reply.Next
	}
}

// storeMirrorPeers replaces what a mirror knows about the other instances in a space.
func (s *Service) storeMirrorPeers(ctx context.Context, spaceID int64, origin string, peers []string) {
	if err := s.repo.DeleteSpacePeers(ctx, spaceID); err != nil {
		logger.Err("federation", err, map[string]any{"space_id": spaceID})
		return
	}
	if len(peers) > maxSpacePeers {
		peers = peers[:maxSpacePeers]
	}
	for _, d := range peers {
		d = strings.ToLower(strings.TrimSpace(d))
		if !validPeerDomain(d) || s.IsLocalServer(d) || d == origin {
			continue
		}
		if err := s.repo.AddSpacePeer(ctx, spaceID, d); err != nil {
			logger.Err("federation", err, map[string]any{"space_id": spaceID, "peer": d})
		}
	}
}

func (s *Service) applySpacePeers(ctx context.Context, m *SpaceMapping, body SpacePeerEvent) error {
	d := strings.ToLower(strings.TrimSpace(body.Domain))
	if !validPeerDomain(d) || s.IsLocalServer(d) || d == m.OriginDomain {
		return nil
	}
	switch body.Action {
	case "add":
		return s.repo.AddSpacePeer(ctx, m.SpaceID, d)
	case "remove":
		return s.repo.RemoveSpacePeer(ctx, m.SpaceID, d)
	}
	return errUnknownAction
}

// SpacePeers POST /spaces/peers - the origin says an instance joined or left the space.
func (h *Handler) SpacePeers(c fiber.Ctx) error {
	var body SpacePeerEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, err := h.mirrorFor(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	return h.applied(c, m, h.svc.applySpacePeers(c.Context(), m, body))
}
