package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/safego"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// SpaceMemberChecker allows rooms to grant access when the user is a space member (for space channels).
type SpaceMemberChecker interface {
	IsMember(ctx context.Context, spaceID, userID int64) (bool, error)
	// EffectiveChannelPermissions resolves the user's permission bits in one space room,
	// honouring role and per-room overrides. Access to an individual space room is gated on
	// PermViewRoom through this, not on space membership alone - otherwise every member
	// would reach a private channel they have no permission to see.
	EffectiveChannelPermissions(ctx context.Context, userID, spaceID, roomID int64) (int64, error)
}

var (
	ErrRoomNotFound        = errors.New("room not found")
	ErrNotParticipant      = errors.New("not a participant of this room")
	ErrNotGroupRoom        = errors.New("not a group room")
	ErrAlreadyInGroup      = errors.New("user already in group")
	ErrMinParticipants     = errors.New("group must have at least 2 participants")
	ErrTooManyParticipants = errors.New("group cannot have more than 50 participants")
	ErrNotCreator          = errors.New("only the group creator can do this")
	ErrCannotRemoveSelf    = errors.New("creator cannot remove themselves")
	ErrUserNotFound        = errors.New("user not found")
	ErrInvalidName         = errors.New("name must be at most 100 characters")
	ErrInvalidNotifyMode   = errors.New("invalid notify mode")
)

const (
	// MaxGroupParticipants caps a group PM. Every message fans out a Megolm key share to
	// each member's devices, so unbounded groups would make sends arbitrarily slow.
	MaxGroupParticipants = 50
	// MaxRoomNameRunes caps a group PM name.
	MaxRoomNameRunes = 100
)

// validName reports whether a (trimmed) room name fits the limit.
func validName(name string) bool {
	return utf8.RuneCountInString(name) <= MaxRoomNameRunes
}

// OnRoomSystemEvent is called when a group room has a system event (member added/removed, room renamed).
// It should create a system message and return its ID so the room's last_message_id can be updated.
// If nil, system messages are not created.
type OnRoomSystemEvent func(ctx context.Context, roomID int64, participantIDs []int64, eventType, payload string) (messageID int64, err error)

// FederationInfo answers "is this room federated, and what is its global identity" -
// read-only, safe for any process (the gateway's READY provider uses it too).
type FederationInfo interface {
	RoomFederation(ctx context.Context, roomID int64) *Federation
}

// Federator receives room lifecycle events so they can be relayed to the other
// instances whose users are in the room. Implemented by internal/federation; nil when
// this instance doesn't federate. Every hook is best-effort and must not block.
type Federator interface {
	FederationInfo
	AfterRoomCreated(ctx context.Context, room *RoomWithParticipants)
	AfterParticipantsChanged(ctx context.Context, room *RoomWithParticipants, removed []int64)
	AfterRoomUpdated(ctx context.Context, room *RoomWithParticipants)
	AfterTyping(ctx context.Context, roomID, userID int64)
}

type Service struct {
	repo          Repository
	user          auth.UserRepository
	redis         *redis.Client
	cfg           *config.Config
	onSystemEvent OnRoomSystemEvent
	spaceChecker  SpaceMemberChecker
	fedInfo       FederationInfo
	federator     Federator
}

func NewService(repo Repository, user auth.UserRepository, redis *redis.Client, cfg *config.Config, onSystemEvent OnRoomSystemEvent, spaceChecker SpaceMemberChecker) *Service {
	return &Service{repo: repo, user: user, redis: redis, cfg: cfg, onSystemEvent: onSystemEvent, spaceChecker: spaceChecker}
}

// SetFederator wires outbound federation (API process).
func (s *Service) SetFederator(f Federator) {
	s.federator = f
	s.fedInfo = f
}

// SetFederationInfo wires read-only federation metadata (gateway process).
func (s *Service) SetFederationInfo(f FederationInfo) {
	s.fedInfo = f
}

// FindLocalUser looks a local user up by username#discriminator.
func (s *Service) FindLocalUser(ctx context.Context, username string, discriminator int) (*auth.User, error) {
	return s.user.GetByUsernameDiscriminator(ctx, username, discriminator)
}

// localDomain is this instance's federation domain ("" when federation is off).
func (s *Service) localDomain() string {
	if s.cfg == nil {
		return ""
	}
	return s.cfg.Federation.Domain
}

