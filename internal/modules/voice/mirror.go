package voice

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/safego"
)

// A mirror's voice states are a copy kept by relays, and relays are worth one attempt: a peer
// down at that moment, an origin restarting with a queue in flight or a full queue all lose
// one. A lost "left" would leave a ghost for good, and clients key their media to whoever the
// states list, so the sweep also asks each origin with a live room here who is really there
// (POST /rooms/voice/states, one signed request per origin per sweep) and brings the copy in
// line. Everything an origin says about a room goes through one function, applyOriginAnswer,
// whether it came from this pull, a join's answer or a space snapshot.

const (
	// MaxSyncRooms is the most rooms one question may name.
	MaxSyncRooms = 64

	// originSyncTimeout bounds one question.
	originSyncTimeout = 10 * time.Second
	// syncLockFor keeps the other API replicas from asking in the same sweep.
	syncLockFor = 25 * time.Second
	// originRetryAfter is how long an origin that failed is left alone.
	originRetryAfter = time.Minute
	// originDownFor is how long a failure is remembered without another: three missed
	// retries means nothing here is asking any more.
	originDownFor = 3 * originRetryAfter
	// originSilenceLimit is how long a mirror keeps states it cannot confirm. Past it they
	// go, and the room stays listed so the first answer brings back whoever is still there.
	originSilenceLimit = 5 * time.Minute
	// originLegacyFor is how long an origin with no states endpoint is left alone. What it
	// relays is kept as it always was.
	originLegacyFor = time.Hour
)

// ErrOriginPredatesSync: the origin has no states endpoint. Only the whole request can say
// so, which is why the endpoint is a batch: a route per room could not tell this 404 from
// "that room is gone", and would wipe the states of a live call on an older peer.
var ErrOriginPredatesSync = errors.New("the origin predates voice state sync")

// RoomSync is the origin's word on one room, already in this instance's ids. A room the
// origin's answer leaves out is simply asked about again.
type RoomSync struct {
	RoomID int64
	// Gone: the origin no longer has the room, or no longer lets this instance in it.
	Gone   bool
	States []State
	// Incomplete: a state could not be read here, so being unnamed proves nothing.
	Incomplete bool
	Call       *Call
}

// originAnswer is what an origin said about one room.
type originAnswer struct {
	states []State
	// asked is when the question went out. Anything written here after that is newer than
	// the answer.
	asked time.Time
	// before is who the room held then, which is how a user the answer names but the room
	// has lost is told from one it has not met: a left relay beat the answer. Nil when no
	// one looked.
	before map[int64]bool
	// complete says the answer names everyone, so whoever it leaves out has left.
	complete bool
}

// pullOrigins asks every origin about its rooms, side by side: one that does not answer
// must not delay the others.
func (s *Service) pullOrigins(ctx context.Context, remote map[string][]*rooms.RoomWithParticipants) {
	var wg sync.WaitGroup
	for origin, list := range remote {
		wg.Add(1)
		go func() {
			defer wg.Done()
			safego.Run("voice-sync", func() { s.pullOrigin(ctx, origin, list) })
		}()
	}
	wg.Wait()
}

func (s *Service) pullOrigin(ctx context.Context, origin string, list []*rooms.RoomWithParticipants) {
	if !s.store.claimSync(ctx, origin, syncLockFor) || s.store.syncHeldOff(ctx, origin) {
		return
	}
	for start := 0; start < len(list); start += MaxSyncRooms {
		end := min(start+MaxSyncRooms, len(list))
		if err := s.pullRooms(ctx, origin, list[start:end]); err != nil {
			s.originFailed(ctx, origin, list[start:], err)
			return
		}
	}
	s.store.markOriginUp(ctx, origin)
}

