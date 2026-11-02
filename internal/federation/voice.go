package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/voice"
)

// Calls across instances. The room's origin hosts the call on its LiveKit; a user
// elsewhere joins through their own instance, which asks the origin for a token
// (POST /rooms/voice/join, synchronous) and afterwards relays leave / self-flag /
// ring / decline requests to it. The origin pushes every voice state and call change to
// the other instances in the room (POST /rooms/voice/state, /rooms/voice/call), which
// mirror them for their own clients, and those instances ask it who is really there on a
// timer (POST /rooms/voice/states), because a relay is one attempt. See voice/federation.go
// and voice/mirror.go for the service side.

// ---- wire types -------------------------------------------------------------------------

// VoiceJoinRequest: POST /rooms/voice/join - a user on the requesting instance wants a
// token for the call in a room this instance originated.
type VoiceJoinRequest struct {
	Room     RoomRef `json:"room"`
	User     Profile `json:"user"`
	SelfMute bool    `json:"self_mute"`
	SelfDeaf bool    `json:"self_deaf"`
}

// VoiceState is a voice state as instances describe it to each other: the user as a
// profile (so a receiver can create the shadow and draw the tile) and the LiveKit
// identity verbatim, since that is what every client matches participants by.
type VoiceState struct {
	User       Profile   `json:"user"`
	Identity   string    `json:"identity"`
	SessionID  string    `json:"session_id"`
	SelfMute   bool      `json:"self_mute"`
	SelfDeaf   bool      `json:"self_deaf"`
	Mute       bool      `json:"mute"`
	Deaf       bool      `json:"deaf"`
	SelfVideo  bool      `json:"self_video"`
	SelfStream bool      `json:"self_stream"`
	Suppress   bool      `json:"suppress"`
	Priority   bool      `json:"priority_speaker"`
	Connected  bool      `json:"connected"`
	JoinedAt   time.Time `json:"joined_at"`
}

// VoiceCall is a ringing / running call with its users as FIDs.
type VoiceCall struct {
	StartedBy string    `json:"started_by"`
	StartedAt time.Time `json:"started_at"`
	Ringing   []string  `json:"ringing"`
}

// VoiceJoinReply is the origin's answer to a join: where to connect, as whom, and who
// is already there.
type VoiceJoinReply struct {
	URL      string       `json:"url"`
	Token    string       `json:"token"`
	RoomName string       `json:"room_name"`
	Bitrate  int          `json:"bitrate"`
	State    VoiceState   `json:"state"`
	States   []VoiceState `json:"states"`
	Call     *VoiceCall   `json:"call,omitempty"`
}

// VoiceStateEvent: POST /rooms/voice/state - the origin reports one state (changed, or
// gone when Left). MovedTo, on a Left, is where a moderator moved the user: the client's cue
// to rejoin there. Room ids differ per instance, hence a ref and not an id.
type VoiceStateEvent struct {
	Room    RoomRef    `json:"room"`
	State   VoiceState `json:"state"`
	Left    bool       `json:"left,omitempty"`
	MovedTo *RoomRef   `json:"moved_to,omitempty"`
}

// VoiceStatesRequest: POST /rooms/voice/states - who is in these rooms hosted here. A mirror
// asks about the rooms it holds states or a call for, at most voice.MaxSyncRooms at a time.
type VoiceStatesRequest struct {
	Rooms []RoomRef `json:"rooms"`
}

// VoiceRoomStates is one room's running call: an entry of the answer to that request, and of
// a space snapshot's voice_rooms. Gone says the room is not there for the asker any more, which
// is how a live empty room is told from one to stop asking about.
type VoiceRoomStates struct {
	Room   RoomRef      `json:"room"`
	Gone   bool         `json:"gone,omitempty"`
	States []VoiceState `json:"states"`
	Call   *VoiceCall   `json:"call,omitempty"`
}

// VoiceStatesReply lists the rooms asked about, as many as fit; one left out is asked about
// again. The route always answers 200 when it exists, so a whole-request 404 can only mean
// an older origin.
type VoiceStatesReply struct {
	Rooms []VoiceRoomStates `json:"rooms"`
}

// The client reads 4 MiB of a reply, and a truncated answer would count as the origin not
// answering. A snapshot carries everything else about the space as well.
const (
	maxVoiceSyncBytes     = 3 << 20
	maxSnapshotVoiceBytes = 1 << 20
)