// participantOf is the wire form of a user in a room. With federation on, every
// participant carries home_domain/origin_id so clients derive @origin:domain for E2EE.
func participantOf(u *auth.User, localDomain string) Participant {
	p := Participant{
		ID:            id.Format(u.ID),
		Username:      u.Username,
		Discriminator: u.Discriminator,
		DisplayName:   u.DisplayName,
		Avatar:        u.Avatar,
		Banner:        u.Banner,
		Bio:           u.Bio,
		AboutMe:       u.AboutMe,
		PublicFlags:   auth.PublicFlags(u),
		Bot:           u.Bot,
		Presence:      auth.ToPublicPresence(u.Presence, true),
	}
	if localDomain != "" {
		p.HomeDomain = localDomain
		if u.IsRemote() {
			p.HomeDomain = u.HomeDomain
		}
		p.OriginID = id.Format(u.OriginID())
	}
	return p
}

// enrich fills Participants (and the room's federation identity) for display.
func (s *Service) enrich(ctx context.Context, rwp *RoomWithParticipants) {
	if len(rwp.ParticipantIDs) > 0 {
		users, err := s.user.GetByIDs(ctx, rwp.ParticipantIDs)
		if err == nil {
			for i := range rwp.ParticipantIDs {
				if i < len(users) && users[i] != nil {
					rwp.Participants = append(rwp.Participants, participantOf(users[i], s.localDomain()))
				}
			}
		}
	}
	if s.fedInfo != nil && rwp.Federation == nil {
		rwp.Federation = s.fedInfo.RoomFederation(ctx, rwp.ID)
	}
}

// LoadRoom returns a room with enriched participants (used by federation to build
// payloads and local events for rooms it just touched).
func (s *Service) LoadRoom(ctx context.Context, roomID int64) (*RoomWithParticipants, error) {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, ErrRoomNotFound
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return nil, err
	}
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: ids}
	s.enrich(ctx, rwp)
	return rwp, nil
}

// RoomEventPayload is the ROOM_CREATE/ROOM_UPDATE shape clients expect.
func (s *Service) RoomEventPayload(rwp *RoomWithParticipants) map[string]interface{} {
	recipients := make([]string, len(rwp.ParticipantIDs))
	for i, pid := range rwp.ParticipantIDs {
		recipients[i] = id.Format(pid)
	}
	payload := map[string]interface{}{
		"id":           id.Format(rwp.ID),
		"type":         rwp.Type,
		"recipients":   recipients,
		"created_at":   rwp.CreatedAt,
		"participants": rwp.Participants,
	}
	if rwp.Name != "" {
		payload["name"] = rwp.Name
	}
	if rwp.Type == TypeGroupPM || rwp.CreatorID != 0 {
		payload["creator_id"] = id.Format(rwp.CreatorID)
	}
	e2ee := true
	if rwp.E2EEEnabled != nil {
		e2ee = *rwp.E2EEEnabled
	}
	payload["e2ee_enabled"] = e2ee
	if rwp.Federation != nil {
		payload["federation"] = rwp.Federation
	}
	return payload
}

// PublishRoomEvent pushes a room event to the given local users.
func (s *Service) PublishRoomEvent(ctx context.Context, rwp *RoomWithParticipants, eventType string, userIDs []int64) {
	if s.redis == nil || s.cfg == nil {
		return
	}
	payload := s.RoomEventPayload(rwp)
	region := s.stargateRegion()
	for _, uid := range userIDs {
		stargate.PublishToUser(ctx, s.redis, uid, eventType, payload, region)
	}
}

// PublishTypingFrom pushes TYPING_START for a (possibly remote) user without the
// per-user rate limit - the origin instance already applied it.
func (s *Service) PublishTypingFrom(ctx context.Context, roomID, userID int64) {
	if s.redis == nil || s.cfg == nil {
		return
	}
	payload := map[string]interface{}{
		"room_id":   id.Format(roomID),
		"user_id":   id.Format(userID),
		"timestamp": time.Now().Unix(),
	}
	stargate.PublishToSpace(ctx, s.redis, roomID, "TYPING_START", payload, s.cfg.Stargate.Region)
}

// CreateMirror stores a room announced by another instance, with the given local
// participant ids, and notifies them. No system messages: the origin relays its own.
func (s *Service) CreateMirror(ctx context.Context, room *Room, participantIDs []int64, fed *Federation) (*RoomWithParticipants, error) {
	if err := s.repo.Create(ctx, room, participantIDs); err != nil {
		return nil, err
	}
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: participantIDs, Federation: fed}
	s.enrich(ctx, rwp)
	s.PublishRoomEvent(ctx, rwp, "ROOM_CREATE", participantIDs)
	return rwp, nil
}

