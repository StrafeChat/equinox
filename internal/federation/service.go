package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/scylladb/gocqlx/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/devices"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/voice"
	"github.com/StrafeChat/equinox/internal/safego"
)

// Service is the federation engine: outbound relays (implements rooms.Federator,
// messages.Federator, spaces.Federator, devices.KeyRouter/FIDResolver) plus the state
// inbound handlers use.
type Service struct {
	cfg       *config.Config
	fcfg      config.FederationConfig
	redis     *redis.Client
	signer    *Signer
	discovery *Discovery
	client    *Client
	repo      Repository
	users     auth.UserRepository
	roomRepo  rooms.Repository
	roomSvc   *rooms.Service
	msgRepo   messages.Repository
	msgSvc    *messages.Service
	devSvc    *devices.Service
	relSvc    *relationships.Service
	// spaceSvc applies what peers say about spaces and serves their members' requests
	// (SetSpaces hands in the API's fully wired instance; until then an internal one).
	spaceSvc *spaces.Service
	// voice is the API's voice service (SetVoice); nil in a process without voice, where
	// relayed calls are acknowledged and ignored.
	voice *voice.Service

	// Relays to a peer go out in the order they were made, one at a time: ephemeral ones
	// (typing, presence, calls) through an in-memory queue per peer, everything else
	// through a durable per-peer worker over federation_outbox (see outbox.go).
	outMu     sync.Mutex
	outQueues map[string]chan func()
	outboxes  map[string]*peerOutbox
	failing   map[string]bool // peers the outbox currently cannot reach
	outboxOn  bool            // this process drains the outbox (the API; see startOutbox)
	outboxCtx context.Context
	// deliver hands one relay to a peer: the signed client in production, a stub in tests.
	deliver func(ctx context.Context, domain, method, path string, body any) error
}

// outboxDepth bounds each peer's in-memory queue of ephemeral relays; beyond it they are
// dropped and logged rather than growing memory without limit while a peer is down.
const outboxDepth = 4096

// New builds the engine with its own repository/service instances (stateless wrappers
// over the same Scylla session the API uses).
func New(cfg *config.Config, session gocqlx.Session, rdb *redis.Client) (*Service, error) {
	if !cfg.Federation.Enabled {
		return nil, ErrFederationOff
	}
	signer, err := LoadSigner(cfg.Federation)
	if err != nil {
		return nil, err
	}
	return newService(cfg, session, rdb, signer), nil
}

// NewWithExistingKey is New for a process that shares the API's signing key but must never
// create it (the gateway): until the API has written the key this fails instead of
// minting a second identity that no peer would accept.
func NewWithExistingKey(cfg *config.Config, session gocqlx.Session, rdb *redis.Client) (*Service, error) {
	if !cfg.Federation.Enabled {
		return nil, ErrFederationOff
	}
	signer, err := LoadExistingSigner(cfg.Federation)
	if err != nil {
		return nil, err
	}
	return newService(cfg, session, rdb, signer), nil
}

func newService(cfg *config.Config, session gocqlx.Session, rdb *redis.Client, signer *Signer) *Service {
	users := auth.NewCachedUserRepository(auth.NewUserRepository(session), rdb, cfg)
	roomRepo := rooms.NewRepository(session)
	spaceSvc := spaces.NewService(spaces.NewRepository(session), roomRepo, users, rdb, cfg)
	roomSvc := rooms.NewService(roomRepo, users, rdb, cfg, nil, spaceSvc)
	msgRepo := messages.NewRepository(session)
	msgSvc := messages.NewService(msgRepo, roomRepo, users, rdb, cfg, spaceSvc)
	devSvc := devices.NewService(devices.NewRepository(session), rdb, cfg)
	relSvc := relationships.NewService(relationships.NewRepository(session), users, rdb, cfg)
	discovery := NewDiscovery(cfg.Federation)
	s := &Service{
		cfg:       cfg,
		fcfg:      cfg.Federation,
		redis:     rdb,
		signer:    signer,
		discovery: discovery,
		client:    NewClient(cfg.Federation, signer, discovery),
		repo:      NewRepository(session),
		users:     users,
		roomRepo:  roomRepo,
		roomSvc:   roomSvc,
		msgRepo:   msgRepo,
		msgSvc:    msgSvc,
		devSvc:    devSvc,
		relSvc:    relSvc,
		spaceSvc:  spaceSvc,
		outQueues: map[string]chan func(){},
		outboxes:  map[string]*peerOutbox{},
		failing:   map[string]bool{},
		outboxCtx: context.Background(),
	}
	s.deliver = s.deliverWith
	roomSvc.SetFederationInfo(s)
	devSvc.SetRouter(s)
	// Inbound events apply through relSvc; it relays only for the accept a crossed
	// request produces, which the peer has not seen.
	relSvc.SetFederator(s)
	// A member of another instance writing into a space hosted here goes through msgSvc
	// and spaceSvc like a local member, and what they do must reach every other mirror.
	msgSvc.SetFederator(s)
	spaceSvc.SetFederator(s)
	logger.Info("federation", "enabled as %s (key %s)", cfg.Federation.Domain, signer.KeyID)
	return s
}

