package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/safego"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	ErrDisabled         = errors.New("voice is not configured on this instance")
	ErrNotVoiceRoom     = errors.New("this room has no voice")
	ErrNotInVoice       = errors.New("not connected to a voice room")
	ErrTargetNotInVoice = errors.New("that member is not in a voice room of this space")
	ErrRoomFull         = errors.New("voice room is full")
	ErrMissingPerm      = errors.New("missing permission")
	ErrNoCall           = errors.New("no call in this room")
	ErrInvalidTarget    = errors.New("invalid target room")
	ErrCannotModerateOwner = errors.New("cannot moderate the space owner")
)

// Gateway event names.
const (
	EventVoiceStateUpdate = "VOICE_STATE_UPDATE"
	EventCallCreate       = "CALL_CREATE"
	EventCallUpdate       = "CALL_UPDATE"
	EventCallDelete       = "CALL_DELETE"
)

// System message types posted in PM / group PM rooms.
const (
	SystemCallStarted = "call_started"
	SystemCallEnded   = "call_ended"
)

// reconcileEvery is how often abandoned states (a token that never connected, a
// LiveKit restart, a lost webhook) are swept; reconcileGrace is how long a fresh token
// gets to show up before it counts as abandoned.
const (
	reconcileEvery = 30 * time.Second
	reconcileGrace = 45 * time.Second
)

type Service struct {
	store  *Store
	lk     *LiveKit
	rooms  *rooms.Service
	spaces *spaces.Service
	users  auth.UserRepository
	redis  *redis.Client
	cfg    *config.Config
	// onSystemEvent posts "call started" / "call ended" messages in PM rooms.
	onSystemEvent rooms.OnRoomSystemEvent
}

func NewService(store *Store, lk *LiveKit, roomSvc *rooms.Service, spaceSvc *spaces.Service, users auth.UserRepository, rdb *redis.Client, cfg *config.Config, onSystemEvent rooms.OnRoomSystemEvent) *Service {
	return &Service{store: store, lk: lk, rooms: roomSvc, spaces: spaceSvc, users: users, redis: rdb, cfg: cfg, onSystemEvent: onSystemEvent}
}

func (s *Service) region() string {
	if s.cfg == nil || s.cfg.Stargate.Region == "" {
		return "default"
	}
	return s.cfg.Stargate.Region
}

func newSessionID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// PermsFor is the voice slice of userID's effective permissions in room. PM and group
// PM participants can always connect, speak and share video; nobody moderates there.
func (s *Service) PermsFor(ctx context.Context, userID int64, room *rooms.Room) (Perms, error) {
	if room.SpaceID == nil {
		return Perms{Connect: true, Speak: true, Video: true, VAD: true}, nil
	}
	mask, err := s.spaces.EffectiveChannelPermissions(ctx, userID, *room.SpaceID, room.ID)
	if err != nil {
		return Perms{}, err
	}
	has := func(bit int64) bool { return permissions.Has(mask, bit) }
	return Perms{
		Connect:       has(permissions.PermConnect),
		Speak:         has(permissions.PermSpeak),
		Video:         has(permissions.PermVideo),
		MuteMembers:   has(permissions.PermMuteMembers),
		DeafenMembers: has(permissions.PermDeafenMembers),
		MoveMembers:   has(permissions.PermMoveMembers),
		VAD:           has(permissions.PermUseVAD),
		Priority:      has(permissions.PermPrioritySpeaker),
	}, nil
}