// ApplyParticipants reconciles a room's member set with `target` (adds/removes as
// needed) and notifies local users. Used for participant changes relayed by peers.
func (s *Service) ApplyParticipants(ctx context.Context, roomID int64, target []int64) (*RoomWithParticipants, error) {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, ErrRoomNotFound
	}
	current, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return nil, err
	}
	want := make(map[int64]struct{}, len(target))
	for _, t := range target {
		want[t] = struct{}{}
	}
	have := make(map[int64]struct{}, len(current))
	for _, c := range current {
		have[c] = struct{}{}
	}
	var added, removed []int64
	for _, t := range target {
		if _, ok := have[t]; !ok {
			if err := s.repo.AddParticipant(ctx, roomID, t); err != nil {
				return nil, err
			}
			added = append(added, t)
		}
	}
	for _, c := range current {
		if _, ok := want[c]; !ok {
			if err := s.repo.RemoveParticipant(ctx, roomID, c); err != nil {
				return nil, err
			}
			removed = append(removed, c)
		}
	}
	rwp, err := s.LoadRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if len(added) > 0 || len(removed) > 0 {
		s.PublishRoomEvent(ctx, rwp, "ROOM_CREATE", added)
		var remaining []int64
		for _, pid := range rwp.ParticipantIDs {
			isNew := false
			for _, a := range added {
				if a == pid {
					isNew = true
					break
				}
			}
			if !isNew {
				remaining = append(remaining, pid)
			}
		}
		s.PublishRoomEvent(ctx, rwp, "ROOM_UPDATE", remaining)
		if s.redis != nil && s.cfg != nil {
			region := s.stargateRegion()
			for _, uid := range removed {
				stargate.PublishToUser(ctx, s.redis, uid, "ROOM_LEAVE", map[string]interface{}{"room_id": id.Format(roomID)}, region)
			}
			if len(removed) > 0 && (room.E2EEEnabled == nil || *room.E2EEEnabled) {
				rotate := map[string]interface{}{"room_id": id.Format(roomID)}
				for _, uid := range rwp.ParticipantIDs {
					stargate.PublishToUser(ctx, s.redis, uid, "SESSION_ROTATE", rotate, region)
				}
			}
		}
	}
	return rwp, nil
}

// ApplyRoomPatch updates name/E2EE for a mirrored room and notifies local users.
func (s *Service) ApplyRoomPatch(ctx context.Context, roomID int64, name *string, e2ee *bool) (*RoomWithParticipants, error) {
	if name != nil {
		if err := s.repo.UpdateRoomName(ctx, roomID, *name); err != nil {
			return nil, err
		}
	}
	if e2ee != nil {
		if err := s.repo.UpdateRoomE2EEEnabled(ctx, roomID, *e2ee); err != nil {
			return nil, err
		}
	}
	rwp, err := s.LoadRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	s.PublishRoomEvent(ctx, rwp, "ROOM_UPDATE", rwp.ParticipantIDs)
	return rwp, nil
}

func (s *Service) fedRoomCreated(ctx context.Context, rwp *RoomWithParticipants) {
	if s.federator != nil {
		s.federator.AfterRoomCreated(ctx, rwp)
		// The hook registers the room's global identity (when it has remote members);
		// reflect it in the response so the creating client keys E2EE by it right away.
		rwp.Federation = s.federator.RoomFederation(ctx, rwp.ID)
	}
}

func (s *Service) fedParticipantsChanged(ctx context.Context, rwp *RoomWithParticipants, removed []int64) {
	if s.federator != nil {
		s.federator.AfterParticipantsChanged(ctx, rwp, removed)
	}
}

func (s *Service) fedRoomUpdated(ctx context.Context, rwp *RoomWithParticipants) {
	if s.federator != nil {
		s.federator.AfterRoomUpdated(ctx, rwp)
	}
}

func (s *Service) stargateRegion() string {
	if s.cfg == nil || s.cfg.Stargate.Region == "" {
		return "default"
	}
	return s.cfg.Stargate.Region
}

// usersExist returns ErrUserNotFound unless every id names a user row. Without this a
// client could create rooms with phantom participants (a typo'd id, or one guessed at).
func (s *Service) usersExist(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	users, err := s.user.GetByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u == nil {
			return ErrUserNotFound
		}
	}
	return nil
}