// SetSpaces hands in the API's spaces service (the one with the system-room messenger),
// so a remote member's join posts the same notice a local join does.
func (s *Service) SetSpaces(svc *spaces.Service) {
	if svc != nil {
		s.spaceSvc = svc
	}
}

// infoProvider is the read-only slice of the engine (room / space → global identity)
// for processes that never relay anything, like the gateway's READY builder.
type infoProvider struct {
	repo   Repository
	domain string
}

// NewInfoProvider builds the read-only federation view; domain is this instance's own.
func NewInfoProvider(session gocqlx.Session, domain string) *infoProvider {
	return &infoProvider{repo: NewRepository(session), domain: strings.ToLower(domain)}
}

func (p *infoProvider) RoomFederation(ctx context.Context, roomID int64) *rooms.Federation {
	return roomFederation(ctx, p.repo, roomID)
}

func (p *infoProvider) RoomFederations(ctx context.Context, roomIDs []int64) map[int64]*rooms.Federation {
	return roomFederations(ctx, p.repo, roomIDs)
}

func (p *infoProvider) SpaceFederation(ctx context.Context, spaceID int64) *spaces.SpaceFederation {
	return spaceFederation(ctx, p.repo, spaceID, func(d string) bool { return d == "" || d == p.domain || d == LegacyServer })
}

func roomFederation(ctx context.Context, repo Repository, roomID int64) *rooms.Federation {
	m, err := repo.GetRoomMapping(ctx, roomID)
	if err != nil || m == nil {
		return nil
	}
	return &rooms.Federation{OriginDomain: m.OriginDomain, OriginID: id.Format(m.OriginRoomID)}
}

func roomFederations(ctx context.Context, repo Repository, roomIDs []int64) map[int64]*rooms.Federation {
	ms, err := repo.GetRoomMappings(ctx, roomIDs)
	if err != nil {
		return nil
	}
	out := make(map[int64]*rooms.Federation, len(ms))
	for rid, m := range ms {
		out[rid] = &rooms.Federation{OriginDomain: m.OriginDomain, OriginID: id.Format(m.OriginRoomID)}
	}
	return out
}

// spaceFederation is a space's global identity when another instance hosts it.
func spaceFederation(ctx context.Context, repo Repository, spaceID int64, isLocal func(string) bool) *spaces.SpaceFederation {
	m, err := repo.GetSpaceMapping(ctx, spaceID)
	if err != nil || m == nil || isLocal(m.OriginDomain) {
		return nil
	}
	return &spaces.SpaceFederation{OriginDomain: m.OriginDomain, OriginID: id.Format(m.OriginSpaceID)}
}

func (s *Service) RoomFederations(ctx context.Context, roomIDs []int64) map[int64]*rooms.Federation {
	return roomFederations(ctx, s.repo, roomIDs)
}

func (s *Service) SpaceFederation(ctx context.Context, spaceID int64) *spaces.SpaceFederation {
	return spaceFederation(ctx, s.repo, spaceID, s.IsLocalServer)
}

// ---- relay targets --------------------------------------------------------------------

type ctxKey int

const (
	ctxKeyExcludedPeer ctxKey = iota
	ctxKeyRelayCapture
)

// WithExcludedPeer marks the instance whose request is being served, so the relays the
// request causes skip it: it applies the reply itself, and a copy would race it.
func WithExcludedPeer(ctx context.Context, domain string) context.Context {
	return context.WithValue(ctx, ctxKeyExcludedPeer, domain)
}

func excludedPeer(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyExcludedPeer).(string)
	return v
}

// RelayEvent is one relay as it would have gone out: the request a peer would have
// received. The origin hands these back in the reply to a request that caused them, so
// the asking instance applies exactly what every other mirror gets.
type RelayEvent struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

type relayCapture struct {
	mu     sync.Mutex
	events []RelayEvent
}

func (c *relayCapture) add(method, path string, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	c.mu.Lock()
	c.events = append(c.events, RelayEvent{Method: method, Path: path, Body: raw})
	c.mu.Unlock()
}

func (c *relayCapture) take() []RelayEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.events
	c.events = nil
	if out == nil {
		out = []RelayEvent{}
	}
	return out
}

// WithRelayCapture is WithExcludedPeer where the relays meant for the excluded instance
// are collected instead of dropped, to be returned in the reply.
func WithRelayCapture(ctx context.Context, domain string) (context.Context, *relayCapture) {
	c := &relayCapture{}
	ctx = context.WithValue(WithExcludedPeer(ctx, domain), ctxKeyRelayCapture, c)
	return ctx, c
}