// VoiceCallEvent: POST /rooms/voice/call - the origin reports the call starting,
// changing (ringing set, re-ring) or ending.
type VoiceCallEvent struct {
	Room    RoomRef    `json:"room"`
	Event   string     `json:"event"` // CALL_CREATE | CALL_UPDATE | CALL_DELETE
	Call    *VoiceCall `json:"call,omitempty"`
	Starter *Profile   `json:"starter,omitempty"`
	Rerung  bool       `json:"rerung,omitempty"`
}

// VoiceUserRequest: POST /rooms/voice/leave and /rooms/voice/decline.
type VoiceUserRequest struct {
	Room RoomRef `json:"room"`
	User string  `json:"user"` // FID, must belong to the requesting instance
	// SessionID, on a leave, is the session that left. A join goes straight to the origin
	// while this request waits in a queue, and a newer session must survive it.
	SessionID string `json:"session_id,omitempty"`
}

// VoiceSelfUpdate: POST /rooms/voice/self - the user's own flags changed.
type VoiceSelfUpdate struct {
	Room       RoomRef `json:"room"`
	User       string  `json:"user"`
	SelfMute   *bool   `json:"self_mute,omitempty"`
	SelfDeaf   *bool   `json:"self_deaf,omitempty"`
	SelfVideo  *bool   `json:"self_video,omitempty"`
	SelfStream *bool   `json:"self_stream,omitempty"`
}

// VoiceRing: POST /rooms/voice/ring.
type VoiceRing struct {
	Room    RoomRef  `json:"room"`
	User    string   `json:"user"`
	Targets []string `json:"targets,omitempty"` // FIDs; empty = everyone not yet in
}

// ---- conversions ------------------------------------------------------------------------

// SetVoice wires the voice service federation serves remote users through.
func (s *Service) SetVoice(v *voice.Service) { s.voice = v }

func (s *Service) voiceStateToWire(ctx context.Context, st *voice.State) (VoiceState, error) {
	u, err := s.users.GetByID(ctx, st.UserID)
	if err != nil {
		return VoiceState{}, err
	}
	if u == nil {
		return VoiceState{}, ErrRemoteUserNotFound
	}
	return VoiceState{
		User:       s.ProfileOf(u),
		Identity:   st.Identity(),
		SessionID:  st.SessionID,
		SelfMute:   st.SelfMute,
		SelfDeaf:   st.SelfDeaf,
		Mute:       st.Mute,
		Deaf:       st.Deaf,
		SelfVideo:  st.SelfVideo,
		SelfStream: st.SelfStream,
		Suppress:   st.Suppress,
		Priority:   st.Priority,
		Connected:  st.Connected,
		JoinedAt:   st.JoinedAt,
	}, nil
}

// voiceStateFromWire turns a relayed state into this instance's ids. from is the
// instance that vouches for the profile (the origin, or the instance that sent it). Space
// ids are local to each instance, so the space comes from the room. On a mirror IssuedAt
// means "last heard from the origin": no token is issued here, and reconciling needs to know
// which of two writes is newer.
func (s *Service) voiceStateFromWire(ctx context.Context, from string, room *rooms.RoomWithParticipants, w VoiceState) (*voice.State, error) {
	u, err := s.resolveProfile(ctx, w.User, from)
	if err != nil {
		return nil, err
	}
	var spaceID int64
	if room.SpaceID != nil {
		spaceID = *room.SpaceID
	}
	return &voice.State{
		UserID:     u.ID,
		RoomID:     room.ID,
		SpaceID:    spaceID,
		SessionID:  w.SessionID,
		Ident:      w.Identity,
		SelfMute:   w.SelfMute,
		SelfDeaf:   w.SelfDeaf,
		Mute:       w.Mute,
		Deaf:       w.Deaf,
		SelfVideo:  w.SelfVideo,
		SelfStream: w.SelfStream,
		Suppress:   w.Suppress,
		Priority:   w.Priority,
		Connected:  w.Connected,
		JoinedAt:   w.JoinedAt,
		IssuedAt:   time.Now().UTC(),
		User:       voice.SummaryOf(u),
	}, nil
}

// voiceStatesFromWire converts a list of states. complete is false when one could not be
// read, so the list cannot be taken to name everyone.
func (s *Service) voiceStatesFromWire(ctx context.Context, from string, room *rooms.RoomWithParticipants, list []VoiceState) (out []voice.State, complete bool) {
	out = make([]voice.State, 0, len(list))
	complete = true
	for _, w := range list {
		st, err := s.voiceStateFromWire(ctx, from, room, w)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": from, "fid": w.User.FID, "room_id": room.ID})
			complete = false
			continue
		}
		out = append(out, *st)
	}
	return out, complete
}