// CreatePM finds or creates a 1:1 PM room between actor and target.
// When actorID == targetID, creates a "notes" room (one-person PM for personal notes).
// Returns (room, true) when created, (room, false) when existing.
func (s *Service) CreatePM(ctx context.Context, actorID, targetID int64) (*RoomWithParticipants, bool, error) {
	if actorID != targetID {
		if err := s.usersExist(ctx, []int64{targetID}); err != nil {
			return nil, false, err
		}
	}
	existing, err := s.repo.GetPMRoom(ctx, actorID, targetID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		ids, _ := s.repo.GetParticipants(ctx, existing.ID)
		rwp := &RoomWithParticipants{Room: *existing, ParticipantIDs: ids}
		s.enrich(ctx, rwp)
		return rwp, false, nil
	}
	roomID := id.Next()
	room := &Room{
		ID:       roomID,
		Type:     TypePM,
		SpaceID:  nil,
		ParentID: nil,
		Name:     "",
		Topic:    "",
	}
	participantIDs := []int64{actorID, targetID}
	if actorID == targetID {
		participantIDs = []int64{actorID}
	}
	if err := s.repo.Create(ctx, room, participantIDs); err != nil {
		return nil, false, err
	}
	rwp := &RoomWithParticipants{
		Room:           *room,
		ParticipantIDs: participantIDs,
	}
	s.enrich(ctx, rwp)
	// Real-time: notify target user of new PM with full room (skip for notes room)
	if actorID != targetID && s.redis != nil && s.cfg != nil {
		recipients := make([]string, len(participantIDs))
		for i, pid := range participantIDs {
			recipients[i] = id.Format(pid)
		}
		payload := map[string]interface{}{
			"id":           id.Format(room.ID),
			"type":         TypePM,
			"recipients":   recipients,
			"created_at":   room.CreatedAt,
			"participants": rwp.Participants,
		}
		stargate.PublishToUser(ctx, s.redis, targetID, "ROOM_CREATE", payload, s.stargateRegion())
	}
	if actorID != targetID {
		s.fedRoomCreated(ctx, rwp)
	}
	return rwp, true, nil
}

// CreateGroupPM creates a group PM with the given name and participants (actor is always included).
// All participant IDs must be friends of the actor (not enforced here; can be added).
func (s *Service) CreateGroupPM(ctx context.Context, actorID int64, name string, participantIDs []int64) (*RoomWithParticipants, error) {
	seen := map[int64]struct{}{actorID: {}}
	allIDs := []int64{actorID}
	for _, pid := range participantIDs {
		if pid == actorID {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		allIDs = append(allIDs, pid)
	}
	if len(allIDs) < 2 {
		return nil, ErrMinParticipants
	}
	if len(allIDs) > MaxGroupParticipants {
		return nil, ErrTooManyParticipants
	}
	if !validName(name) {
		return nil, ErrInvalidName
	}
	if err := s.usersExist(ctx, allIDs[1:]); err != nil {
		return nil, err
	}
	roomID := id.Next()
	room := &Room{
		ID:        roomID,
		Type:      TypeGroupPM,
		SpaceID:   nil,
		ParentID:  nil,
		Name:      name,
		Topic:     "",
		CreatorID: actorID,
	}
	if err := s.repo.Create(ctx, room, allIDs); err != nil {
		return nil, err
	}
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: allIDs}
	s.enrich(ctx, rwp)
	if s.redis != nil && s.cfg != nil {
		recipients := make([]string, len(allIDs))
		for i, pid := range allIDs {
			recipients[i] = id.Format(pid)
		}
		payload := map[string]interface{}{
			"id":           id.Format(room.ID),
			"type":         TypeGroupPM,
			"recipients":   recipients,
			"name":         room.Name,
			"created_at":   room.CreatedAt,
			"participants": rwp.Participants,
		}
		region := s.stargateRegion()
		for _, uid := range allIDs {
			stargate.PublishToUser(ctx, s.redis, uid, "ROOM_CREATE", payload, region)
		}
	}
	s.fedRoomCreated(ctx, rwp)
	return rwp, nil
}

// AddParticipant adds a user to a group PM. Caller must be a participant; room must be TypeGroupPM.
func (s *Service) AddParticipant(ctx context.Context, actorID, roomID, targetID int64) error {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrRoomNotFound
	}
	if room.Type != TypeGroupPM {
		return ErrNotGroupRoom
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	actorIn := false
	for _, pid := range ids {
		if pid == actorID {
			actorIn = true
			break
		}
	}
	if !actorIn {
		return ErrNotParticipant
	}
	for _, pid := range ids {
		if pid == targetID {
			return ErrAlreadyInGroup
		}
	}
	if len(ids) >= MaxGroupParticipants {
		return ErrTooManyParticipants
	}
	if err := s.usersExist(ctx, []int64{targetID}); err != nil {
		return err
	}
	if err := s.repo.AddParticipant(ctx, roomID, targetID); err != nil {
		return err
	}
	newIDs, _ := s.repo.GetParticipants(ctx, roomID)
	// System message: X added Y
	if s.onSystemEvent != nil {
		payload := `{"actor_id":"` + id.Format(actorID) + `","user_id":"` + id.Format(targetID) + `"}`
		if msgID, err := s.onSystemEvent(ctx, roomID, newIDs, "member_added", payload); err == nil {
			_ = s.repo.UpdateLastMessageID(ctx, roomID, newIDs, msgID)
		}
	}
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: newIDs}
	s.enrich(ctx, rwp)
	if s.redis != nil && s.cfg != nil {
		recipients := make([]string, len(newIDs))
		for i, pid := range newIDs {
			recipients[i] = id.Format(pid)
		}
		payload := map[string]interface{}{
			"id":           id.Format(room.ID),
			"type":         TypeGroupPM,
			"recipients":   recipients,
			"name":         room.Name,
			"created_at":   room.CreatedAt,
			"participants": rwp.Participants,
		}
		region := s.stargateRegion()
		stargate.PublishToUser(ctx, s.redis, targetID, "ROOM_CREATE", payload, region)
		for _, uid := range ids {
			stargate.PublishToUser(ctx, s.redis, uid, "ROOM_UPDATE", payload, region)
		}
	}
	s.fedParticipantsChanged(ctx, rwp, nil)
	return nil
}