// fanOut relays one request to every listed instance but the excluded one (whose copy is
// captured when the request asked for that).
func (s *Service) fanOut(ctx context.Context, domains map[string]struct{}, method, path string, body any) {
	skip := excludedPeer(ctx)
	for domain := range domains {
		if domain == skip {
			if c, _ := ctx.Value(ctxKeyRelayCapture).(*relayCapture); c != nil {
				c.add(method, path, body)
			}
			continue
		}
		if !s.fcfg.IsAllowedPeer(domain) {
			continue
		}
		s.send(domain, method, path, body)
	}
}

// spacePeers is the set of instances mirroring a space: on the origin every mirror, on a
// mirror the other mirrors as the origin reported them (its own self excluded).
func (s *Service) spacePeers(ctx context.Context, spaceID int64) map[string]struct{} {
	list, err := s.repo.ListSpacePeers(ctx, spaceID)
	if err != nil {
		logger.Err("federation", err, map[string]any{"space_id": spaceID})
		return nil
	}
	out := make(map[string]struct{}, len(list))
	for _, d := range list {
		if !s.IsLocalServer(d) {
			out[d] = struct{}{}
		}
	}
	return out
}

// spaceDomains is every other instance in a space: its origin (when that is not this
// instance) and its mirrors.
func (s *Service) spaceDomains(ctx context.Context, spaceID int64) map[string]struct{} {
	m, err := s.repo.GetSpaceMapping(ctx, spaceID)
	if err != nil || m == nil {
		return nil // a space nobody else has joined: nothing federates yet
	}
	out := s.spacePeers(ctx, spaceID)
	if out == nil {
		out = map[string]struct{}{}
	}
	if !s.IsLocalServer(m.OriginDomain) {
		out[m.OriginDomain] = struct{}{}
	}
	return out
}

// relayTargets resolves who must hear about something that happened in a room: for a PM
// or group, the home instances of its remote participants; for a channel of a space
// hosted here, the instances mirroring the space; for a channel of a space hosted
// elsewhere, nobody - writes there go to the origin, which tells everyone - except that
// origin is returned for the few things a mirror sends on its own (typing).
func (s *Service) relayTargets(ctx context.Context, roomID int64, participants []int64) (targets map[string]struct{}, origin string) {
	room, err := s.roomRepo.GetByID(ctx, roomID)
	if err != nil || room == nil || room.SpaceID == nil {
		byDomain, _, err := s.remotePeers(ctx, participants)
		if err != nil {
			return nil, ""
		}
		targets = make(map[string]struct{}, len(byDomain))
		for d := range byDomain {
			targets[d] = struct{}{}
		}
		return targets, ""
	}
	m, err := s.repo.GetSpaceMapping(ctx, *room.SpaceID)
	if err != nil || m == nil {
		return nil, "" // a space nobody else has joined: nothing federates yet
	}
	if !s.IsLocalServer(m.OriginDomain) {
		return nil, m.OriginDomain
	}
	return s.spacePeers(ctx, *room.SpaceID), ""
}

// spaceDomainsForUser is every instance that shares a space with the user: the mirrors
// of spaces hosted here that they are in, and the origin and other mirrors of spaces
// they mirror (the origin tells each mirror who else is in the space, so a user's home
// instance reaches all of them directly - nobody relays another instance's presence).
func (s *Service) spaceDomainsForUser(ctx context.Context, u *auth.User) map[string]struct{} {
	out := map[string]struct{}{}
	for _, spaceID := range u.Spaces {
		for d := range s.spaceDomains(ctx, spaceID) {
			out[d] = struct{}{}
		}
	}
	return out
}

// ---- identity -------------------------------------------------------------------------

func (s *Service) Enabled() bool  { return s.fcfg.Enabled }
func (s *Service) Domain() string { return s.fcfg.Domain }

// Info is this instance's /.well-known/strafe document.
func (s *Service) Info() *InstanceInfo {
	return &InstanceInfo{
		Domain:        s.fcfg.Domain,
		Version:       1,
		APIURL:        s.fcfg.PublicURL,
		GatewayURL:    s.fcfg.GatewayURL,
		FederationURL: s.fcfg.PublicURL + BasePath,
		KeyID:         s.signer.KeyID,
		PublicKey:     s.signer.PublicKeyBase64(),
		Software:      SoftwareInfo{Name: "strafe-equinox", Version: s.cfg.App.Version},
	}
}

// IsLocalServer: the server part of @id:server names this instance (or the pre-federation
// synthetic name, or nothing at all).
func (s *Service) IsLocalServer(server string) bool {
	server = strings.ToLower(strings.TrimSpace(server))
	return server == "" || server == s.fcfg.Domain || server == LegacyServer
}

func (s *Service) LocalFID(userID int64) string {
	return FormatFID(userID, s.fcfg.Domain)
}