func (s *Service) callToWire(ctx context.Context, c *voice.Call) *VoiceCall {
	if c == nil {
		return nil
	}
	ids := append([]int64{c.StartedBy}, c.Ringing...)
	users, _ := s.users.GetByIDs(ctx, ids)
	fidAt := func(i int) string {
		if i < len(users) && users[i] != nil {
			return s.FIDOf(users[i])
		}
		return ""
	}
	out := &VoiceCall{StartedBy: fidAt(0), StartedAt: c.StartedAt, Ringing: []string{}}
	for i := range c.Ringing {
		if f := fidAt(i + 1); f != "" {
			out.Ringing = append(out.Ringing, f)
		}
	}
	return out
}

func (s *Service) callFromWire(ctx context.Context, roomID int64, w *VoiceCall) *voice.Call {
	if w == nil {
		return nil
	}
	c := &voice.Call{RoomID: roomID, StartedAt: w.StartedAt}
	if uid, err := s.ResolveLocalID(ctx, w.StartedBy); err == nil {
		c.StartedBy = uid
	}
	for _, f := range w.Ringing {
		if uid, err := s.ResolveLocalID(ctx, f); err == nil {
			c.Ringing = append(c.Ringing, uid)
		}
	}
	return c
}

// peerErrorMessage pulls the "error" out of a peer's JSON reply, or returns the body.
func peerErrorMessage(body string) string {
	var m struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &m) == nil && m.Error != "" {
		return m.Error
	}
	return strings.TrimSpace(body)
}

// ---- outbound (voice.Federator) --------------------------------------------------------

func (s *Service) JoinRemoteVoice(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User, in voice.JoinInput) (*voice.RemoteJoin, error) {
	ref, err := s.roomRef(ctx, room.ID)
	if err != nil {
		return nil, err
	}
	var reply VoiceJoinReply
	body := VoiceJoinRequest{Room: ref, User: s.ProfileOf(user), SelfMute: in.SelfMute, SelfDeaf: in.SelfDeaf}
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/rooms/voice/join", body, &reply); err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			return nil, &voice.OriginError{Status: se.Status, Message: peerErrorMessage(se.Body)}
		}
		logger.Err("federation", err, map[string]any{"peer": origin, "path": "/rooms/voice/join"})
		return nil, voice.ErrOriginUnavailable
	}
	st, err := s.voiceStateFromWire(ctx, origin, room, reply.State)
	if err != nil {
		return nil, err
	}
	// The origin knows us by our shadow row there; here the state is ours.
	st.UserID = user.ID
	st.User = voice.SummaryOf(user)
	states := make([]voice.State, 0, len(reply.States)+1)
	for _, w := range reply.States {
		if w.Identity == st.Ident {
			continue
		}
		other, err := s.voiceStateFromWire(ctx, origin, room, w)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": origin, "fid": w.User.FID})
			continue
		}
		states = append(states, *other)
	}
	states = append(states, *st)
	call := s.callFromWire(ctx, room.ID, reply.Call)
	return &voice.RemoteJoin{
		Result: &voice.JoinResult{
			URL:      reply.URL,
			Token:    reply.Token,
			RoomName: reply.RoomName,
			Bitrate:  reply.Bitrate,
			State:    st,
			States:   states,
			Call:     call.View(),
		},
		Call: call,
	}, nil
}

func (s *Service) voiceUserRequest(ctx context.Context, room *rooms.RoomWithParticipants, user *auth.User) (VoiceUserRequest, bool) {
	ref, err := s.roomRef(ctx, room.ID)
	if err != nil {
		return VoiceUserRequest{}, false
	}
	return VoiceUserRequest{Room: ref, User: s.FIDOf(user)}, true
}

func (s *Service) LeaveRemoteVoiceSession(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User, sessionID string) {
	if body, ok := s.voiceUserRequest(ctx, room, user); ok {
		body.SessionID = sessionID
		s.send(origin, http.MethodPost, "/rooms/voice/leave", body)
	}
}

func (s *Service) DeclineRemoteCall(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User) {
	if body, ok := s.voiceUserRequest(ctx, room, user); ok {
		s.send(origin, http.MethodPost, "/rooms/voice/decline", body)
	}
}

func (s *Service) UpdateRemoteVoiceSelf(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User, in voice.SelfInput) {
	ref, err := s.roomRef(ctx, room.ID)
	if err != nil {
		return
	}
	s.send(origin, http.MethodPost, "/rooms/voice/self", VoiceSelfUpdate{
		Room: ref, User: s.FIDOf(user),
		SelfMute: in.SelfMute, SelfDeaf: in.SelfDeaf, SelfVideo: in.SelfVideo, SelfStream: in.SelfStream,
	})
}