// RemoveParticipant removes a user from a group PM.
// - If targetID == actorID: "leave group" — any participant can remove themselves.
// - Otherwise: only the group creator can remove another member; at least 2 participants must remain.
func (s *Service) RemoveParticipant(ctx context.Context, actorID, roomID, targetID int64) error {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrRoomNotFound
	}
	if room.Type != TypeGroupPM {
		return ErrNotGroupRoom
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	actorIn := false
	targetIn := false
	for _, pid := range ids {
		if pid == actorID {
			actorIn = true
		}
		if pid == targetID {
			targetIn = true
		}
	}
	if !actorIn {
		return ErrNotParticipant
	}
	if !targetIn {
		return ErrRoomNotFound
	}
	leaveSelf := targetID == actorID
	if !leaveSelf {
		if room.CreatorID != 0 && room.CreatorID != actorID {
			return ErrNotCreator
		}
		if targetID == room.CreatorID {
			return ErrCannotRemoveSelf
		}
		if len(ids) <= 2 {
			return ErrMinParticipants
		}
	}
	if err := s.repo.RemoveParticipant(ctx, roomID, targetID); err != nil {
		return err
	}
	remaining, _ := s.repo.GetParticipants(ctx, roomID)
	if s.onSystemEvent != nil {
		var eventType, payload string
		if leaveSelf {
			eventType = "member_left"
			payload = `{"user_id":"` + id.Format(targetID) + `"}`
		} else {
			eventType = "member_removed"
			payload = `{"actor_id":"` + id.Format(actorID) + `","user_id":"` + id.Format(targetID) + `"}`
		}
		if msgID, err := s.onSystemEvent(ctx, roomID, remaining, eventType, payload); err == nil {
			_ = s.repo.UpdateLastMessageID(ctx, roomID, remaining, msgID)
		}
	}
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: remaining}
	s.enrich(ctx, rwp)
	if s.redis != nil && s.cfg != nil {
		recipients := make([]string, len(remaining))
		for i, pid := range remaining {
			recipients[i] = id.Format(pid)
		}
		payload := map[string]interface{}{
			"id":           id.Format(room.ID),
			"type":         TypeGroupPM,
			"recipients":   recipients,
			"name":         room.Name,
			"created_at":   room.CreatedAt,
			"participants": rwp.Participants,
		}
		region := s.stargateRegion()
		for _, uid := range remaining {
			stargate.PublishToUser(ctx, s.redis, uid, "ROOM_UPDATE", payload, region)
		}
		stargate.PublishToUser(ctx, s.redis, targetID, "ROOM_LEAVE", map[string]interface{}{"room_id": id.Format(roomID)}, region)
		// A Megolm group session has no cryptographic revocation on its own - anyone who
		// had the session key can still decrypt anything encrypted under it. Removing a
		// member only matters in practice if the remaining members rotate to a fresh
		// outbound session afterward; this tells their clients to do that.
		if room.E2EEEnabled == nil || *room.E2EEEnabled {
			rotatePayload := map[string]interface{}{"room_id": id.Format(roomID)}
			for _, uid := range remaining {
				stargate.PublishToUser(ctx, s.redis, uid, "SESSION_ROTATE", rotatePayload, region)
			}
		}
	}
	s.fedParticipantsChanged(ctx, rwp, []int64{targetID})
	return nil
}