// FIDOf is a user's federated id: their home instance's id for them at their home domain.
func (s *Service) FIDOf(u *auth.User) string {
	if u.IsRemote() {
		return FormatFID(*u.RemoteID, u.HomeDomain)
	}
	return s.LocalFID(u.ID)
}

// ResolveLocalID maps a federated id to the local row that represents it.
func (s *Service) ResolveLocalID(ctx context.Context, fid string) (int64, error) {
	originID, domain, err := ParseFID(fid)
	if err != nil {
		return 0, err
	}
	if s.IsLocalServer(domain) {
		return originID, nil
	}
	u, err := s.users.GetByRemote(ctx, domain, originID)
	if err != nil {
		return 0, err
	}
	if u == nil {
		return 0, ErrRemoteUserNotFound
	}
	return u.ID, nil
}

func (s *Service) ProfileOf(u *auth.User) Profile {
	return Profile{
		FID:         s.FIDOf(u),
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Avatar:      u.Avatar,
		Banner:      u.Banner,
		Bio:         u.Bio,
		AboutMe:     u.AboutMe,
		Bot:         u.Bot,
	}
}

func (s *Service) RoomFederation(ctx context.Context, roomID int64) *rooms.Federation {
	return roomFederation(ctx, s.repo, roomID)
}

// ResolveHandle turns "name@domain" into a local user row: the local user for this
// instance's own domain (or no domain), or an up-to-date shadow row fetched from the
// remote instance.
func (s *Service) ResolveHandle(ctx context.Context, raw string) (*auth.User, error) {
	h, err := ParseHandle(raw)
	if err != nil {
		return nil, err
	}
	if h.Domain == "" || h.Domain == s.fcfg.Domain {
		u, err := s.users.GetByUsername(ctx, h.Username)
		if err != nil {
			return nil, err
		}
		if u == nil {
			return nil, ErrRemoteUserNotFound
		}
		return u, nil
	}
	if !s.fcfg.IsAllowedPeer(h.Domain) {
		return nil, ErrPeerNotAllowed
	}
	var p Profile
	q := url.Values{}
	q.Set("username", h.Username)
	if _, err := s.client.Do(ctx, h.Domain, http.MethodGet, "/users/lookup?"+q.Encode(), nil, &p); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil, ErrRemoteUserNotFound
		}
		return nil, err
	}
	return s.EnsureShadow(ctx, h.Domain, p)
}

// clampProfile applies the same field limits a local profile has to one a peer sent, so
// a remote instance cannot store megabytes in our users table, and drops avatar/banner
// values that are not http(s) URLs (every client renders them as <img src>).
func clampProfile(p Profile) Profile {
	clamp := func(s string, max int) string {
		if r := []rune(strings.TrimSpace(s)); len(r) > max {
			return string(r[:max])
		}
		return strings.TrimSpace(s)
	}
	httpOnly := func(u string) string {
		u = strings.TrimSpace(u)
		if len(u) > 256 || (!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://")) {
			return ""
		}
		return u
	}
	p.Username = clamp(p.Username, 32)
	p.DisplayName = clamp(p.DisplayName, 32)
	p.Bio = clamp(p.Bio, 190)
	p.AboutMe = clamp(p.AboutMe, 190)
	p.Avatar = httpOnly(p.Avatar)
	p.Banner = httpOnly(p.Banner)
	return p
}