func (s *Service) RingRemoteCall(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User, targets []int64) {
	ref, err := s.roomRef(ctx, room.ID)
	if err != nil {
		return
	}
	body := VoiceRing{Room: ref, User: s.FIDOf(user)}
	if len(targets) > 0 {
		if users, err := s.users.GetByIDs(ctx, targets); err == nil {
			for _, u := range users {
				if u != nil {
					body.Targets = append(body.Targets, s.FIDOf(u))
				}
			}
		}
	}
	s.send(origin, http.MethodPost, "/rooms/voice/ring", body)
}

// ModerateRemoteVoice mutes, deafens or moves a member in a space hosted elsewhere, through
// the voice.moderate op. The relay the origin then sends is what updates this instance.
func (s *Service) ModerateRemoteVoice(ctx context.Context, origin string, spaceID int64, actor *auth.User, targetID int64, in voice.ModerateInput) error {
	fid, err := s.fidOfLocal(ctx, targetID)
	if err != nil {
		return err
	}
	room, err := s.optOriginRoomID(ctx, origin, in.RoomID)
	if err != nil {
		return voice.ErrInvalidTarget
	}
	return voiceOriginError(s.manage(ctx, origin, spaceID, actor, "voice.moderate", manageVoice{User: fid, Mute: in.Mute, Deaf: in.Deaf, Room: room}, nil))
}

// DisconnectRemoteVoice kicks a member out of voice in a space hosted elsewhere.
func (s *Service) DisconnectRemoteVoice(ctx context.Context, origin string, spaceID int64, actor *auth.User, targetID int64) error {
	fid, err := s.fidOfLocal(ctx, targetID)
	if err != nil {
		return err
	}
	return voiceOriginError(s.manage(ctx, origin, spaceID, actor, "voice.disconnect", manageVoice{User: fid}, nil))
}

// voiceOriginError puts a failed /spaces/manage in voice's terms: its error switch knows
// nothing of the spaces forms, and would answer 500 for a refusal.
func voiceOriginError(err error) error {
	var oe *spaces.OriginError
	switch {
	case errors.As(err, &oe):
		return &voice.OriginError{Status: oe.Status, Message: oe.Message}
	case errors.Is(err, spaces.ErrOriginUnavailable):
		return voice.ErrOriginUnavailable
	}
	return err
}

// RemoteVoiceStates asks a room's origin who is in it (voice/mirror.go has the rest).
func (s *Service) RemoteVoiceStates(ctx context.Context, origin string, list []*rooms.RoomWithParticipants) ([]voice.RoomSync, error) {
	req := VoiceStatesRequest{Rooms: make([]RoomRef, 0, len(list))}
	byRef := make(map[string]*rooms.RoomWithParticipants, len(list))
	for _, room := range list {
		// From the room as it was read, not roomRef: a mirror going away under this question
		// must not have its rooms registered as hosted here.
		req.Rooms = append(req.Rooms, RoomRef{OriginDomain: origin, OriginRoomID: room.Federation.OriginID})
		byRef[room.Federation.OriginID] = room
	}
	var reply VoiceStatesReply
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/rooms/voice/states", req, &reply); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil, voice.ErrOriginPredatesSync
		}
		return nil, err
	}
	if reply.Rooms == nil {
		return nil, ErrRemoteUnavailable // a 200 with no answer in it is no answer
	}
	out := make([]voice.RoomSync, 0, len(reply.Rooms))
	for i := range reply.Rooms {
		vr := &reply.Rooms[i]
		room := byRef[vr.Room.OriginRoomID]
		if room == nil || vr.Room.OriginDomain != origin {
			continue
		}
		rs := voice.RoomSync{RoomID: room.ID, Gone: vr.Gone}
		if !vr.Gone {
			var complete bool
			rs.States, complete = s.voiceStatesFromWire(ctx, origin, room, vr.States)
			rs.Incomplete = !complete
			rs.Call = s.callFromWire(ctx, room.ID, vr.Call)
		}
		out = append(out, rs)
	}
	return out, nil
}