// UpdateRoomName updates a group PM's name. Only the group creator can rename.
func (s *Service) UpdateRoomName(ctx context.Context, actorID, roomID int64, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrRoomNotFound
	}
	if room.Type != TypeGroupPM {
		return ErrNotGroupRoom
	}
	if room.CreatorID != 0 && room.CreatorID != actorID {
		return ErrNotCreator
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	actorIn := false
	for _, pid := range ids {
		if pid == actorID {
			actorIn = true
			break
		}
	}
	if !actorIn {
		return ErrNotParticipant
	}
	oldName := room.Name
	if err := s.repo.UpdateRoomName(ctx, roomID, name); err != nil {
		return err
	}
	room.Name = name
	// System message: X renamed the group to Y
	if s.onSystemEvent != nil {
		payload, _ := json.Marshal(map[string]string{
			"actor_id": id.Format(actorID),
			"old_name": oldName,
			"new_name": name,
		})
		if msgID, err := s.onSystemEvent(ctx, roomID, ids, "room_renamed", string(payload)); err == nil {
			_ = s.repo.UpdateLastMessageID(ctx, roomID, ids, msgID)
		}
	}
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: ids}
	s.enrich(ctx, rwp)
	if s.redis != nil && s.cfg != nil {
		recipients := make([]string, len(ids))
		for i, pid := range ids {
			recipients[i] = id.Format(pid)
		}
		payload := map[string]interface{}{
			"id":           id.Format(room.ID),
			"type":         TypeGroupPM,
			"recipients":   recipients,
			"name":         room.Name,
			"created_at":   room.CreatedAt,
			"participants": rwp.Participants,
		}
		region := s.stargateRegion()
		for _, uid := range ids {
			stargate.PublishToUser(ctx, s.redis, uid, "ROOM_UPDATE", payload, region)
		}
	}
	s.fedRoomUpdated(ctx, rwp)
	return nil
}

// UpdateRoomE2EEEnabled sets whether messages in the group are E2EE. Only the group creator can change.
func (s *Service) UpdateRoomE2EEEnabled(ctx context.Context, actorID, roomID int64, enabled bool) error {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrRoomNotFound
	}
	if room.Type != TypeGroupPM {
		return ErrNotGroupRoom
	}
	if room.CreatorID != 0 && room.CreatorID != actorID {
		return ErrNotCreator
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	actorIn := false
	for _, pid := range ids {
		if pid == actorID {
			actorIn = true
			break
		}
	}
	if !actorIn {
		return ErrNotParticipant
	}
	wasOn := room.E2EEEnabled == nil || *room.E2EEEnabled
	if err := s.repo.UpdateRoomE2EEEnabled(ctx, roomID, enabled); err != nil {
		return err
	}
	room.E2EEEnabled = &enabled
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: ids}
	s.enrich(ctx, rwp)
	// Every participant's client must learn the new setting right away: it decides
	// whether the next send goes out as ciphertext or plaintext, and a client that missed
	// the change would populate the wrong column and be rejected.
	s.PublishRoomEvent(ctx, rwp, "ROOM_UPDATE", ids)
	if enabled && !wasOn && s.redis != nil && s.cfg != nil {
		// Same reasoning as spaces.Service.UpdateRoom: a Megolm session left over from an
		// earlier E2EE stint may still be shared with people who have since left.
		rotate := map[string]interface{}{"room_id": id.Format(roomID)}
		region := s.stargateRegion()
		for _, uid := range ids {
			stargate.PublishToUser(ctx, s.redis, uid, "SESSION_ROTATE", rotate, region)
		}
	}
	s.fedRoomUpdated(ctx, rwp)
	return nil
}

// participantFetchConcurrency bounds the parallel room_participants reads in ListRooms.
const participantFetchConcurrency = 8

// ListRooms returns rooms the user participates in (PMs first, ordered by recency).
//
// The room rows come from one batched read; the per-room participant partitions are
// read concurrently. This used to be two sequential queries per room, which made READY
// (which calls this) grow linearly with the number of conversations a user has.
func (s *Service) ListRooms(ctx context.Context, userID int64) ([]RoomWithParticipants, error) {
	rows, err := s.repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	mentionCounts, _ := s.repo.GetMentionCounts(ctx, userID)
	roomIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		roomIDs = append(roomIDs, row.RoomID)
	}
	roomObjs, err := s.repo.GetByIDs(ctx, roomIDs)
	if err != nil {
		return nil, err
	}
	participants := make([][]int64, len(rows))
	var wg sync.WaitGroup
	sem := make(chan struct{}, participantFetchConcurrency)
	for i := range rows {
		if roomObjs[i] == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go safego.Run("rooms", func() {
			defer wg.Done()
			defer func() { <-sem }()
			participants[i], _ = s.repo.GetParticipants(ctx, rows[i].RoomID)
		})
	}
	wg.Wait()
	out := make([]RoomWithParticipants, 0, len(rows))
	for i, row := range rows {
		room := roomObjs[i]
		if room == nil {
			continue
		}
		rwp := RoomWithParticipants{Room: *room, ParticipantIDs: participants[i]}
		if row.LastMessageID != nil {
			rwp.LastMessageID = row.LastMessageID
		}
		rwp.LastReadMessageID = row.LastReadMessageID
		rwp.MentionCount = row.DisplayMentionCount(mentionCounts[row.RoomID])
		rwp.Muted = row.Muted
		rwp.MutedUntil = row.MutedUntil
		rwp.NotifyMode = row.NotifyMode
		out = append(out, rwp)
	}
	sort.Slice(out, func(i, j int) bool {
		ida, idb := int64(0), int64(0)
		if out[i].LastMessageID != nil {
			ida = *out[i].LastMessageID
		}
		if out[j].LastMessageID != nil {
			idb = *out[j].LastMessageID
		}
		return ida > idb
	})

	// Enrich with participant details for display (PM names, etc.)
	allIDs := make(map[int64]struct{})
	for _, r := range out {
		for _, pid := range r.ParticipantIDs {
			allIDs[pid] = struct{}{}
		}
	}
	ids := make([]int64, 0, len(allIDs))
	for pid := range allIDs {
		ids = append(ids, pid)
	}
	if len(ids) > 0 {
		users, err := s.user.GetByIDs(ctx, ids)
		if err == nil {
			byID := make(map[int64]*auth.User)
			for i := range ids {
				if i < len(users) && users[i] != nil {
					byID[ids[i]] = users[i]
				}
			}
			localDomain := s.localDomain()
			for i := range out {
				for _, pid := range out[i].ParticipantIDs {
					if u := byID[pid]; u != nil {
						out[i].Participants = append(out[i].Participants, participantOf(u, localDomain))
					}
				}
			}
		}
	}
	if s.fedInfo != nil {
		for i := range out {
			out[i].Federation = s.fedInfo.RoomFederation(ctx, out[i].ID)
		}
	}
	return out, nil
}