// Join issues a token for room and records the user's voice state there. Joining while
// connected elsewhere leaves that room first (one voice connection per user, like
// Discord); joining the room one is already in re-issues the token (used after a
// reconnect is refused, and after a moderator move).
func (s *Service) Join(ctx context.Context, user *auth.User, roomID int64, in JoinInput) (*JoinResult, error) {
	room, err := s.rooms.GetRoom(ctx, user.ID, roomID)
	if err != nil {
		return nil, err
	}
	if !room.IsVoice() {
		return nil, ErrNotVoiceRoom
	}
	perms, err := s.PermsFor(ctx, user.ID, &room.Room)
	if err != nil {
		return nil, err
	}
	if !perms.Connect {
		return nil, ErrMissingPerm
	}
	prev, err := s.store.GetState(ctx, roomID, user.ID)
	if err != nil {
		return nil, err
	}
	others, err := s.store.RoomStates(ctx, roomID)
	if err != nil {
		return nil, err
	}
	othersCount := 0
	for _, st := range others {
		if st.UserID != user.ID {
			othersCount++
		}
	}
	// The cap counts people, and Move Members gets past it (a moderator can always
	// get into a full room), as on Discord.
	if room.Type == rooms.TypeSpaceVoice && room.UserLimit > 0 && othersCount >= room.UserLimit && !perms.MoveMembers {
		return nil, ErrRoomFull
	}
	if cur, ok, _ := s.store.UserRoom(ctx, user.ID); ok && cur != roomID {
		if err := s.leave(ctx, user.ID, cur, true); err != nil {
			logger.Err("voice", err, map[string]any{"user_id": user.ID, "room_id": cur})
		}
	}

	now := time.Now().UTC()
	st := &State{
		UserID:    user.ID,
		RoomID:    roomID,
		SessionID: newSessionID(),
		SelfMute:  in.SelfMute,
		SelfDeaf:  in.SelfDeaf,
		Suppress:  !perms.Speak,
		Priority:  perms.Priority,
		JoinedAt:  now,
		IssuedAt:  now,
		User:      summaryOf(user),
	}
	if room.SpaceID != nil {
		st.SpaceID = *room.SpaceID
	}
	if prev != nil {
		// Same room again: keep what a moderator did to them and when they arrived, and
		// drop the old LiveKit participant so there is one connection per person.
		st.Mute, st.Deaf, st.JoinedAt = prev.Mute, prev.Deaf, prev.JoinedAt
		st.SelfVideo, st.SelfStream = false, false
		_ = s.lk.RemoveParticipant(ctx, RoomName(roomID), prev.Identity())
	}
	if err := s.store.PutState(ctx, st); err != nil {
		return nil, err
	}
	s.publishState(ctx, room, st, false)

	roomName := RoomName(roomID)
	if err := s.lk.EnsureRoom(ctx, roomName); err != nil {
		_ = s.store.DeleteState(ctx, roomID, user.ID)
		s.publishState(ctx, room, st, true)
		return nil, err
	}
	token, err := s.lk.Token(st.Identity(), displayName(user), roomName, !st.Deaf, sourcesFor(perms.Speak, perms.Video, st.Mute))
	if err != nil {
		return nil, err
	}

	var call *Call
	if room.Type == rooms.TypePM || room.Type == rooms.TypeGroupPM {
		call = s.joinCall(ctx, room, user, othersCount == 0 && prev == nil)
	}
	states, _ := s.store.RoomStates(ctx, roomID)
	bitrate := room.Bitrate
	if bitrate == 0 {
		bitrate = rooms.DefaultVoiceBitrate
	}
	return &JoinResult{
		URL:      s.lk.PublicURL(),
		Token:    token,
		RoomName: roomName,
		State:    st,
		States:   states,
		Bitrate:  bitrate,
		Call:     call.View(),
	}, nil
}

