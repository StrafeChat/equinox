package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// A call in a federated PM, group or space room runs on the LiveKit of the room's origin
// instance. The origin mints every token, owns every voice state and the call object, and
// relays each change to the other instances, which mirror it for their own clients. A user
// on another instance joins by asking the origin through their own instance (joinViaOrigin),
// connects to the origin's LiveKit, and gets E2EE media keys over the federated to-device
// channel like any other Olm traffic. Participant identities in such rooms are
// "<federated id>.<session>" so every instance's clients can tell who a participant is.

// Federator is what voice needs from the federation engine. Nil when federation is off.
type Federator interface {
	IsLocalServer(domain string) bool
	FIDOf(u *auth.User) string
	ResolveLocalID(ctx context.Context, fid string) (int64, error)

	// SpaceFederation is non-nil for a space another instance hosts.
	SpaceFederation(ctx context.Context, spaceID int64) *spaces.SpaceFederation

	// Towards the origin of a room this instance does not host. JoinRemoteVoice is
	// synchronous (the client is waiting for its token); the rest are relayed in order.
	JoinRemoteVoice(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User, in JoinInput) (*RemoteJoin, error)
	// LeaveRemoteVoiceSession names the session that left: the join that may overtake this
	// relay has another one, and the origin must not end it.
	LeaveRemoteVoiceSession(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User, sessionID string)
	UpdateRemoteVoiceSelf(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User, in SelfInput)
	RingRemoteCall(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User, targets []int64)
	DeclineRemoteCall(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User)

	// Moderation in a space another instance hosts, done at the origin as actor, with every
	// check made there. Synchronous: the origin's answer is the moderator's answer.
	ModerateRemoteVoice(ctx context.Context, origin string, spaceID int64, actor *auth.User, targetID int64, in ModerateInput) error
	DisconnectRemoteVoice(ctx context.Context, origin string, spaceID int64, actor *auth.User, targetID int64) error

	// RemoteVoiceStates asks the origin who is in these rooms, at most MaxSyncRooms of them.
	// It fails with ErrOriginPredatesSync when the origin has no way to answer.
	RemoteVoiceStates(ctx context.Context, origin string, list []*rooms.RoomWithParticipants) ([]RoomSync, error)

	// From the origin to every other instance with a participant in the room. A move is a
	// left that names where the user went.
	AfterVoiceStateChanged(ctx context.Context, room *rooms.RoomWithParticipants, st *State, left bool)
	AfterVoiceStateMoved(ctx context.Context, room *rooms.RoomWithParticipants, st *State, dest *rooms.RoomWithParticipants)
	AfterCallChanged(ctx context.Context, room *rooms.RoomWithParticipants, event string, call *Call, starter *auth.User, rerung bool)
}

// RemoteJoin is the origin's answer to a join, already in this instance's ids.
type RemoteJoin struct {
	Result *JoinResult
	// Call is the running call in Redis form for the mirror (Result carries the view).
	Call *Call
}

// OriginError carries the status and message the hosting instance answered with, so a
// "room full" from there reaches the client exactly as one from here would.
type OriginError struct {
	Status  int
	Message string
}

func (e *OriginError) Error() string { return e.Message }

// ErrOriginUnavailable: the instance hosting the call did not answer.
var ErrOriginUnavailable = errors.New("the instance hosting this call could not be reached")

// SetFederator wires calls across instances.
func (s *Service) SetFederator(f Federator) { s.fed = f }

// remoteOrigin names the instance hosting the room's call when it is not this one.
func (s *Service) remoteOrigin(room *rooms.RoomWithParticipants) string {
	if s.fed == nil || room == nil || room.Federation == nil || s.fed.IsLocalServer(room.Federation.OriginDomain) {
		return ""
	}
	return room.Federation.OriginDomain
}

// spaceOrigin names the instance hosting the space when it is not this one.
func (s *Service) spaceOrigin(ctx context.Context, spaceID int64) string {
	if s.fed == nil {
		return ""
	}
	if f := s.fed.SpaceFederation(ctx, spaceID); f != nil {
		return f.OriginDomain
	}
	return ""
}

// hosting reports whether this instance runs the room's call and must tell the others.
func (s *Service) hosting(room *rooms.RoomWithParticipants) bool {
	return s.fed != nil && room != nil && room.Federation != nil && s.fed.IsLocalServer(room.Federation.OriginDomain)
}

// identityFor is the LiveKit participant identity for a join: the federated id in a
// federated room, the plain local id otherwise.
func (s *Service) identityFor(room *rooms.RoomWithParticipants, user *auth.User, sessionID string) string {
	if s.fed != nil && room.Federation != nil {
		return s.fed.FIDOf(user) + "." + sessionID
	}
	return id.Format(user.ID) + "." + sessionID
}

// userOfIdentity is parseIdentity for both identity forms. A federated identity has
// dots inside the id (the domain), so it splits at the last one and resolves the id to
// the local row.
func (s *Service) userOfIdentity(ctx context.Context, identity string) (int64, string, bool) {
	if !strings.HasPrefix(identity, "@") {
		return parseIdentity(identity)
	}
	i := strings.LastIndexByte(identity, '.')
	if i <= 0 || s.fed == nil {
		return 0, "", false
	}
	uid, err := s.fed.ResolveLocalID(ctx, identity[:i])
	if err != nil {
		return 0, "", false
	}
	return uid, identity[i+1:], true
}