const typingRateLimitSec = 5

// Typing publishes TYPING_START to the room. No persistence. Rate-limited per user/room.
func (s *Service) Typing(ctx context.Context, userID, roomID int64) error {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrRoomNotFound
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	if !s.canAccessLoaded(ctx, userID, room, ids) {
		return ErrNotParticipant
	}
	if s.redis == nil || s.cfg == nil {
		return nil
	}
	key := "typing:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(roomID, 10)
	// Rate limit: 1 event per 5s per user per room. Silent drop if throttled.
	if set, _ := s.redis.SetNX(ctx, key, "1", typingRateLimitSec*time.Second).Result(); !set {
		return nil
	}
	payload := map[string]interface{}{
		"room_id":   id.Format(roomID),
		"user_id":   id.Format(userID),
		"timestamp": time.Now().Unix(),
	}
	stargate.PublishToSpace(ctx, s.redis, roomID, "TYPING_START", payload, s.cfg.Stargate.Region)
	if s.federator != nil {
		s.federator.AfterTyping(ctx, roomID, userID)
	}
	return nil
}

// canAccessLoaded is the pure access rule shared by every call site: either a
// room participant (PM/group PM), or - for space text/voice rooms - a member
// of the owning space. Takes already-loaded room + participants so callers
// that need that data anyway (GetRoom, Ack, Typing) don't fetch it twice.
func (s *Service) canAccessLoaded(ctx context.Context, userID int64, room *Room, participantIDs []int64) bool {
	if room.SpaceID != nil {
		// A space room is reachable only if the user can actually view it. Space
		// membership is not enough: a private channel denies PermViewRoom to the roles
		// that must not see it, and this is the one gate the realtime gateway shares with
		// REST message access, so the two cannot drift. Fail closed on any error.
		if s.spaceChecker == nil {
			return false
		}
		perms, err := s.spaceChecker.EffectiveChannelPermissions(ctx, userID, *room.SpaceID, room.ID)
		return err == nil && permissions.Has(perms, permissions.PermViewRoom)
	}
	for _, pid := range participantIDs {
		if pid == userID {
			return true
		}
	}
	return false
}

// CanAccess reports whether userID may access roomID. Used by callers (e.g. the
// Stargate channel authorizer) that don't already have the room/participants
// loaded; the REST handlers use canAccessLoaded directly to avoid refetching.
func (s *Service) CanAccess(ctx context.Context, userID, roomID int64) (bool, error) {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil {
		return false, err
	}
	if room == nil {
		return false, ErrRoomNotFound
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return false, err
	}
	return s.canAccessLoaded(ctx, userID, room, ids), nil
}

// GetRoom returns a room by ID if the user is a participant or (for space rooms) a space member.
func (s *Service) GetRoom(ctx context.Context, userID, roomID int64) (*RoomWithParticipants, error) {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return nil, ErrRoomNotFound
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if !s.canAccessLoaded(ctx, userID, room, ids) {
		return nil, ErrNotParticipant
	}
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: ids}
	row, _ := s.repo.GetRoomRow(ctx, userID, roomID)
	rawMentions, _ := s.repo.GetMentionCount(ctx, userID, roomID)
	if row != nil {
		rwp.LastReadMessageID = row.LastReadMessageID
		rwp.Muted = row.Muted
		rwp.MutedUntil = row.MutedUntil
		rwp.NotifyMode = row.NotifyMode
	}
	rwp.MentionCount = row.DisplayMentionCount(rawMentions)
	s.enrich(ctx, rwp)
	return rwp, nil
}