// pullRooms asks once and applies the answer, room by room.
func (s *Service) pullRooms(ctx context.Context, origin string, list []*rooms.RoomWithParticipants) error {
	byID := make(map[int64]*rooms.RoomWithParticipants, len(list))
	before := make(map[int64]map[int64]bool, len(list))
	calls := make(map[int64]*Call, len(list))
	for _, room := range list {
		byID[room.ID] = room
		states, _ := s.store.RoomStates(ctx, room.ID)
		before[room.ID] = userIDsOf(states)
		calls[room.ID], _ = s.store.GetCall(ctx, room.ID)
	}
	asked := time.Now()
	askCtx, cancel := context.WithTimeout(ctx, originSyncTimeout)
	defer cancel()
	answers, err := s.fed.RemoteVoiceStates(askCtx, origin, list)
	if err != nil {
		return err
	}
	for i := range answers {
		a := &answers[i]
		room := byID[a.RoomID]
		if room == nil {
			continue
		}
		if a.Gone {
			s.clearMirror(ctx, room)
			s.endMirrorCall(ctx, room, calls[room.ID])
			continue
		}
		s.applyOriginAnswer(ctx, room, originAnswer{states: a.States, asked: asked, before: before[room.ID], complete: !a.Incomplete})
		if a.Call == nil {
			s.endMirrorCall(ctx, room, calls[room.ID])
		}
		s.untrackIfEmpty(ctx, room.ID)
	}
	return nil
}

// originFailed records an origin not answering. Its states stay until the silence limit: a
// blip must not hang up a live call, and the whole space is read-only here meanwhile anyway.
func (s *Service) originFailed(ctx context.Context, origin string, pending []*rooms.RoomWithParticipants, err error) {
	if errors.Is(err, ErrOriginPredatesSync) {
		if err := s.store.markOriginLegacy(ctx, origin, originLegacyFor); err != nil {
			logger.Warn("voice", "remember %s predates voice sync: %v", origin, err)
		}
		return
	}
	logger.Warn("voice", "voice sync with %s: %v", origin, err)
	now := time.Now()
	since, err := s.store.markOriginDown(ctx, origin, now, originRetryAfter, originDownFor)
	if err != nil {
		logger.Warn("voice", "record %s not answering: %v", origin, err)
		return
	}
	if now.Sub(since) < originSilenceLimit {
		return
	}
	for _, room := range pending {
		s.clearMirror(ctx, room)
		if err := s.store.TrackRoom(ctx, room.ID); err != nil {
			logger.Warn("voice", "keep room %d listed: %v", room.ID, err)
		}
	}
}

// clearMirror drops every state in a mirrored room and tells this instance's clients.
func (s *Service) clearMirror(ctx context.Context, room *rooms.RoomWithParticipants) {
	states, err := s.store.ClearRoom(ctx, room.ID)
	if err != nil {
		logger.Err("voice", err, map[string]any{"room_id": room.ID, "stage": "clear mirror"})
		return
	}
	for i := range states {
		s.publishState(ctx, room, &states[i], true)
	}
}

// endMirrorCall ends a call the origin no longer has: its CALL_DELETE was lost. had is the
// call that was here when the question went out, so one that started since stands.
func (s *Service) endMirrorCall(ctx context.Context, room *rooms.RoomWithParticipants, had *Call) {
	if had == nil {
		return
	}
	cur, err := s.store.GetCall(ctx, room.ID)
	if err != nil || cur == nil || !cur.StartedAt.Equal(had.StartedAt) {
		return
	}
	if err := s.ApplyRemoteCall(ctx, room, EventCallDelete, nil, nil, false); err != nil {
		logger.Err("voice", err, map[string]any{"room_id": room.ID, "stage": "end mirrored call"})
	}
}

// untrackIfEmpty stops asking about a room once the origin has said it is empty, the one
// answer that may do this.
func (s *Service) untrackIfEmpty(ctx context.Context, roomID int64) {
	states, err := s.store.RoomStates(ctx, roomID)
	if err != nil || len(states) > 0 {
		return
	}
	if call, err := s.store.GetCall(ctx, roomID); err != nil || call != nil {
		return
	}
	if err := s.store.UntrackRoom(ctx, roomID); err != nil {
		logger.Warn("voice", "stop listing room %d: %v", roomID, err)
	}
}

// ApplySnapshotStates records who a space snapshot says is in a room, which is how a mirror
// discovers a call already running. asked is when the snapshot was requested. Removal is
// left to the pull: a snapshot names only rooms with someone in them.
func (s *Service) ApplySnapshotStates(ctx context.Context, room *rooms.RoomWithParticipants, states []State, asked time.Time) {
	s.applyOriginAnswer(ctx, room, originAnswer{states: states, asked: asked})
}