// EnsureShadow creates or refreshes the local shadow row for a remote user. The profile's
// FID must be at `domain` - a peer can only vouch for its own users.
func (s *Service) EnsureShadow(ctx context.Context, domain string, p Profile) (*auth.User, error) {
	p = clampProfile(p)
	originID, fidDomain, err := ParseFID(p.FID)
	if err != nil {
		return nil, err
	}
	if fidDomain != domain {
		return nil, fmt.Errorf("federation: profile %s is not a %s user", p.FID, domain)
	}
	if fidDomain == s.fcfg.Domain {
		u, err := s.users.GetByID(ctx, originID)
		if err != nil {
			return nil, err
		}
		if u == nil {
			return nil, ErrRemoteUserNotFound
		}
		return u, nil
	}
	existing, err := s.users.GetByRemote(ctx, domain, originID)
	if err != nil {
		return nil, err
	}
	u := existing
	if u == nil {
		rid := originID
		u = &auth.User{ID: id.Next(), HomeDomain: domain, RemoteID: &rid, CreatedAt: time.Now().UTC()}
	} else if u.Username == p.Username && u.DisplayName == p.DisplayName &&
		u.Avatar == p.Avatar && u.Banner == p.Banner && u.Bio == p.Bio && u.AboutMe == p.AboutMe && u.Bot == p.Bot {
		return u, nil
	}
	u.Username = p.Username
	u.DisplayName = p.DisplayName
	u.Avatar = p.Avatar
	u.Banner = p.Banner
	u.Bio = p.Bio
	u.AboutMe = p.AboutMe
	u.Bot = p.Bot
	if u.DisplayName == "" {
		u.DisplayName = u.Username
	}
	if err := s.users.UpsertShadow(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// resolveProfile turns a profile in an inbound payload into a local row. Profiles the
// requesting instance vouches for (its own users) are trusted as-is; a third instance's
// user is fetched from their home so one peer can't spoof another's profile.
func (s *Service) resolveProfile(ctx context.Context, p Profile, requester string) (*auth.User, error) {
	originID, domain, err := ParseFID(p.FID)
	if err != nil {
		return nil, err
	}
	if s.IsLocalServer(domain) {
		u, err := s.users.GetByID(ctx, originID)
		if err != nil {
			return nil, err
		}
		if u == nil || u.IsRemote() {
			return nil, ErrRemoteUserNotFound
		}
		return u, nil
	}
	if domain == requester {
		return s.EnsureShadow(ctx, domain, p)
	}
	if existing, err := s.users.GetByRemote(ctx, domain, originID); err == nil && existing != nil {
		return existing, nil
	}
	if !s.fcfg.IsAllowedPeer(domain) {
		return nil, ErrPeerNotAllowed
	}
	var fetched Profile
	if _, err := s.client.Do(ctx, domain, http.MethodGet, "/users/"+id.Format(originID), nil, &fetched); err != nil {
		return nil, err
	}
	return s.EnsureShadow(ctx, domain, fetched)
}

// ---- outbound relays ------------------------------------------------------------------

// shadowByFID is the local row for a federated id: the user themselves when local, their
// shadow when one exists, else the profile is fetched from their home instance and a
// shadow created - so a reference to a user nobody here has met yet still resolves.
func (s *Service) shadowByFID(ctx context.Context, fid string) (*auth.User, error) {
	originID, domain, err := ParseFID(fid)
	if err != nil {
		return nil, err
	}
	if s.IsLocalServer(domain) {
		u, err := s.users.GetByID(ctx, originID)
		if err != nil {
			return nil, err
		}
		if u == nil || u.IsRemote() {
			return nil, ErrRemoteUserNotFound
		}
		return u, nil
	}
	if u, err := s.users.GetByRemote(ctx, domain, originID); err != nil {
		return nil, err
	} else if u != nil {
		return u, nil
	}
	if !s.fcfg.IsAllowedPeer(domain) {
		return nil, ErrPeerNotAllowed
	}
	var fetched Profile
	if _, err := s.client.Do(ctx, domain, http.MethodGet, "/users/"+id.Format(originID), nil, &fetched); err != nil {
		return nil, err
	}
	return s.EnsureShadow(ctx, domain, fetched)
}

// remotePeers groups a room's participants by home instance (local users excluded).
func (s *Service) remotePeers(ctx context.Context, participantIDs []int64) (map[string][]*auth.User, []*auth.User, error) {
	if len(participantIDs) == 0 {
		return nil, nil, nil
	}
	list, err := s.users.GetByIDs(ctx, participantIDs)
	if err != nil {
		return nil, nil, err
	}
	byDomain := map[string][]*auth.User{}
	var all []*auth.User
	for _, u := range list {
		if u == nil {
			continue
		}
		all = append(all, u)
		if u.IsRemote() && s.fcfg.IsAllowedPeer(u.HomeDomain) {
			byDomain[u.HomeDomain] = append(byDomain[u.HomeDomain], u)
		}
	}
	return byDomain, all, nil
}

// roomRef returns the room's global identity, registering the room as origin-local when
// it's federating for the first time.
func (s *Service) roomRef(ctx context.Context, roomID int64) (RoomRef, error) {
	m, err := s.repo.GetRoomMapping(ctx, roomID)
	if err != nil {
		return RoomRef{}, err
	}
	if m == nil {
		m = &RoomMapping{RoomID: roomID, OriginDomain: s.fcfg.Domain, OriginRoomID: roomID}
		if err := s.repo.PutRoomMapping(ctx, m); err != nil {
			return RoomRef{}, err
		}
	}
	return RoomRef{OriginDomain: m.OriginDomain, OriginRoomID: id.Format(m.OriginRoomID)}, nil
}

func (s *Service) messageRef(ctx context.Context, roomID, msgID int64) MessageRef {
	if m, err := s.repo.GetMessageMapping(ctx, roomID, msgID); err == nil && m != nil {
		return MessageRef{OriginDomain: m.OriginDomain, OriginMessageID: id.Format(m.OriginMessageID)}
	}
	return MessageRef{OriginDomain: s.fcfg.Domain, OriginMessageID: id.Format(msgID)}
}

// enqueue runs job on the peer's in-memory worker (ephemeral relays; see send in
// outbox.go). Jobs to one peer run in order, one at a time.
func (s *Service) enqueue(domain string, job func()) {
	s.outMu.Lock()
	q, ok := s.outQueues[domain]
	if !ok {
		q = make(chan func(), outboxDepth)
		s.outQueues[domain] = q
		// safego: this runs outside any request, so nothing else would catch a panic - and
		// a peer sending back something unexpected must not take the whole API down. One
		// panicking job must not kill the peer's worker either, hence the per-job recover.
		safego.Go("federation", func() {
			for job := range q {
				runRelay(domain, job)
			}
		})
	}
	s.outMu.Unlock()
	select {
	case q <- job:
	default:
		logger.Warn("federation", "outbox for %s is full, dropping a relay", domain)
	}
}

func runRelay(domain string, job func()) {
	defer func() {
		if r := recover(); r != nil {
			logger.Err("federation", fmt.Errorf("relay panic: %v", r), map[string]any{"peer": domain})
		}
	}()
	job()
}

func (s *Service) touchPeer(ctx context.Context, domain string) {
	if s.discovery == nil || s.repo == nil {
		return
	}
	info, err := s.discovery.Lookup(ctx, domain, false)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	p, _ := s.repo.GetPeer(ctx, domain)
	if p == nil {
		p = &Peer{Domain: domain, FirstSeen: now}
	}
	p.KeyID = info.KeyID
	p.PublicKey = info.PublicKey
	p.FederationURL = info.FederationURL
	p.APIURL = info.APIURL
	p.LastSeen = now
	_ = s.repo.UpsertPeer(ctx, p)
}

func (s *Service) profiles(list []*auth.User, only map[int64]struct{}) []Profile {
	out := make([]Profile, 0, len(list))
	for _, u := range list {
		if only != nil {
			if _, ok := only[u.ID]; !ok {
				continue
			}
		}
		out = append(out, s.ProfileOf(u))
	}
	return out
}

func idSet(ids []int64) map[int64]struct{} {
	m := make(map[int64]struct{}, len(ids))
	for _, v := range ids {
		m[v] = struct{}{}
	}
	return m
}

func (s *Service) AfterRoomCreated(ctx context.Context, rwp *rooms.RoomWithParticipants) {
	byDomain, all, err := s.remotePeers(ctx, rwp.ParticipantIDs)
	if err != nil || len(byDomain) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, rwp.ID)
	if err != nil {
		logger.Err("federation", err, map[string]any{"room_id": rwp.ID})
		return
	}
	body := RoomAnnounce{
		Room:         ref,
		Type:         rwp.Type,
		Name:         rwp.Name,
		E2EEEnabled:  rwp.E2EEEnabled == nil || *rwp.E2EEEnabled,
		Participants: s.profiles(all, nil),
	}
	for _, u := range all {
		if u.ID == rwp.CreatorID {
			body.Creator = s.FIDOf(u)
		}
	}
	for domain := range byDomain {
		s.send(domain, http.MethodPost, "/rooms", body)
	}
}

func (s *Service) AfterParticipantsChanged(ctx context.Context, rwp *rooms.RoomWithParticipants, removed []int64) {
	ids := append(append([]int64{}, rwp.ParticipantIDs...), removed...)
	byDomain, all, err := s.remotePeers(ctx, ids)
	if err != nil || len(byDomain) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, rwp.ID)
	if err != nil {
		return
	}
	current := idSet(rwp.ParticipantIDs)
	body := ParticipantsUpdate{Room: ref, Participants: s.profiles(all, current)}
	removedSet := idSet(removed)
	for _, u := range all {
		if _, ok := removedSet[u.ID]; ok {
			body.Removed = append(body.Removed, s.FIDOf(u))
		}
	}
	for domain := range byDomain {
		s.send(domain, http.MethodPut, "/rooms/participants", body)
	}
}