// GetUserRoomRow returns the rooms_by_user row for (userID, roomID), if any.
func (s *Service) GetUserRoomRow(ctx context.Context, userID, roomID int64) (*RoomRow, error) {
	return s.repo.GetRoomRow(ctx, userID, roomID)
}

// UserRoomRows returns every rooms_by_user row of a user keyed by room id in one read.
func (s *Service) UserRoomRows(ctx context.Context, userID int64) (map[int64]*RoomRow, error) {
	rows, err := s.repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*RoomRow, len(rows))
	for i := range rows {
		out[rows[i].RoomID] = &rows[i]
	}
	return out, nil
}

// Ack marks messages up to messageID as read for the user in the room.
// Publishes MESSAGE_ACK to the user's channel (for multi-session sync).
func (s *Service) Ack(ctx context.Context, userID, roomID, messageID int64) error {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrRoomNotFound
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	if !s.canAccessLoaded(ctx, userID, room, ids) {
		return ErrNotParticipant
	}
	// Clamp to the room's actual last message: snowflakes encode time, so an unvalidated
	// ack would let a client permanently suppress unread state by acking an id that
	// doesn't exist yet.
	if room.LastMessageID != nil && messageID > *room.LastMessageID {
		messageID = *room.LastMessageID
	}
	if row, err := s.repo.GetRoomRow(ctx, userID, roomID); err == nil && row != nil && row.LastReadMessageID != nil {
		// ACK cursor is monotonic; ignore stale backwards ACKs.
		if messageID <= *row.LastReadMessageID {
			return nil
		}
	}
	if err := s.repo.UpdateReadState(ctx, userID, roomID, messageID); err != nil {
		return err
	}
	if total, err := s.repo.GetMentionCount(ctx, userID, roomID); err == nil {
		if err := s.repo.SetMentionCountBaseline(ctx, userID, roomID, int64(total)); err != nil {
			// non-fatal: read cursor is already persisted; worst case the mention badge
			// stays stale until the next successful ack clears it.
		}
	}
	if s.redis != nil && s.cfg != nil {
		region := s.cfg.Stargate.Region
		if region == "" {
			region = "default"
		}
		payload := map[string]interface{}{
			"room_id":              id.Format(roomID),
			"message_id":           id.Format(messageID),
			"last_read_message_id": id.Format(messageID),
		}
		stargate.PublishToUser(ctx, s.redis, userID, "MESSAGE_ACK", payload, region)
	}
	return nil
}

// SetRoomNotifySettings updates the caller's own mute/notify-mode override for a room.
// This is purely personal state (not visible to anyone else in the room), so the only
// access check is "can this user see the room at all" - the same rule as Ack. Publishes to
// the user's own channel (not the room) so muting on one device is reflected on every
// other session immediately, matching how MESSAGE_ACK already keeps read state in sync.
func (s *Service) SetRoomNotifySettings(ctx context.Context, userID, roomID int64, muted bool, mutedUntil *time.Time, notifyMode int) error {
	if !ValidNotifyMode(notifyMode) {
		return ErrInvalidNotifyMode
	}
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrRoomNotFound
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	if !s.canAccessLoaded(ctx, userID, room, ids) {
		return ErrNotParticipant
	}
	// A timed mute already in the past is the same as no mute - store it as such rather
	// than persisting a value every future read has to re-decide is expired.
	if mutedUntil != nil && !mutedUntil.After(time.Now().UTC()) {
		mutedUntil = nil
	}
	if err := s.repo.SetRoomNotifySettings(ctx, userID, roomID, muted, mutedUntil, notifyMode); err != nil {
		return err
	}
	if s.redis != nil && s.cfg != nil {
		region := s.cfg.Stargate.Region
		if region == "" {
			region = "default"
		}
		payload := map[string]interface{}{
			"room_id":     id.Format(roomID),
			"muted":       muted,
			"notify_mode": notifyMode,
		}
		if mutedUntil != nil {
			payload["muted_until"] = *mutedUntil
		}
		stargate.PublishToUser(ctx, s.redis, userID, "ROOM_NOTIFY_SETTINGS_UPDATE", payload, region)
	}
	return nil
}

// GetMentionCounts returns every room's mention count for a user (one query).
func (s *Service) GetMentionCounts(ctx context.Context, userID int64) (map[int64]int, error) {
	return s.repo.GetMentionCounts(ctx, userID)
}