// joinViaOrigin joins a call another instance hosts: the origin mints the token and
// records the state; this instance mirrors what it answered so READY, the states
// endpoint and the gateway show the call here too, and the origin keeps the mirror
// current from then on. The answer is a point in time, so it is applied like any other
// answer from the origin: a relay that landed meanwhile stands. The caller's own state is
// the one thing written outright, and removals are left to reconcile.
func (s *Service) joinViaOrigin(ctx context.Context, user *auth.User, room *rooms.RoomWithParticipants, origin string, in JoinInput) (*JoinResult, error) {
	if cur, ok, _ := s.store.UserRoom(ctx, user.ID); ok && cur != room.ID {
		if err := s.leave(ctx, user.ID, cur, true); err != nil {
			logger.Err("voice", err, map[string]any{"user_id": user.ID, "room_id": cur})
		}
	}
	before, _ := s.store.RoomStates(ctx, room.ID)
	asked := time.Now()
	rj, err := s.fed.JoinRemoteVoice(ctx, origin, room, user, in)
	if err != nil {
		return nil, err
	}
	res := rj.Result
	others := make([]State, 0, len(res.States))
	for i := range res.States {
		if res.States[i].UserID != user.ID {
			others = append(others, res.States[i])
		}
	}
	s.applyOriginAnswer(ctx, room, originAnswer{states: others, asked: asked, before: userIDsOf(before)})
	if err := s.store.PutState(ctx, res.State); err != nil {
		return nil, err
	}
	if rj.Call != nil {
		_ = s.store.PutCall(ctx, rj.Call)
	}
	s.publishState(ctx, room, res.State, false)
	return res, nil
}

// LeaveFederated drops a remote user's state from a call this instance hosts. sessionID is
// the session that left; an older peer names none. A join that overtook the leave has made
// a new session, and that one stays.
func (s *Service) LeaveFederated(ctx context.Context, userID, roomID int64, sessionID string) error {
	if sessionID != "" {
		st, err := s.store.GetState(ctx, roomID, userID)
		if err != nil {
			return err
		}
		if st != nil && st.SessionID != sessionID {
			return nil
		}
	}
	return s.leave(ctx, userID, roomID, true)
}

// HostedStates is everyone in a room this instance hosts and its call, for an instance
// mirroring it. The caller has checked that instance may ask.
func (s *Service) HostedStates(ctx context.Context, roomID int64) ([]State, *Call, error) {
	states, err := s.store.RoomStates(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	call, err := s.store.GetCall(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	return states, call, nil
}

// LiveStates is who is in each of the listed rooms, for those with anyone in them.
func (s *Service) LiveStates(ctx context.Context, roomIDs []int64) (map[int64][]State, error) {
	states, _, err := s.store.StatesForRooms(ctx, roomIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]State)
	for i := range states {
		out[states[i].RoomID] = append(out[states[i].RoomID], states[i])
	}
	return out, nil
}

// ApplyRemoteVoiceState mirrors a state change the origin relayed. movedTo is this
// instance's id for the room a moderator moved the user to, when it knows one.
func (s *Service) ApplyRemoteVoiceState(ctx context.Context, room *rooms.RoomWithParticipants, st *State, left bool, movedTo int64) error {
	if !left {
		return s.mirrorState(ctx, room, st)
	}
	existing, err := s.store.GetState(ctx, room.ID, st.UserID)
	if err != nil || existing == nil {
		return err
	}
	// The relay queue can be overtaken by a join's answer: a left for an older session says
	// nothing about the session that is here now. A move keeps its session.
	if existing.SessionID != st.SessionID {
		return nil
	}
	if err := s.store.DeleteState(ctx, room.ID, st.UserID); err != nil {
		return err
	}
	s.publishLocal(ctx, room, st, true, movedTo)
	return nil
}

// ApplyRemoteCall mirrors a call event the origin relayed.
func (s *Service) ApplyRemoteCall(ctx context.Context, room *rooms.RoomWithParticipants, event string, call *Call, starter *auth.User, rerung bool) error {
	switch event {
	case EventCallDelete:
		if err := s.store.DeleteCall(ctx, room.ID); err != nil {
			return err
		}
		s.publishCallLocal(ctx, room, EventCallDelete, map[string]interface{}{"room_id": id.Format(room.ID)})
		s.pushCall(ctx, room, EventCallDelete, nil, false)
		return nil
	case EventCallCreate, EventCallUpdate:
		if call == nil {
			return errors.New("call event without a call")
		}
		if err := s.store.PutCall(ctx, call); err != nil {
			return err
		}
		// Listed, so that if its CALL_DELETE is lost the sweep still asks about it.
		_ = s.store.TrackRoom(ctx, room.ID)
		s.publishCallLocal(ctx, room, event, callData(call, starter, rerung))
		s.pushCall(ctx, room, event, call, rerung)
		return nil
	}
	return fmt.Errorf("unknown call event %q", event)
}