// voiceRoomsFor is the running calls in a space hosted here as a peer may see them, for its
// snapshot. It is best-effort: a snapshot with fewer calls in it is still a snapshot, and
// what it leaves out arrives with the next relay.
func (s *Service) voiceRoomsFor(ctx context.Context, spaceID int64, peer string, list []*rooms.Room) []VoiceRoomStates {
	if s.voice == nil {
		return nil
	}
	ids := make([]int64, 0, len(list))
	for _, r := range list {
		if r.Type == rooms.TypeSpaceVoice {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	live, err := s.voice.LiveStates(ctx, ids)
	if err != nil {
		logger.Err("federation", err, map[string]any{"space_id": spaceID, "stage": "snapshot voice rooms"})
		return nil
	}
	var out []VoiceRoomStates
	size := 0
	for _, roomID := range ids {
		states, known := s.statesForPeer(ctx, spaceID, roomID, peer, live[roomID])
		if !known || len(states) == 0 {
			continue
		}
		ref, err := s.roomRef(ctx, roomID)
		if err != nil {
			continue
		}
		entry, err := s.voiceRoomWire(ctx, ref, states, nil)
		if err != nil {
			continue
		}
		raw, err := json.Marshal(entry)
		if err != nil || size+len(raw) > maxSnapshotVoiceBytes {
			break
		}
		size += len(raw)
		out = append(out, entry)
	}
	return out
}

// voiceRoomWire is one room's states and call for a peer. A state whose user no longer
// exists is left out, so the peer drops it; any other failure means nothing may be said about
// who is there.
func (s *Service) voiceRoomWire(ctx context.Context, ref RoomRef, states []voice.State, call *voice.Call) (VoiceRoomStates, error) {
	out := VoiceRoomStates{Room: ref, States: make([]VoiceState, 0, len(states)), Call: s.callToWire(ctx, call)}
	for i := range states {
		w, err := s.voiceStateToWire(ctx, &states[i])
		if errors.Is(err, ErrRemoteUserNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		out.States = append(out.States, w)
	}
	return out, nil
}

// statesForPeer is what a peer may hear of who is in a space's room: everything when the room
// is open or one of its members can view it, otherwise its own users only. A mirror holding
// states for a room nobody there can see any more gets them cleared by this answer, and a
// user of its own still in the call is not hung up by it. known is false when the viewers
// could not be read (a room always has one, its owner): nothing can be said about that room
// then, and saying "nobody" would hang up the call on the peer.
func (s *Service) statesForPeer(ctx context.Context, spaceID, roomID int64, peer string, states []voice.State) (out []voice.State, known bool) {
	if len(states) == 0 {
		return nil, true
	}
	viewers, everyone := s.spaceSvc.ViewersOf(ctx, spaceID, roomID)
	if everyone {
		return states, true
	}
	if len(viewers) == 0 {
		return nil, false
	}
	homedThere := func(u *auth.User) bool { return u != nil && u.IsRemote() && u.HomeDomain == peer }
	users, err := s.users.GetByIDs(ctx, viewers)
	if err != nil {
		return nil, false
	}
	if slices.ContainsFunc(users, homedThere) {
		return states, true
	}
	out = make([]voice.State, 0, len(states))
	for i := range states {
		u, err := s.users.GetByID(ctx, states[i].UserID)
		if err != nil {
			return nil, false
		}
		if homedThere(u) {
			out = append(out, states[i])
		}
	}
	return out, true
}

// applyVoiceRooms records the calls a snapshot says are running, which is how a mirror finds
// out about one that started before anyone here could hear of it. asked is when the
// snapshot was requested.
func (s *Service) applyVoiceRooms(ctx context.Context, origin string, list []VoiceRoomStates, asked time.Time) {
	if s.voice == nil {
		return
	}
	for i := range list {
		vr := &list[i]
		if vr.Room.OriginDomain != origin {
			continue
		}
		oid, err := id.Parse(vr.Room.OriginRoomID)
		if err != nil {
			continue
		}
		local, err := s.localRoomID(ctx, origin, oid, false)
		if err != nil {
			continue
		}
		room, err := s.roomSvc.LoadRoom(ctx, local)
		if err != nil {
			continue
		}
		states, _ := s.voiceStatesFromWire(ctx, origin, room, vr.States)
		s.voice.ApplySnapshotStates(ctx, room, states, asked)
	}
}

func (s *Service) AfterVoiceStateChanged(ctx context.Context, room *rooms.RoomWithParticipants, st *voice.State, left bool) {
	s.relayVoiceState(ctx, room, st, left, nil)
}

func (s *Service) AfterVoiceStateMoved(ctx context.Context, room *rooms.RoomWithParticipants, st *voice.State, dest *rooms.RoomWithParticipants) {
	s.relayVoiceState(ctx, room, st, true, dest)
}

func (s *Service) relayVoiceState(ctx context.Context, room *rooms.RoomWithParticipants, st *voice.State, left bool, dest *rooms.RoomWithParticipants) {
	targets := s.voiceTargets(ctx, room, st)
	if len(targets) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, room.ID)
	if err != nil {
		return
	}
	w, err := s.voiceStateToWire(ctx, st)
	if err != nil {
		logger.Err("federation", err, map[string]any{"room_id": room.ID, "user_id": st.UserID})
		return
	}
	ev := VoiceStateEvent{Room: ref, State: w, Left: left}
	ctx = withoutExcludedPeer(ctx)
	if dest != nil {
		// Where they went is the destination's occupancy, so it goes only where that may be
		// known; everyone else just sees them leave.
		if to, err := s.roomRef(ctx, dest.ID); err == nil {
			moved := ev
			moved.MovedTo = &to
			inDest := s.voiceTargets(ctx, dest, st)
			told := make(map[string]struct{}, len(targets))
			for d := range targets {
				if _, ok := inDest[d]; ok {
					told[d] = struct{}{}
					delete(targets, d)
				}
			}
			s.fanOut(ctx, told, http.MethodPost, "/rooms/voice/state", moved)
		}
	}
	s.fanOut(ctx, targets, http.MethodPost, "/rooms/voice/state", ev)
}

// voiceTargets is who hears of a voice state: relayTargets, so a peer with no viewer of a
// private room learns nothing of who is in it, plus the user's own home, which is told about
// them whatever else it can see. That leaves a peer that lost its last viewer in the middle of
// a call: it holds states the relays no longer touch, and it is not told they ended, since a
// left is occupancy too. Its next pull of /rooms/voice/states clears them (statesForPeer
// answers with its own users alone), within a minute.
func (s *Service) voiceTargets(ctx context.Context, room *rooms.RoomWithParticipants, st *voice.State) map[string]struct{} {
	targets, origin := s.relayTargets(ctx, room.ID, room.ParticipantIDs)
	if room.SpaceID == nil || origin != "" {
		return targets
	}
	u, err := s.users.GetByID(ctx, st.UserID)
	if err != nil || u == nil || !u.IsRemote() {
		return targets
	}
	if _, ok := s.spacePeers(ctx, *room.SpaceID)[u.HomeDomain]; ok {
		if targets == nil {
			targets = map[string]struct{}{}
		}
		targets[u.HomeDomain] = struct{}{}
	}
	return targets
}

func (s *Service) AfterCallChanged(ctx context.Context, room *rooms.RoomWithParticipants, event string, call *voice.Call, starter *auth.User, rerung bool) {
	targets, _ := s.relayTargets(ctx, room.ID, room.ParticipantIDs)
	if len(targets) == 0 {
		return
	}
	ref, err := s.roomRef(ctx, room.ID)
	if err != nil {
		return
	}
	ev := VoiceCallEvent{Room: ref, Event: event, Call: s.callToWire(ctx, call), Rerung: rerung}
	if starter != nil {
		p := s.ProfileOf(starter)
		ev.Starter = &p
	}
	s.fanOut(withoutExcludedPeer(ctx), targets, http.MethodPost, "/rooms/voice/call", ev)
}

// ---- inbound ----------------------------------------------------------------------------

var errNotHosting = errors.New("this instance does not host that call")

// voiceHosted resolves a room the requesting instance may act in and checks this
// instance is the one hosting its call.
func (h *Handler) voiceHosted(c fiber.Ctx, ref RoomRef) (*roomScope, error) {
	sc, err := h.roomFor(c, ref)
	if err != nil {
		return nil, err
	}
	if !sc.hostedHere {
		return nil, errNotHosting
	}
	if h.svc.voice == nil {
		return nil, voice.ErrDisabled
	}
	return sc, nil
}

func voiceFail(c fiber.Ctx, err error, fields map[string]any) error {
	switch {
	case errors.Is(err, errNotHosting):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, voice.ErrDisabled):
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrRemoteUserNotFound), errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, ErrPeerNotAllowed), errors.Is(err, ErrInvalidFID):
		return fail(c, err, fields)
	}
	return voice.WriteError(c, err, fields)
}