func displayName(u *auth.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// Leave disconnects the user from whatever voice room they are in.
func (s *Service) Leave(ctx context.Context, userID int64) error {
	cur, ok, err := s.store.UserRoom(ctx, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotInVoice
	}
	return s.leave(ctx, userID, cur, true)
}

// leave removes the state, tells everyone, kicks the LiveKit participant when asked
// (not when LiveKit itself reported them gone), and ends a PM call that emptied out.
func (s *Service) leave(ctx context.Context, userID, roomID int64, removeFromLiveKit bool) error {
	st, err := s.store.GetState(ctx, roomID, userID)
	if err != nil {
		return err
	}
	if st == nil {
		return nil
	}
	if err := s.store.DeleteState(ctx, roomID, userID); err != nil {
		return err
	}
	if removeFromLiveKit {
		if err := s.lk.RemoveParticipant(ctx, RoomName(roomID), st.Identity()); err != nil {
			logger.Warn("voice", "remove participant %s from %d: %v", st.Identity(), roomID, err)
		}
	}
	room, err := s.rooms.LoadRoom(ctx, roomID)
	if err != nil {
		// The room is gone (deleted); nobody is left to tell.
		return nil
	}
	s.publishState(ctx, room, st, true)
	if room.Type == rooms.TypePM || room.Type == rooms.TypeGroupPM {
		s.maybeEndCall(ctx, room)
	}
	return nil
}

// UpdateSelf applies the user's own mute / deafen / camera / screen flags.
func (s *Service) UpdateSelf(ctx context.Context, user *auth.User, in SelfInput) (*State, error) {
	roomID, ok, err := s.store.UserRoom(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotInVoice
	}
	st, err := s.store.GetState(ctx, roomID, user.ID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, ErrNotInVoice
	}
	room, err := s.rooms.LoadRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if (in.SelfVideo != nil && *in.SelfVideo) || (in.SelfStream != nil && *in.SelfStream) {
		perms, err := s.PermsFor(ctx, user.ID, &room.Room)
		if err != nil {
			return nil, err
		}
		if !perms.Video {
			return nil, ErrMissingPerm
		}
	}
	changed := false
	apply := func(dst *bool, src *bool) {
		if src != nil && *dst != *src {
			*dst = *src
			changed = true
		}
	}
	apply(&st.SelfMute, in.SelfMute)
	apply(&st.SelfDeaf, in.SelfDeaf)
	apply(&st.SelfVideo, in.SelfVideo)
	apply(&st.SelfStream, in.SelfStream)
	if !changed {
		return st, nil
	}
	if err := s.store.PutState(ctx, st); err != nil {
		return nil, err
	}
	s.publishState(ctx, room, st, false)
	return st, nil
}

// RoomStates lists the voice states in a room the user can access.
func (s *Service) RoomStates(ctx context.Context, userID, roomID int64) ([]State, *Call, error) {
	room, err := s.rooms.GetRoom(ctx, userID, roomID)
	if err != nil {
		return nil, nil, err
	}
	if !room.IsVoice() {
		return nil, nil, ErrNotVoiceRoom
	}
	states, err := s.store.RoomStates(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	call, _ := s.store.GetCall(ctx, roomID)
	return states, call, nil
}

// ---- moderation -----------------------------------------------------------------------

// Moderate server-mutes / deafens a member, or moves them to another voice room. Each
// action needs its own permission, evaluated in the room the target is in, and is
// written to the space's audit log.
func (s *Service) Moderate(ctx context.Context, actor *auth.User, spaceID, targetID int64, in ModerateInput) error {
	if in.Mute == nil && in.Deaf == nil && in.RoomID == nil {
		return spaces.ErrNothingToPatch
	}
	st, room, err := s.targetInSpace(ctx, spaceID, targetID)
	if err != nil {
		return err
	}
	if err := s.canModerate(ctx, actor.ID, spaceID, targetID); err != nil {
		return err
	}
	actorPerms, err := s.PermsFor(ctx, actor.ID, &room.Room)
	if err != nil {
		return err
	}
	targetPerms, err := s.PermsFor(ctx, targetID, &room.Room)
	if err != nil {
		return err
	}
	roomName := RoomName(room.ID)
	if in.Mute != nil && *in.Mute != st.Mute {
		if !actorPerms.MuteMembers {
			return ErrMissingPerm
		}
		st.Mute = *in.Mute
		if err := s.store.PutState(ctx, st); err != nil {
			return err
		}
		if err := s.lk.SetPermission(ctx, roomName, st.Identity(), !st.Deaf, sourcesFor(targetPerms.Speak, targetPerms.Video, st.Mute)); err != nil {
			logger.Warn("voice", "set permission for %s: %v", st.Identity(), err)
		}
		if st.Mute {
			if err := s.lk.MuteSource(ctx, roomName, st.Identity(), livekit.TrackSource_MICROPHONE); err != nil {
				logger.Warn("voice", "server mute %s: %v", st.Identity(), err)
			}
		}
		s.publishState(ctx, room, st, false)
		s.spaces.RecordAudit(ctx, spaceID, actor.ID, spaces.AuditMemberVoiceMute, id.Format(targetID), map[string]spaces.AuditChange{
			"mute": {Old: !st.Mute, New: st.Mute},
		}, "")
	}
	if in.Deaf != nil && *in.Deaf != st.Deaf {
		if !actorPerms.DeafenMembers {
			return ErrMissingPerm
		}
		st.Deaf = *in.Deaf
		if err := s.store.PutState(ctx, st); err != nil {
			return err
		}
		if err := s.lk.SetPermission(ctx, roomName, st.Identity(), !st.Deaf, sourcesFor(targetPerms.Speak, targetPerms.Video, st.Mute)); err != nil {
			logger.Warn("voice", "set permission for %s: %v", st.Identity(), err)
		}
		s.publishState(ctx, room, st, false)
		s.spaces.RecordAudit(ctx, spaceID, actor.ID, spaces.AuditMemberVoiceDeafen, id.Format(targetID), map[string]spaces.AuditChange{
			"deaf": {Old: !st.Deaf, New: st.Deaf},
		}, "")
	}
	if in.RoomID != nil {
		if !actorPerms.MoveMembers {
			return ErrMissingPerm
		}
		destID, err := id.Parse(strings.TrimSpace(*in.RoomID))
		if err != nil {
			return ErrInvalidTarget
		}
		if destID == room.ID {
			return nil
		}
		dest, err := s.rooms.LoadRoom(ctx, destID)
		if err != nil || dest.Type != rooms.TypeSpaceVoice || dest.SpaceID == nil || *dest.SpaceID != spaceID {
			return ErrInvalidTarget
		}
		if err := s.move(ctx, st, room, dest); err != nil {
			return err
		}
		s.spaces.RecordAudit(ctx, spaceID, actor.ID, spaces.AuditMemberVoiceMove, id.Format(targetID), map[string]spaces.AuditChange{
			"room_id": {Old: id.Format(room.ID), New: id.Format(dest.ID)},
		}, "")
	}
	return nil
}

// move re-homes a state into dest. The old room's "left" event carries moved_to, which
// is the client's cue to rejoin there (it asks for a fresh token like any join); the
// old LiveKit participant is dropped so audio stops at once. The new state is written
// before anything is published, so a client that rejoins immediately finds it.
func (s *Service) move(ctx context.Context, st *State, from, dest *rooms.RoomWithParticipants) error {
	old := *st
	st.RoomID = dest.ID
	st.Connected = false
	st.SelfVideo, st.SelfStream = false, false
	st.IssuedAt = time.Now().UTC()
	if perms, err := s.PermsFor(ctx, st.UserID, &dest.Room); err == nil {
		st.Suppress = !perms.Speak
		st.Priority = perms.Priority
	}
	if err := s.store.PutState(ctx, st); err != nil {
		return err
	}
	// DeleteState only clears the user pointer when it still names the old room; it
	// now names dest, so this just drops the old room's entry.
	if err := s.store.DeleteState(ctx, from.ID, st.UserID); err != nil {
		return err
	}
	s.publishStateExtra(ctx, from, &old, true, map[string]interface{}{"moved_to": id.Format(dest.ID)})
	_ = s.lk.RemoveParticipant(ctx, RoomName(from.ID), old.Identity())
	s.publishState(ctx, dest, st, false)
	return nil
}

// Disconnect kicks a member out of voice. Needs Move Members in their room.
func (s *Service) Disconnect(ctx context.Context, actor *auth.User, spaceID, targetID int64) error {
	st, room, err := s.targetInSpace(ctx, spaceID, targetID)
	if err != nil {
		return err
	}
	if err := s.canModerate(ctx, actor.ID, spaceID, targetID); err != nil {
		return err
	}
	perms, err := s.PermsFor(ctx, actor.ID, &room.Room)
	if err != nil {
		return err
	}
	if !perms.MoveMembers {
		return ErrMissingPerm
	}
	if err := s.leave(ctx, st.UserID, room.ID, true); err != nil {
		return err
	}
	s.spaces.RecordAudit(ctx, spaceID, actor.ID, spaces.AuditMemberVoiceDisconnect, id.Format(targetID), nil, "")
	return nil
}

// targetInSpace finds the target's voice state and checks it is in a voice room of the
// space being moderated.
func (s *Service) targetInSpace(ctx context.Context, spaceID, targetID int64) (*State, *rooms.RoomWithParticipants, error) {
	roomID, ok, err := s.store.UserRoom(ctx, targetID)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, ErrTargetNotInVoice
	}
	st, err := s.store.GetState(ctx, roomID, targetID)
	if err != nil {
		return nil, nil, err
	}
	if st == nil || st.SpaceID != spaceID {
		return nil, nil, ErrTargetNotInVoice
	}
	room, err := s.rooms.LoadRoom(ctx, roomID)
	if err != nil {
		return nil, nil, ErrTargetNotInVoice
	}
	return st, room, nil
}

// canModerate: the owner may act on anyone; nobody else may act on the owner.
func (s *Service) canModerate(ctx context.Context, actorID, spaceID, targetID int64) error {
	sp, err := s.spaces.GetSpace(ctx, spaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return spaces.ErrSpaceNotFound
	}
	if targetID == sp.OwnerID && actorID != sp.OwnerID {
		return ErrCannotModerateOwner
	}
	return nil
}

// ---- PM calls --------------------------------------------------------------------------

// joinCall records the caller in the room's call: starting one (and ringing everyone
// else) when the room was empty, otherwise just answering.
func (s *Service) joinCall(ctx context.Context, room *rooms.RoomWithParticipants, user *auth.User, fresh bool) *Call {
	call, _ := s.store.GetCall(ctx, room.ID)
	if call == nil || fresh {
		if call != nil {
			// A stale call object with nobody in the room: end it quietly first.
			_ = s.store.DeleteCall(ctx, room.ID)
		}
		call = &Call{RoomID: room.ID, StartedBy: user.ID, StartedAt: time.Now().UTC()}
		for _, pid := range room.ParticipantIDs {
			if pid != user.ID {
				call.Ringing = append(call.Ringing, pid)
			}
		}
		if err := s.store.PutCall(ctx, call); err != nil {
			logger.Err("voice", err, map[string]any{"room_id": room.ID})
			return nil
		}
		data := call.toMap()
		data["starter"] = summaryOf(user)
		stargate.PublishToUsers(ctx, s.redis, room.ParticipantIDs, EventCallCreate, data, s.region())
		s.systemMessage(ctx, room, SystemCallStarted, map[string]interface{}{"actor_id": id.Format(user.ID)})
		return call
	}
	if call.stopRinging(user.ID) {
		if err := s.store.PutCall(ctx, call); err == nil {
			stargate.PublishToUsers(ctx, s.redis, room.ParticipantIDs, EventCallUpdate, call.toMap(), s.region())
		}
	}
	return call
}

// maybeEndCall ends the room's call once nobody is connected.
func (s *Service) maybeEndCall(ctx context.Context, room *rooms.RoomWithParticipants) {
	states, err := s.store.RoomStates(ctx, room.ID)
	if err != nil || len(states) > 0 {
		return
	}
	call, _ := s.store.GetCall(ctx, room.ID)
	if call == nil {
		return
	}
	_ = s.store.DeleteCall(ctx, room.ID)
	stargate.PublishToUsers(ctx, s.redis, room.ParticipantIDs, EventCallDelete, map[string]interface{}{
		"room_id": id.Format(room.ID),
	}, s.region())
	s.systemMessage(ctx, room, SystemCallEnded, map[string]interface{}{
		"actor_id":         id.Format(call.StartedBy),
		"duration_seconds": int(time.Since(call.StartedAt).Seconds()),
	})
}

// Ring calls (again) the given participants of a group call, or everyone not yet in
// it when no one is named. Only someone in the call can ring.
func (s *Service) Ring(ctx context.Context, user *auth.User, roomID int64, userIDs []int64) (*Call, error) {
	room, err := s.rooms.GetRoom(ctx, user.ID, roomID)
	if err != nil {
		return nil, err
	}
	if room.Type != rooms.TypePM && room.Type != rooms.TypeGroupPM {
		return nil, ErrNotVoiceRoom
	}
	call, err := s.store.GetCall(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if call == nil {
		return nil, ErrNoCall
	}
	if st, _ := s.store.GetState(ctx, roomID, user.ID); st == nil {
		return nil, ErrNotInVoice
	}
	states, _ := s.store.RoomStates(ctx, roomID)
	connected := make(map[int64]struct{}, len(states))
	for _, st := range states {
		connected[st.UserID] = struct{}{}
	}
	member := make(map[int64]struct{}, len(room.ParticipantIDs))
	for _, pid := range room.ParticipantIDs {
		member[pid] = struct{}{}
	}
	targets := userIDs
	if len(targets) == 0 {
		targets = room.ParticipantIDs
	}
	changed := false
	for _, uid := range targets {
		if _, ok := member[uid]; !ok {
			continue
		}
		if _, ok := connected[uid]; ok || uid == user.ID || call.isRinging(uid) {
			continue
		}
		call.Ringing = append(call.Ringing, uid)
		changed = true
	}
	if changed {
		if err := s.store.PutCall(ctx, call); err != nil {
			return nil, err
		}
	}
	// Publish even when nothing changed: a re-ring restarts the ringtone on the
	// callee's devices.
	data := call.toMap()
	data["rerung"] = true
	stargate.PublishToUsers(ctx, s.redis, room.ParticipantIDs, EventCallUpdate, data, s.region())
	return call, nil
}

// Decline stops ringing the user for the room's call (on every device).
func (s *Service) Decline(ctx context.Context, user *auth.User, roomID int64) error {
	room, err := s.rooms.GetRoom(ctx, user.ID, roomID)
	if err != nil {
		return err
	}
	call, err := s.store.GetCall(ctx, roomID)
	if err != nil {
		return err
	}
	if call == nil {
		return ErrNoCall
	}
	if !call.stopRinging(user.ID) {
		return nil
	}
	if err := s.store.PutCall(ctx, call); err != nil {
		return err
	}
	stargate.PublishToUsers(ctx, s.redis, room.ParticipantIDs, EventCallUpdate, call.toMap(), s.region())
	return nil
}

func (s *Service) systemMessage(ctx context.Context, room *rooms.RoomWithParticipants, typ string, payload map[string]interface{}) {
	if s.onSystemEvent == nil {
		return
	}
	raw, _ := json.Marshal(payload)
	if _, err := s.onSystemEvent(ctx, room.ID, room.ParticipantIDs, typ, string(raw)); err != nil {
		logger.Err("voice", err, map[string]any{"room_id": room.ID, "system_type": typ})
	}
}

// ---- events ---------------------------------------------------------------------------

// publishState fans a state out: to the space channel for space rooms (every member
// sees who is in which voice room), to each participant for PMs and group PMs. A leave
// carries left=true with the room it left, so clients can drop it.
func (s *Service) publishState(ctx context.Context, room *rooms.RoomWithParticipants, st *State, left bool) {
	s.publishStateExtra(ctx, room, st, left, nil)
}

func (s *Service) publishStateExtra(ctx context.Context, room *rooms.RoomWithParticipants, st *State, left bool, extra map[string]interface{}) {
	if s.redis == nil {
		return
	}
	data := stateMap(st)
	if left {
		data["left"] = true
	}
	for k, v := range extra {
		data[k] = v
	}
	if room.SpaceID != nil {
		stargate.PublishToSpace(ctx, s.redis, *room.SpaceID, EventVoiceStateUpdate, data, s.region())
		return
	}
	stargate.PublishToUsers(ctx, s.redis, room.ParticipantIDs, EventVoiceStateUpdate, data, s.region())
}

// stateMap is the wire form of a state - the same JSON the struct produces, as a map so
// the publisher can add fields.
func stateMap(st *State) map[string]interface{} {
	raw, _ := json.Marshal(st)
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// ---- LiveKit webhooks -----------------------------------------------------------------

// parseIdentity splits "<user>.<session>".
func parseIdentity(identity string) (int64, string, bool) {
	i := strings.IndexByte(identity, '.')
	if i <= 0 {
		return 0, "", false
	}
	uid, err := id.Parse(identity[:i])
	if err != nil {
		return 0, "", false
	}
	return uid, identity[i+1:], true
}

func roomIDFromName(name string) (int64, bool) {
	if !strings.HasPrefix(name, "strafe_") {
		return 0, false
	}
	rid, err := id.Parse(strings.TrimPrefix(name, "strafe_"))
	return rid, err == nil
}

// HandleWebhook applies what LiveKit reports: a participant that connected (state
// confirmed), one that left (state removed), a room that ended (all states removed),
// and camera / screen tracks coming and going (the truthful source for the video and
// stream flags, whatever the client claimed).
func (s *Service) HandleWebhook(ctx context.Context, ev *livekit.WebhookEvent) {
	if ev == nil || ev.Room == nil {
		return
	}
	roomID, ok := roomIDFromName(ev.Room.Name)
	if !ok {
		return
	}
	switch ev.Event {
	case webhook.EventRoomFinished:
		states, err := s.store.ClearRoom(ctx, roomID)
		if err != nil || len(states) == 0 {
			return
		}
		room, err := s.rooms.LoadRoom(ctx, roomID)
		if err != nil {
			return
		}
		for i := range states {
			s.publishState(ctx, room, &states[i], true)
		}
		if room.Type == rooms.TypePM || room.Type == rooms.TypeGroupPM {
			s.maybeEndCall(ctx, room)
		}
	case webhook.EventParticipantJoined, webhook.EventParticipantLeft, webhook.EventTrackPublished, webhook.EventTrackUnpublished:
		if ev.Participant == nil {
			return
		}
		uid, session, ok := parseIdentity(ev.Participant.Identity)
		if !ok {
			return
		}
		st, err := s.store.GetState(ctx, roomID, uid)
		if err != nil || st == nil || st.SessionID != session {
			return // an older connection of theirs; nothing to do
		}
		switch ev.Event {
		case webhook.EventParticipantLeft:
			if err := s.leave(ctx, uid, roomID, false); err != nil {
				logger.Err("voice", err, map[string]any{"room_id": roomID, "user_id": uid})
			}
			return
		case webhook.EventParticipantJoined:
			if st.Connected {
				return
			}
			st.Connected = true
		default:
			if ev.Track == nil {
				return
			}
			on := ev.Event == webhook.EventTrackPublished
			switch ev.Track.Source {
			case livekit.TrackSource_CAMERA:
				if st.SelfVideo == on {
					return
				}
				st.SelfVideo = on
			case livekit.TrackSource_SCREEN_SHARE:
				if st.SelfStream == on {
					return
				}
				st.SelfStream = on
			default:
				return
			}
		}
		if err := s.store.PutState(ctx, st); err != nil {
			return
		}
		if room, err := s.rooms.LoadRoom(ctx, roomID); err == nil {
			s.publishState(ctx, room, st, false)
		}
	}
}

// ---- reconciliation -------------------------------------------------------------------

// RunReconciler sweeps states LiveKit no longer backs: a token that was issued but
// never used, a participant whose leave webhook was lost, a LiveKit restart. Runs
// until ctx ends; safe to run in every API replica.
func (s *Service) RunReconciler(ctx context.Context) {
	safego.Go("voice-reconcile", func() {
		t := time.NewTicker(reconcileEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reconcile(ctx)
			}
		}
	})
}

func (s *Service) reconcile(ctx context.Context) {
	roomIDs, err := s.store.ActiveRooms(ctx)
	if err != nil {
		return
	}
	for _, roomID := range roomIDs {
		states, err := s.store.RoomStates(ctx, roomID)
		if err != nil {
			continue
		}
		if len(states) == 0 {
			_ = s.redis.SRem(ctx, s.store.roomsKey(), id.Format(roomID)).Err()
			continue
		}
		present, err := s.lk.ListIdentities(ctx, RoomName(roomID))
		if err != nil {
			logger.Warn("voice", "reconcile: list participants for %d: %v", roomID, err)
			continue
		}
		for i := range states {
			st := &states[i]
			if _, ok := present[st.Identity()]; ok {
				continue
			}
			if time.Since(st.IssuedAt) < reconcileGrace {
				continue
			}
			logger.Info("voice", "reconcile: dropping stale state user=%d room=%d", st.UserID, roomID)
			if err := s.leave(ctx, st.UserID, roomID, false); err != nil {
				logger.Err("voice", err, map[string]any{"room_id": roomID, "user_id": st.UserID})
			}
		}
	}
}