func (s *Service) AfterRoomUpdated(ctx context.Context, rwp *rooms.RoomWithParticipants) {
	byDomain, _, err := s.remotePeers(ctx, rwp.ParticipantIDs)
	if err != nil || len(byDomain) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, rwp.ID)
	if err != nil {
		return
	}
	name := rwp.Name
	e2ee := rwp.E2EEEnabled == nil || *rwp.E2EEEnabled
	body := RoomPatch{Room: ref, Name: &name, E2EEEnabled: &e2ee}
	for domain := range byDomain {
		s.send(domain, http.MethodPatch, "/rooms", body)
	}
}

func (s *Service) AfterTyping(ctx context.Context, roomID, userID int64) {
	m, err := s.repo.GetRoomMapping(ctx, roomID)
	if err != nil || m == nil {
		return // never federated: nothing to tell anyone
	}
	ids, err := s.roomRepo.GetParticipants(ctx, roomID)
	if err != nil {
		return
	}
	targets, origin := s.relayTargets(ctx, roomID, ids)
	if origin != "" {
		// A channel of a space hosted elsewhere: the origin passes it on to the others.
		targets = map[string]struct{}{origin: {}}
	}
	if len(targets) == 0 {
		return
	}
	body := TypingEvent{Room: RoomRef{OriginDomain: m.OriginDomain, OriginRoomID: id.Format(m.OriginRoomID)}, User: s.LocalFID(userID)}
	s.fanOut(ctx, targets, http.MethodPost, "/rooms/typing", body)
}

