package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/voice"
)

// Calls across instances. The room's origin hosts the call on its LiveKit; a user
// elsewhere joins through their own instance, which asks the origin for a token
// (POST /rooms/voice/join, synchronous) and afterwards relays leave / self-flag /
// ring / decline requests to it. The origin pushes every voice state and call change to
// the other instances in the room (POST /rooms/voice/state, /rooms/voice/call), which
// mirror them for their own clients. See voice/federation.go for the service side.

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
// gone when Left).
type VoiceStateEvent struct {
	Room  RoomRef    `json:"room"`
	State VoiceState `json:"state"`
	Left  bool       `json:"left,omitempty"`
}

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
// instance that vouches for the profile (the origin, or the instance that sent it).
func (s *Service) voiceStateFromWire(ctx context.Context, from string, roomID int64, w VoiceState) (*voice.State, error) {
	u, err := s.resolveProfile(ctx, w.User, from)
	if err != nil {
		return nil, err
	}
	return &voice.State{
		UserID:     u.ID,
		RoomID:     roomID,
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
		IssuedAt:   w.JoinedAt,
		User:       voice.SummaryOf(u),
	}, nil
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
	st, err := s.voiceStateFromWire(ctx, origin, room.ID, reply.State)
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
		other, err := s.voiceStateFromWire(ctx, origin, room.ID, w)
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

func (s *Service) LeaveRemoteVoice(ctx context.Context, origin string, room *rooms.RoomWithParticipants, user *auth.User) {
	if body, ok := s.voiceUserRequest(ctx, room, user); ok {
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

func (s *Service) AfterVoiceStateChanged(ctx context.Context, room *rooms.RoomWithParticipants, st *voice.State, left bool) {
	byDomain, _, err := s.remotePeers(ctx, room.ParticipantIDs)
	if err != nil || len(byDomain) == 0 {
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
	for domain := range byDomain {
		s.send(domain, http.MethodPost, "/rooms/voice/state", ev)
	}
}

func (s *Service) AfterCallChanged(ctx context.Context, room *rooms.RoomWithParticipants, event string, call *voice.Call, starter *auth.User, rerung bool) {
	byDomain, _, err := s.remotePeers(ctx, room.ParticipantIDs)
	if err != nil || len(byDomain) == 0 {
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
	for domain := range byDomain {
		s.send(domain, http.MethodPost, "/rooms/voice/call", ev)
	}
}

// ---- inbound ----------------------------------------------------------------------------

var errNotHosting = errors.New("this instance does not host that call")

// voiceHosted resolves a room the requesting instance may act in and checks this
// instance is the one hosting its call.
func (h *Handler) voiceHosted(c fiber.Ctx, ref RoomRef) (*RoomMapping, []int64, error) {
	m, participants, err := h.roomFor(c, ref)
	if err != nil {
		return nil, nil, err
	}
	if !h.svc.IsLocalServer(m.OriginDomain) {
		return nil, nil, errNotHosting
	}
	if h.svc.voice == nil {
		return nil, nil, voice.ErrDisabled
	}
	return m, participants, nil
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
func (h *Handler) voiceActor(c fiber.Ctx, fid string, participants []int64) (*auth.User, error) {
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
	for _, pid := range participants {
		if pid == u.ID {
			return u, nil
		}
	}
	return nil, errNotInRoom
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
	m, participants, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	ctx := c.Context()
	user, err := h.svc.EnsureShadow(ctx, requester, body.User)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	isParticipant := false
	for _, pid := range participants {
		if pid == user.ID {
			isParticipant = true
			break
		}
	}
	if !isParticipant {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": errNotInRoom.Error()})
	}
	res, err := h.svc.voice.Join(ctx, user, m.RoomID, voice.JoinInput{SelfMute: body.SelfMute, SelfDeaf: body.SelfDeaf})
	if err != nil {
		return voice.WriteError(c, err, map[string]any{"room_id": m.RoomID, "user_id": user.ID})
	}
	reply := VoiceJoinReply{URL: res.URL, Token: res.Token, RoomName: res.RoomName, Bitrate: res.Bitrate, States: []VoiceState{}}
	if reply.State, err = h.svc.voiceStateToWire(ctx, res.State); err != nil {
		return fail(c, err, map[string]any{"room_id": m.RoomID})
	}
	for i := range res.States {
		w, err := h.svc.voiceStateToWire(ctx, &res.States[i])
		if err != nil {
			continue
		}
		reply.States = append(reply.States, w)
	}
	if _, call, err := h.svc.voice.RoomStates(ctx, user.ID, m.RoomID); err == nil {
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
	m, participants, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	user, err := h.voiceActor(c, body.User, participants)
	if err != nil {
		return actorFail(c, err)
	}
	if err := h.svc.voice.LeaveFederated(c.Context(), user.ID, m.RoomID); err != nil {
		return voice.WriteError(c, err, map[string]any{"room_id": m.RoomID, "user_id": user.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// VoiceSelf POST /rooms/voice/self.
func (h *Handler) VoiceSelf(c fiber.Ctx) error {
	var body VoiceSelfUpdate
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	_, participants, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	user, err := h.voiceActor(c, body.User, participants)
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
	m, participants, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	user, err := h.voiceActor(c, body.User, participants)
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
	if _, err := h.svc.voice.Ring(ctx, user, m.RoomID, targets); err != nil {
		return voice.WriteError(c, err, map[string]any{"room_id": m.RoomID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// VoiceDecline POST /rooms/voice/decline.
func (h *Handler) VoiceDecline(c fiber.Ctx) error {
	var body VoiceUserRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, participants, err := h.voiceHosted(c, body.Room)
	if err != nil {
		return voiceFail(c, err, nil)
	}
	user, err := h.voiceActor(c, body.User, participants)
	if err != nil {
		return actorFail(c, err)
	}
	if err := h.svc.voice.Decline(c.Context(), user, m.RoomID); err != nil && !errors.Is(err, voice.ErrNoCall) {
		return voice.WriteError(c, err, map[string]any{"room_id": m.RoomID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// voiceMirror resolves a relayed room and checks the requesting instance is its origin,
// the only instance that may push voice state for it.
func (h *Handler) voiceMirror(c fiber.Ctx, ref RoomRef) (*rooms.RoomWithParticipants, error) {
	m, _, err := h.roomFor(c, ref)
	if err != nil {
		return nil, err
	}
	if ref.OriginDomain != RequesterDomain(c) {
		return nil, errNotOrigin
	}
	return h.svc.roomSvc.LoadRoom(c.Context(), m.RoomID)
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
	st, err := h.svc.voiceStateFromWire(ctx, RequesterDomain(c), room.ID, body.State)
	if err != nil {
		return fail(c, err, map[string]any{"room_id": room.ID})
	}
	if err := h.svc.voice.ApplyRemoteVoiceState(ctx, room, st, body.Left); err != nil {
		return fail(c, err, map[string]any{"room_id": room.ID})
	}
	return c.SendStatus(http.StatusNoContent)
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