// applyOriginAnswer brings a mirrored room in line with what its origin said. It only ever
// touches this instance's copy: it never calls leave(), which would forward a leave naming
// users the origin may host itself.
func (s *Service) applyOriginAnswer(ctx context.Context, room *rooms.RoomWithParticipants, a originAnswer) {
	have, err := s.store.RoomStates(ctx, room.ID)
	if err != nil {
		logger.Err("voice", err, map[string]any{"room_id": room.ID, "stage": "apply origin answer"})
		return
	}
	cur := make(map[int64]*State, len(have))
	for i := range have {
		cur[have[i].UserID] = &have[i]
	}
	if a.complete {
		named := userIDsOf(a.states)
		for _, st := range cur {
			if named[st.UserID] || st.IssuedAt.After(a.asked) {
				continue
			}
			if err := s.store.DeleteState(ctx, room.ID, st.UserID); err != nil {
				logger.Err("voice", err, map[string]any{"room_id": room.ID, "user_id": st.UserID})
				continue
			}
			s.publishState(ctx, room, st, true)
		}
	}
	for i := range a.states {
		st := &a.states[i]
		prev, ok := cur[st.UserID]
		switch {
		case ok && prev.IssuedAt.After(a.asked):
			continue // a relay or a join landed while the answer was in flight
		case ok && !stateChanged(prev, st):
			continue
		case !ok && a.before[st.UserID]:
			continue // gone since: a left relay beat the answer
		}
		if err := s.mirrorState(ctx, room, st); err != nil {
			logger.Err("voice", err, map[string]any{"room_id": room.ID, "user_id": st.UserID})
		}
	}
}

// stateChanged reports whether a mirrored state differs from what the origin says in a way
// that matters to a client. Times do not count: on a mirror IssuedAt is local.
func stateChanged(have, want *State) bool {
	return have.SessionID != want.SessionID || have.Identity() != want.Identity() || have.SpaceID != want.SpaceID ||
		have.SelfMute != want.SelfMute || have.SelfDeaf != want.SelfDeaf ||
		have.Mute != want.Mute || have.Deaf != want.Deaf ||
		have.SelfVideo != want.SelfVideo || have.SelfStream != want.SelfStream ||
		have.Suppress != want.Suppress || have.Priority != want.Priority || have.Connected != want.Connected
}

// mirrorState writes a state an origin reported and tells this instance's clients.
//
// A user is in one voice room, and a relay can name a room when this instance still has them
// in another. The user pointer is right about this instance's own users, who join and leave
// through here: a relay naming another room is a late one, and writing it would repoint them
// away from the call they are in (the next pull settles a left that was truly lost). About
// anyone else the relay is the news, but only from the origin of the room they were in: that
// copy goes, unless it is the newer of the two. Another origin's word does not end a state it
// does not own, and nothing ends one in a room hosted here, which answers to LiveKit; there the
// copy is written and the pointer stays, so moderation still finds them, until their state in
// that room ends on its own.
func (s *Service) mirrorState(ctx context.Context, room *rooms.RoomWithParticipants, st *State) error {
	cur, ok, err := s.store.UserRoom(ctx, st.UserID)
	if err != nil {
		return err
	}
	point := true
	if ok && cur != room.ID {
		other, err := s.store.GetState(ctx, cur, st.UserID)
		if err != nil {
			return err
		}
		if other != nil {
			if s.homedHere(ctx, st.UserID) {
				return nil
			}
			otherRoom, err := s.rooms.LoadRoom(ctx, cur)
			if err != nil && !errors.Is(err, rooms.ErrRoomNotFound) {
				return err
			}
			switch {
			case otherRoom == nil:
				// Deleted here: nothing is left to tell.
				if err := s.store.DeleteState(ctx, cur, st.UserID); err != nil {
					return err
				}
			case s.remoteOrigin(otherRoom) == "" || s.remoteOrigin(otherRoom) != s.remoteOrigin(room):
				point = false
			case other.JoinedAt.After(st.JoinedAt):
				return nil
			default:
				if err := s.store.DeleteState(ctx, cur, st.UserID); err != nil {
					return err
				}
				s.publishState(ctx, otherRoom, other, true)
			}
		}
	}
	put := s.store.PutState
	if !point {
		put = s.store.PutCopy
	}
	if err := put(ctx, st); err != nil {
		return err
	}
	s.publishState(ctx, room, st, false)
	return nil
}

func (s *Service) homedHere(ctx context.Context, userID int64) bool {
	u, err := s.users.GetByID(ctx, userID)
	return err == nil && u != nil && !u.IsRemote()
}

func userIDsOf(states []State) map[int64]bool {
	out := make(map[int64]bool, len(states))
	for i := range states {
		out[states[i].UserID] = true
	}
	return out
}