// voiceActor resolves the requesting instance's user named by fid and checks they are
// in the room.
func (h *Handler) voiceActor(c fiber.Ctx, fid string, sc *roomScope) (*auth.User, error) {
	requester := RequesterDomain(c)
	if _, domain, err := ParseFID(fid); err != nil || domain != requester {
		return nil, errForeignUser
	}
	uid, err := h.svc.ResolveLocalID(c.Context(), fid)
	if err != nil {
		return nil, err
	}
	u, err := h.svc.users.GetByID(c.Context(), uid)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrRemoteUserNotFound
	}
	if !h.svc.inRoom(c.Context(), sc, u.ID) {
		return nil, errNotInRoom
	}
	return u, nil
}

var (
	errForeignUser = errors.New("user must belong to the requesting instance")
	errNotInRoom   = errors.New("user is not in this room")
)

func actorFail(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, errForeignUser), errors.Is(err, errNotInRoom):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	}
	return fail(c, err, nil)
}

// VoiceJoin POST /rooms/voice/join - mint a token for one of the requesting instance's
// users in a call this instance hosts.
func (h *Handler) VoiceJoin(c fiber.Ctx) error {
	var body VoiceJoinRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	if _, domain, err := ParseFID(body.User.FID); err != nil || domain != requester {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": errForeignUser.Error()})
	}
	sc, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	ctx := c.Context()
	user, err := h.svc.EnsureShadow(ctx, requester, body.User)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	if !h.svc.inRoom(ctx, sc, user.ID) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": errNotInRoom.Error()})
	}
	roomID := sc.room.ID
	res, err := h.svc.voice.Join(ctx, user, roomID, voice.JoinInput{SelfMute: body.SelfMute, SelfDeaf: body.SelfDeaf})
	if err != nil {
		return voice.WriteError(c, err, map[string]any{"room_id": roomID, "user_id": user.ID})
	}
	reply := VoiceJoinReply{URL: res.URL, Token: res.Token, RoomName: res.RoomName, Bitrate: res.Bitrate, States: []VoiceState{}}
	if reply.State, err = h.svc.voiceStateToWire(ctx, res.State); err != nil {
		return fail(c, err, map[string]any{"room_id": roomID})
	}
	for i := range res.States {
		w, err := h.svc.voiceStateToWire(ctx, &res.States[i])
		if err != nil {
			continue
		}
		reply.States = append(reply.States, w)
	}
	if _, call, err := h.svc.voice.RoomStates(ctx, user.ID, roomID); err == nil {
		reply.Call = h.svc.callToWire(ctx, call)
	}
	return c.JSON(reply)
}