// messageEvent is the wire form of a stored message (relays, and the origin's answers to
// a mirror's writes and history reads). Reactions are attached when given.
func (s *Service) messageEvent(ctx context.Context, ref RoomRef, m *messages.Message, reactions []messages.ReactionSummary) MessageEvent {
	ev := MessageEvent{
		Room:            ref,
		Message:         s.messageRef(ctx, m.RoomID, m.ID),
		Ciphertext:      m.Ciphertext,
		Plaintext:       m.Plaintext,
		MentionEveryone: m.MentionEveryone,
		Attachments:     m.Attachments(),
		SystemType:      m.SystemType,
		SystemPayload:   m.SystemPayload,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
		Reactions:       reactions,
	}
	if m.SenderDeviceID != 0 {
		ev.SenderDeviceID = id.Format(m.SenderDeviceID)
	}
	if m.SenderID != 0 {
		if sender, _ := s.users.GetByID(ctx, m.SenderID); sender != nil {
			p := s.ProfileOf(sender)
			ev.Sender = &p
		}
	}
	if m.ReplyToID != nil {
		r := s.messageRef(ctx, m.RoomID, *m.ReplyToID)
		ev.ReplyTo = &r
	}
	if len(m.Mentions) > 0 {
		if users, err := s.users.GetByIDs(ctx, m.Mentions); err == nil {
			for _, u := range users {
				if u != nil {
					ev.Mentions = append(ev.Mentions, s.FIDOf(u))
				}
			}
		}
	}
	for _, rid := range m.MentionRoles {
		ev.MentionRoles = append(ev.MentionRoles, id.Format(rid))
	}
	return ev
}

func (s *Service) AfterMessageCreated(ctx context.Context, roomID int64, participants []int64, m *messages.Message) {
	targets, _ := s.relayTargets(ctx, roomID, participants)
	if len(targets) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, roomID)
	if err != nil {
		return
	}
	if m.RoomID == 0 {
		m.RoomID = roomID
	}
	s.fanOut(ctx, targets, http.MethodPost, "/rooms/messages", s.messageEvent(ctx, ref, m, nil))
}

func (s *Service) AfterMessageEdited(ctx context.Context, roomID int64, participants []int64, m *messages.Message) {
	targets, _ := s.relayTargets(ctx, roomID, participants)
	if len(targets) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, roomID)
	if err != nil {
		return
	}
	body := MessageEdit{Room: ref, Message: s.messageRef(ctx, roomID, m.ID), Ciphertext: m.Ciphertext, Plaintext: m.Plaintext}
	s.fanOut(ctx, targets, http.MethodPatch, "/rooms/messages", body)
}

func (s *Service) AfterMessageDeleted(ctx context.Context, roomID int64, participants []int64, msgID int64) {
	targets, _ := s.relayTargets(ctx, roomID, participants)
	if len(targets) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, roomID)
	if err != nil {
		return
	}
	body := MessageDelete{Room: ref, Message: s.messageRef(ctx, roomID, msgID)}
	s.fanOut(ctx, targets, http.MethodPost, "/rooms/messages/delete", body)
}

// AfterProfileUpdated tells every instance that holds a shadow of this user (any peer
// sharing a room or a space with them, or the home of a remote friend) about the new
// profile.
func (s *Service) AfterProfileUpdated(ctx context.Context, u *auth.User) {
	if u == nil || u.IsRemote() {
		return
	}
	rows, err := s.roomRepo.ListByUser(ctx, u.ID)
	if err != nil {
		return
	}
	domains := map[string]struct{}{}
	for _, row := range rows {
		ids, err := s.roomRepo.GetParticipants(ctx, row.RoomID)
		if err != nil {
			continue
		}
		byDomain, _, err := s.remotePeers(ctx, ids)
		if err != nil {
			continue
		}
		for d := range byDomain {
			domains[d] = struct{}{}
		}
	}
	if byDomain, _, err := s.remotePeers(ctx, u.Relationships); err == nil {
		for d := range byDomain {
			domains[d] = struct{}{}
		}
	}
	for d := range s.spaceDomainsForUser(ctx, u) {
		domains[d] = struct{}{}
	}
	if len(domains) == 0 {
		return
	}
	s.fanOut(ctx, domains, http.MethodPost, "/users/update", ProfileUpdate{User: s.ProfileOf(u)})
}

// ---- relationships (relationships.Federator) ------------------------------------------

