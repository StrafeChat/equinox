package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/scylladb/gocqlx/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/devices"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/safego"
)

// Service is the federation engine: outbound relays (implements rooms.Federator,
// messages.Federator, devices.KeyRouter/FIDResolver) plus the state inbound handlers use.
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
}

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
	users := auth.NewCachedUserRepository(auth.NewUserRepository(session), rdb, cfg)
	roomRepo := rooms.NewRepository(session)
	roomSvc := rooms.NewService(roomRepo, users, rdb, cfg, nil, nil)
	msgRepo := messages.NewRepository(session)
	msgSvc := messages.NewService(msgRepo, roomRepo, users, rdb, cfg, nil)
	devSvc := devices.NewService(devices.NewRepository(session), rdb, cfg)
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
	}
	roomSvc.SetFederationInfo(s)
	devSvc.SetRouter(s)
	logger.Info("federation", "enabled as %s (key %s)", cfg.Federation.Domain, signer.KeyID)
	return s, nil
}

// infoProvider is the read-only slice of the engine (room → global identity) for
// processes that never relay anything, like the gateway's READY builder.
type infoProvider struct {
	repo Repository
}

func NewInfoProvider(session gocqlx.Session) rooms.FederationInfo {
	return &infoProvider{repo: NewRepository(session)}
}

func (p *infoProvider) RoomFederation(ctx context.Context, roomID int64) *rooms.Federation {
	return roomFederation(ctx, p.repo, roomID)
}

func roomFederation(ctx context.Context, repo Repository, roomID int64) *rooms.Federation {
	m, err := repo.GetRoomMapping(ctx, roomID)
	if err != nil || m == nil {
		return nil
	}
	return &rooms.Federation{OriginDomain: m.OriginDomain, OriginID: id.Format(m.OriginRoomID)}
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
		FID:           s.FIDOf(u),
		Username:      u.Username,
		Discriminator: u.Discriminator,
		DisplayName:   u.DisplayName,
		Avatar:        u.Avatar,
		Banner:        u.Banner,
		Bio:           u.Bio,
		AboutMe:       u.AboutMe,
		Bot:           u.Bot,
	}
}

func (s *Service) RoomFederation(ctx context.Context, roomID int64) *rooms.Federation {
	return roomFederation(ctx, s.repo, roomID)
}

// ResolveHandle turns "name#0001@domain" into a local user row: the local user for this
// instance's own domain (or no domain), or an up-to-date shadow row fetched from the
// remote instance.
func (s *Service) ResolveHandle(ctx context.Context, raw string) (*auth.User, error) {
	h, err := ParseHandle(raw)
	if err != nil {
		return nil, err
	}
	if h.Domain == "" || h.Domain == s.fcfg.Domain {
		u, err := s.users.GetByUsernameDiscriminator(ctx, h.Username, h.Discriminator)
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
	q.Set("discriminator", strconv.Itoa(h.Discriminator))
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
	if p.Discriminator < 0 || p.Discriminator > 9999 {
		p.Discriminator = 0
	}
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
	} else if u.Username == p.Username && u.Discriminator == p.Discriminator && u.DisplayName == p.DisplayName &&
		u.Avatar == p.Avatar && u.Banner == p.Banner && u.Bio == p.Bio && u.AboutMe == p.AboutMe && u.Bot == p.Bot {
		return u, nil
	}
	u.Username = p.Username
	u.Discriminator = p.Discriminator
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

// send relays one request in the background. Federation is fire-and-forget from the
// sender's point of view: the local write already succeeded, and a peer being slow or
// down must never fail or delay the user's own request.
func (s *Service) send(domain, method, path string, body any) {
	// safego: this runs outside any request, so nothing else would catch a panic - and a
	// peer sending back something unexpected must not take the whole API down.
	safego.Go("federation", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := s.client.Do(ctx, domain, method, path, body, nil); err != nil {
			logger.Err("federation", err, map[string]any{"peer": domain, "method": method, "path": path})
			return
		}
		s.touchPeer(ctx, domain)
	})
}

func (s *Service) touchPeer(ctx context.Context, domain string) {
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
	byDomain, _, err := s.remotePeers(ctx, ids)
	if err != nil || len(byDomain) == 0 {
		return
	}
	body := TypingEvent{Room: RoomRef{OriginDomain: m.OriginDomain, OriginRoomID: id.Format(m.OriginRoomID)}, User: s.LocalFID(userID)}
	for domain := range byDomain {
		s.send(domain, http.MethodPost, "/rooms/typing", body)
	}
}

func (s *Service) AfterMessageCreated(ctx context.Context, roomID int64, participants []int64, m *messages.Message) {
	byDomain, all, err := s.remotePeers(ctx, participants)
	if err != nil || len(byDomain) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, roomID)
	if err != nil {
		return
	}
	ev := MessageEvent{
		Room:            ref,
		Message:         MessageRef{OriginDomain: s.fcfg.Domain, OriginMessageID: id.Format(m.ID)},
		Ciphertext:      m.Ciphertext,
		Plaintext:       m.Plaintext,
		MentionEveryone: m.MentionEveryone,
		Attachments:     m.Attachments(),
		SystemType:      m.SystemType,
		SystemPayload:   m.SystemPayload,
		CreatedAt:       m.CreatedAt,
	}
	if m.SenderDeviceID != 0 {
		ev.SenderDeviceID = id.Format(m.SenderDeviceID)
	}
	byID := map[int64]*auth.User{}
	for _, u := range all {
		byID[u.ID] = u
	}
	if m.SenderID != 0 {
		sender := byID[m.SenderID]
		if sender == nil {
			sender, _ = s.users.GetByID(ctx, m.SenderID)
		}
		if sender != nil {
			p := s.ProfileOf(sender)
			ev.Sender = &p
		}
	}
	if m.ReplyToID != nil {
		r := s.messageRef(ctx, roomID, *m.ReplyToID)
		ev.ReplyTo = &r
	}
	for _, uid := range m.Mentions {
		if u := byID[uid]; u != nil {
			ev.Mentions = append(ev.Mentions, s.FIDOf(u))
		}
	}
	for domain := range byDomain {
		s.send(domain, http.MethodPost, "/rooms/messages", ev)
	}
}

func (s *Service) AfterMessageEdited(ctx context.Context, roomID int64, participants []int64, m *messages.Message) {
	byDomain, _, err := s.remotePeers(ctx, participants)
	if err != nil || len(byDomain) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, roomID)
	if err != nil {
		return
	}
	body := MessageEdit{Room: ref, Message: s.messageRef(ctx, roomID, m.ID), Ciphertext: m.Ciphertext, Plaintext: m.Plaintext}
	for domain := range byDomain {
		s.send(domain, http.MethodPatch, "/rooms/messages", body)
	}
}

func (s *Service) AfterMessageDeleted(ctx context.Context, roomID int64, participants []int64, msgID int64) {
	byDomain, _, err := s.remotePeers(ctx, participants)
	if err != nil || len(byDomain) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, roomID)
	if err != nil {
		return
	}
	body := MessageDelete{Room: ref, Message: s.messageRef(ctx, roomID, msgID)}
	for domain := range byDomain {
		s.send(domain, http.MethodPost, "/rooms/messages/delete", body)
	}
}

// AfterProfileUpdated tells every instance that holds a shadow of this user (any peer
// sharing a room with them) about the new profile.
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
	if len(domains) == 0 {
		return
	}
	body := ProfileUpdate{User: s.ProfileOf(u)}
	for domain := range domains {
		s.send(domain, http.MethodPost, "/users/update", body)
	}
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