// VoiceLeave POST /rooms/voice/leave.
func (h *Handler) VoiceLeave(c fiber.Ctx) error {
	var body VoiceUserRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	user, err := h.voiceActor(c, body.User, sc)
	if err != nil {
		return actorFail(c, err)
	}
	if err := h.svc.voice.LeaveFederated(c.Context(), user.ID, sc.room.ID, body.SessionID); err != nil {
		return voice.WriteError(c, err, map[string]any{"room_id": sc.room.ID, "user_id": user.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// VoiceSelf POST /rooms/voice/self.
func (h *Handler) VoiceSelf(c fiber.Ctx) error {
	var body VoiceSelfUpdate
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	user, err := h.voiceActor(c, body.User, sc)
	if err != nil {
		return actorFail(c, err)
	}
	in := voice.SelfInput{SelfMute: body.SelfMute, SelfDeaf: body.SelfDeaf, SelfVideo: body.SelfVideo, SelfStream: body.SelfStream}
	if _, err := h.svc.voice.UpdateSelf(c.Context(), user, in); err != nil {
		if errors.Is(err, voice.ErrNotInVoice) {
			return c.SendStatus(http.StatusNoContent) // already gone; nothing to flag
		}
		return voice.WriteError(c, err, map[string]any{"user_id": user.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// VoiceRing POST /rooms/voice/ring.
func (h *Handler) VoiceRing(c fiber.Ctx) error {
	var body VoiceRing
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	user, err := h.voiceActor(c, body.User, sc)
	if err != nil {
		return actorFail(c, err)
	}
	ctx := c.Context()
	var targets []int64
	for _, fid := range body.Targets {
		if uid, err := h.svc.ResolveLocalID(ctx, fid); err == nil {
			targets = append(targets, uid)
		}
	}
	if _, err := h.svc.voice.Ring(ctx, user, sc.room.ID, targets); err != nil {
		return voice.WriteError(c, err, map[string]any{"room_id": sc.room.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// VoiceDecline POST /rooms/voice/decline.
func (h *Handler) VoiceDecline(c fiber.Ctx) error {
	var body VoiceUserRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	user, err := h.voiceActor(c, body.User, sc)
	if err != nil {
		return actorFail(c, err)
	}
	if err := h.svc.voice.Decline(c.Context(), user, sc.room.ID); err != nil && !errors.Is(err, voice.ErrNoCall) {
		return voice.WriteError(c, err, map[string]any{"room_id": sc.room.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// voiceMirror resolves a relayed room and checks the requesting instance is its origin,
// the only instance that may push voice state for it.
func (h *Handler) voiceMirror(c fiber.Ctx, ref RoomRef) (*rooms.RoomWithParticipants, error) {
	sc, err := h.roomFor(c, ref)
	if err != nil {
		return nil, err
	}
	if !sc.fromOrigin {
		return nil, errNotOrigin
	}
	return h.svc.roomSvc.LoadRoom(c.Context(), sc.room.ID)
}

var errNotOrigin = errors.New("only the room's origin hosts its call")

// VoiceState POST /rooms/voice/state - mirror a state the origin reported.
func (h *Handler) VoiceState(c fiber.Ctx) error {
	var body VoiceStateEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	room, err := h.voiceMirror(c, body.Room)
	if err != nil {
		if errors.Is(err, errNotOrigin) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		}
		return fail(c, err, nil)
	}
	if h.svc.voice == nil {
		return c.SendStatus(http.StatusNoContent) // no voice here: nothing to mirror into
	}
	ctx := c.Context()
	st, err := h.svc.voiceStateFromWire(ctx, RequesterDomain(c), room, body.State)
	if err != nil {
		return fail(c, err, map[string]any{"room_id": room.ID})
	}
	var movedTo int64
	if body.Left {
		movedTo = h.svc.movedToLocal(ctx, RequesterDomain(c), body.MovedTo)
	}
	if err := h.svc.voice.ApplyRemoteVoiceState(ctx, room, st, body.Left, movedTo); err != nil {
		return fail(c, err, map[string]any{"room_id": room.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// movedToLocal is this instance's id for the room a move names, 0 when it has none: the left
// still applies, and the client just has nowhere to follow.
func (s *Service) movedToLocal(ctx context.Context, origin string, ref *RoomRef) int64 {
	if ref == nil || ref.OriginDomain != origin {
		return 0
	}
	oid, err := id.Parse(ref.OriginRoomID)
	if err != nil {
		return 0
	}
	local, err := s.localRoomID(ctx, origin, oid, false)
	if err != nil {
		return 0
	}
	return local
}

// VoiceStates POST /rooms/voice/states - who is in rooms hosted here, for an instance that
// holds a copy. It cannot be answered from a relay, so it is what repairs a lost one. Every
// room is answered for on its own; only something wrong here fails the request, and the
// asker keeps its states then.
func (h *Handler) VoiceStates(c fiber.Ctx) error {
	var body VoiceStatesRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if len(body.Rooms) > voice.MaxSyncRooms {
		return fail(c, ErrPayloadTooLarge, nil)
	}
	ctx := c.Context()
	peer := RequesterDomain(c)
	reply := VoiceStatesReply{Rooms: make([]VoiceRoomStates, 0, len(body.Rooms))}
	size := 0
	for _, ref := range body.Rooms {
		entry := VoiceRoomStates{Room: ref, States: []VoiceState{}}
		sc, err := h.voiceHosted(c, ref)
		switch {
		case err == nil:
			states, call, err := h.svc.voice.HostedStates(ctx, sc.room.ID)
			if err != nil {
				return fail(c, err, map[string]any{"room_id": sc.room.ID})
			}
			if sc.spaceID != 0 {
				var known bool
				if states, known = h.svc.statesForPeer(ctx, sc.spaceID, sc.room.ID, peer, states); !known {
					continue // left out, so it is asked about again
				}
			}
			if entry, err = h.svc.voiceRoomWire(ctx, ref, states, call); err != nil {
				return fail(c, err, map[string]any{"room_id": sc.room.ID})
			}
		case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, ErrPeerNotAllowed), errors.Is(err, errNotHosting),
			errors.Is(err, ErrInvalidFID), errors.Is(err, voice.ErrDisabled):
			entry.Gone = true
		default:
			return fail(c, err, nil)
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return fail(c, err, nil)
		}
		if size > 0 && size+len(raw) > maxVoiceSyncBytes {
			break
		}
		size += len(raw)
		reply.Rooms = append(reply.Rooms, entry)
	}
	return c.JSON(reply)
}

// VoiceCall POST /rooms/voice/call - mirror a call event the origin reported.
func (h *Handler) VoiceCall(c fiber.Ctx) error {
	var body VoiceCallEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	room, err := h.voiceMirror(c, body.Room)
	if err != nil {
		if errors.Is(err, errNotOrigin) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		}
		return fail(c, err, nil)
	}
	if h.svc.voice == nil {
		return c.SendStatus(http.StatusNoContent)
	}
	ctx := c.Context()
	var starter *auth.User
	if body.Starter != nil {
		if u, err := h.svc.resolveProfile(ctx, *body.Starter, RequesterDomain(c)); err == nil {
			starter = u
		}
	}
	call := h.svc.callFromWire(ctx, room.ID, body.Call)
	if err := h.svc.voice.ApplyRemoteCall(ctx, room, body.Event, call, starter, body.Rerung); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	return c.SendStatus(http.StatusNoContent)
}