// relayRelationship tells target's home instance what actor, one of our users, did.
func (s *Service) relayRelationship(action string, actor, target *auth.User) {
	if actor == nil || target == nil || actor.IsRemote() || !target.IsRemote() || !s.fcfg.IsAllowedPeer(target.HomeDomain) {
		return
	}
	body := RelationshipEvent{Action: action, Actor: s.ProfileOf(actor), Target: s.FIDOf(target)}
	s.send(target.HomeDomain, http.MethodPost, "/relationships", body)
	if action == "accept" {
		// A new friendship: their side gets our user's current status right away instead
		// of "offline until the next change".
		s.sendPresence(actor, target.HomeDomain)
	}
}

// ---- reactions (messages.Federator) ---------------------------------------------------

func (s *Service) relayReaction(ctx context.Context, path string, roomID int64, participants []int64, msgID, userID int64, emoji string) {
	targets, _ := s.relayTargets(ctx, roomID, participants)
	if len(targets) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, roomID)
	if err != nil {
		return
	}
	// In a space the reactor may be a member from a third instance; name them by their
	// own identity so every mirror resolves the same shadow.
	user := s.LocalFID(userID)
	if u, _ := s.users.GetByID(ctx, userID); u != nil {
		user = s.FIDOf(u)
	}
	body := ReactionEvent{Room: ref, Message: s.messageRef(ctx, roomID, msgID), User: user, Emoji: emoji}
	s.fanOut(ctx, targets, http.MethodPost, path, body)
}

func (s *Service) AfterReactionAdded(ctx context.Context, roomID int64, participants []int64, msgID, userID int64, emoji string) {
	s.relayReaction(ctx, "/rooms/reactions", roomID, participants, msgID, userID, emoji)
}

func (s *Service) AfterReactionRemoved(ctx context.Context, roomID int64, participants []int64, msgID, userID int64, emoji string) {
	s.relayReaction(ctx, "/rooms/reactions/delete", roomID, participants, msgID, userID, emoji)
}

// ---- presence (stargate.PresenceFederator, users.ProfileFederator) --------------------

func (s *Service) presenceEvent(u *auth.User) PresenceEvent {
	return PresenceEvent{User: s.LocalFID(u.ID), Presence: auth.ToPublicPresence(u.Presence, true)}
}

// AfterPresenceChanged tells the home instance of each remote friend, and every instance
// sharing a space with the user, how the user now appears to others - the same audience
// the local gateway tells.
func (s *Service) AfterPresenceChanged(ctx context.Context, u *auth.User) {
	if u == nil || u.IsRemote() {
		return
	}
	domains := s.spaceDomainsForUser(ctx, u)
	if byDomain, _, err := s.remotePeers(ctx, u.Relationships); err == nil {
		for d := range byDomain {
			domains[d] = struct{}{}
		}
	}
	if len(domains) == 0 {
		return
	}
	s.fanOut(ctx, domains, http.MethodPost, "/users/presence", s.presenceEvent(u))
}

// sendPresence tells one instance how a local user appears right now.
func (s *Service) sendPresence(u *auth.User, domain string) {
	if u == nil || u.IsRemote() || !s.fcfg.IsAllowedPeer(domain) {
		return
	}
	s.send(domain, http.MethodPost, "/users/presence", s.presenceEvent(u))
}

func (s *Service) AfterRelationshipRequested(_ context.Context, actor, target *auth.User) {
	s.relayRelationship("request", actor, target)
}

func (s *Service) AfterRelationshipAccepted(_ context.Context, actor, target *auth.User) {
	s.relayRelationship("accept", actor, target)
}

func (s *Service) AfterRelationshipRemoved(_ context.Context, actor, target *auth.User) {
	s.relayRelationship("remove", actor, target)
}

// ---- key routing (devices.KeyRouter) --------------------------------------------------

func (s *Service) QueryRemote(ctx context.Context, domain string, req map[string][]string) (*devices.QueryKeysOutput, error) {
	out := &devices.QueryKeysOutput{DeviceKeys: map[string]map[string]json.RawMessage{}}
	if _, err := s.client.Do(ctx, domain, http.MethodPost, "/keys/query", KeysQuery{DeviceKeys: req}, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) ClaimRemote(ctx context.Context, domain string, req map[string]map[string]string) (*devices.ClaimKeysOutput, error) {
	out := &devices.ClaimKeysOutput{OneTimeKeys: map[string]map[string]json.RawMessage{}}
	if _, err := s.client.Do(ctx, domain, http.MethodPost, "/keys/claim", KeysClaim{OneTimeKeys: req}, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) SendToDeviceRemote(ctx context.Context, domain string, senderUserID, senderDeviceID int64, eventType string, msgs map[string]map[string]json.RawMessage) error {
	sender, err := s.users.GetByID(ctx, senderUserID)
	if err != nil {
		return err
	}
	if sender == nil {
		return ErrRemoteUserNotFound
	}
	body := ToDevice{
		Sender:         s.ProfileOf(sender),
		SenderDeviceID: id.Format(senderDeviceID),
		EventType:      eventType,
		Messages:       msgs,
	}
	_, err = s.client.Do(ctx, domain, http.MethodPost, "/to_device", body, nil)
	return err
}
