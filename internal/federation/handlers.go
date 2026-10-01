package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/devices"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// Handler serves the inbound side of /federation/v1 (every route except /instance sits
// behind Service.RequireInstance).
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func fail(c fiber.Ctx, err error, fields map[string]any) error {
	switch {
	case errors.Is(err, ErrRemoteUserNotFound), errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, messages.ErrMessageNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrPeerNotAllowed):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrInvalidFID), errors.Is(err, ErrInvalidHandle), errors.Is(err, ErrPayloadTooLarge),
		errors.Is(err, messages.ErrContentTooLong), errors.Is(err, messages.ErrInvalidInput), errors.Is(err, devices.ErrInvalidInput):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	logger.Err("federation", err, fields)
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

// Caps on what a peer may hand us in one payload. A peer is trusted to speak for its
// own users, not to size our rows.
const (
	maxAnnouncedParticipants = rooms.MaxGroupParticipants
	maxRelayedAttachments    = messages.MaxAttachmentsPerMessage
)

// ErrPayloadTooLarge is returned for inbound federation payloads over the caps.
var ErrPayloadTooLarge = errors.New("federation payload exceeds limits")

// sanitizeAttachments keeps only attachments with an http(s) URL - anything else would be
// handed to every client in the room as an <img>/<a> target.
func sanitizeAttachments(in []messages.Attachment) []messages.Attachment {
	out := make([]messages.Attachment, 0, len(in))
	for _, a := range in {
		if len(out) >= maxRelayedAttachments {
			break
		}
		u := strings.TrimSpace(a.URL)
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			continue
		}
		a.URL = u
		out = append(out, a)
	}
	return out
}

// Instance serves the instance document (also mounted at /.well-known/strafe).
func (h *Handler) Instance(c fiber.Ctx) error {
	return c.JSON(h.svc.Info())
}

// LookupUser GET /users/lookup?username=&discriminator= - a local user's profile.
func (h *Handler) LookupUser(c fiber.Ctx) error {
	username := c.Query("username")
	disc, err := strconv.Atoi(c.Query("discriminator"))
	if username == "" || err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "username and discriminator required"})
	}
	u, err := h.svc.users.GetByUsernameDiscriminator(c.Context(), username, disc)
	if err != nil {
		return fail(c, err, nil)
	}
	if u == nil || u.IsRemote() {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	return c.JSON(h.svc.ProfileOf(u))
}

// GetUser GET /users/:id - a local user's profile by id.
func (h *Handler) GetUser(c fiber.Ctx) error {
	uid, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	u, err := h.svc.users.GetByID(c.Context(), uid)
	if err != nil {
		return fail(c, err, nil)
	}
	if u == nil || u.IsRemote() {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	return c.JSON(h.svc.ProfileOf(u))
}

// UserUpdated POST /users/update - refresh a shadow and tell local rooms.
func (h *Handler) UserUpdated(c fiber.Ctx) error {
	var body ProfileUpdate
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	u, err := h.svc.EnsureShadow(c.Context(), requester, body.User)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	if h.svc.redis != nil {
		payload := map[string]interface{}{
			"user_id":       id.Format(u.ID),
			"avatar":        u.Avatar,
			"banner":        u.Banner,
			"display_name":  u.DisplayName,
			"username":      u.Username,
			"discriminator": strconv.Itoa(u.Discriminator),
			"bio":           u.Bio,
			"about_me":      u.AboutMe,
		}
		rows, _ := h.svc.roomRepo.ListByUser(c.Context(), u.ID)
		for _, row := range rows {
			stargate.PublishToSpace(c.Context(), h.svc.redis, row.RoomID, "USER_UPDATE", payload, h.svc.cfg.Stargate.Region)
		}
		// Spaces they are in here (members of a mirrored space, or remote members of one
		// hosted here) see the change in the member list too.
		for _, spaceID := range u.Spaces {
			stargate.PublishToSpace(c.Context(), h.svc.redis, spaceID, "USER_UPDATE", payload, h.svc.cfg.Stargate.Region)
		}
	}
	return c.SendStatus(http.StatusNoContent)
}

// roomScope is a federated room as this instance holds it: the mapping, the room row,
// its participants (PMs and groups) or its space (channels), and who the requesting
// instance is to it.
type roomScope struct {
	mapping      *RoomMapping
	room         *rooms.Room
	participants []int64
	spaceID      int64
	// fromOrigin: the requesting instance is the room's origin. hostedHere: this one is.
	fromOrigin bool
	hostedHere bool
}

// roomScope loads what a mapping points at.
func (s *Service) roomScope(ctx context.Context, m *RoomMapping) (*roomScope, error) {
	room, err := s.roomRepo.GetByID(ctx, m.RoomID)
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, rooms.ErrRoomNotFound
	}
	sc := &roomScope{mapping: m, room: room, hostedHere: s.IsLocalServer(m.OriginDomain)}
	if room.SpaceID != nil {
		sc.spaceID = *room.SpaceID
		return sc, nil
	}
	if sc.participants, err = s.roomRepo.GetParticipants(ctx, m.RoomID); err != nil {
		return nil, err
	}
	return sc, nil
}

// inRoom reports whether a local user id may act in the room: a participant, or a
// member of the channel's space.
func (s *Service) inRoom(ctx context.Context, sc *roomScope, uid int64) bool {
	if sc.spaceID != 0 {
		ok, _ := s.spaceSvc.IsMember(ctx, sc.spaceID, uid)
		return ok
	}
	for _, pid := range sc.participants {
		if pid == uid {
			return true
		}
	}
	return false
}

// roomParticipants is who a new message in the room fans out to.
func (s *Service) roomParticipants(ctx context.Context, sc *roomScope) []int64 {
	if sc.spaceID == 0 {
		return sc.participants
	}
	ids, _ := s.spaceSvc.ListSpaceMemberUserIDs(ctx, sc.spaceID)
	return ids
}

// roomFor resolves a RoomRef to the local room and checks the requesting instance is
// entitled to act on it: for a PM or group, it created the room or one of its users is
// a participant; for a space channel, it is the space's origin, or this instance is and
// the requester mirrors the space.
func (h *Handler) roomFor(c fiber.Ctx, ref RoomRef) (*roomScope, error) {
	originRoomID, err := id.Parse(ref.OriginRoomID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	ctx := c.Context()
	var m *RoomMapping
	if h.svc.IsLocalServer(ref.OriginDomain) {
		// A room hosted here is its own origin; its mapping is registered on first use
		// (a mirror may ask about a channel before anything was ever relayed from it).
		if room, err := h.svc.roomRepo.GetByID(ctx, originRoomID); err != nil {
			return nil, err
		} else if room == nil {
			return nil, rooms.ErrRoomNotFound
		}
		if _, err := h.svc.roomRef(ctx, originRoomID); err != nil {
			return nil, err
		}
		m = &RoomMapping{RoomID: originRoomID, OriginDomain: h.svc.fcfg.Domain, OriginRoomID: originRoomID}
	} else {
		var err error
		if m, err = h.svc.repo.GetRoomByOrigin(ctx, ref.OriginDomain, originRoomID); err != nil {
			return nil, err
		}
		if m == nil {
			return nil, rooms.ErrRoomNotFound
		}
	}
	sc, err := h.svc.roomScope(ctx, m)
	if err != nil {
		return nil, err
	}
	requester := RequesterDomain(c)
	sc.fromOrigin = ref.OriginDomain == requester
	if sc.fromOrigin {
		return sc, nil
	}
	if sc.spaceID != 0 {
		if !sc.hostedHere {
			return nil, ErrPeerNotAllowed // a mirror takes a channel's events from its origin only
		}
		ok, err := h.svc.repo.IsSpacePeer(ctx, sc.spaceID, requester)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrPeerNotAllowed
		}
		return sc, nil
	}
	byDomain, _, err := h.svc.remotePeers(ctx, sc.participants)
	if err != nil {
		return nil, err
	}
	if _, ok := byDomain[requester]; !ok {
		return nil, ErrPeerNotAllowed
	}
	return sc, nil
}

// senderAllowed: who may be named as the acting user in a relayed event. In a PM the
// requester speaks for its own users only; in a space channel the origin relays what
// any member did, so the requester must be the origin.
func (h *Handler) senderAllowed(c fiber.Ctx, sc *roomScope, fid string) bool {
	_, domain, err := ParseFID(fid)
	if err != nil {
		return false
	}
	if sc.spaceID != 0 {
		return sc.fromOrigin
	}
	return domain == RequesterDomain(c)
}

// messageFromEvent turns a relayed message into this instance's ids without storing it.
// from is the instance vouching for the profiles in it.
func (s *Service) messageFromEvent(ctx context.Context, from string, sc *roomScope, ev MessageEvent) (*messages.Message, error) {
	originMsgID, err := id.Parse(ev.Message.OriginMessageID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	msg := &messages.Message{
		RoomID:          sc.room.ID,
		Ciphertext:      ev.Ciphertext,
		Plaintext:       ev.Plaintext,
		MentionEveryone: ev.MentionEveryone,
		SystemType:      ev.SystemType,
		SystemPayload:   ev.SystemPayload,
		CreatedAt:       ev.CreatedAt.UTC(),
		UpdatedAt:       ev.UpdatedAt.UTC(),
	}
	// A channel keeps the origin's message ids everywhere (every message in it comes
	// from the origin), so replies, reactions and order agree without a lookup; a PM
	// gets a local id like before.
	if sc.spaceID != 0 {
		msg.ID = originMsgID
	} else {
		msg.ID = id.Next()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	if msg.UpdatedAt.IsZero() {
		msg.UpdatedAt = msg.CreatedAt
	}
	if ev.Sender != nil {
		sender, err := s.resolveProfile(ctx, *ev.Sender, from)
		if err != nil {
			return nil, err
		}
		if !s.inRoom(ctx, sc, sender.ID) {
			return nil, errNotInRoom
		}
		msg.SenderID = sender.ID
	} else if ev.SystemType == "" {
		return nil, messages.ErrInvalidInput
	}
	if ev.SenderDeviceID != "" {
		if did, err := id.Parse(ev.SenderDeviceID); err == nil {
			msg.SenderDeviceID = did
		}
	}
	if ev.ReplyTo != nil {
		if rid := s.localMessageID(ctx, sc.room.ID, *ev.ReplyTo); rid != 0 {
			msg.ReplyToID = &rid
		}
	}
	for _, fid := range ev.Mentions {
		if len(msg.Mentions) >= messages.MaxMentionsPerMessage {
			break
		}
		if uid, err := s.ResolveLocalID(ctx, fid); err == nil {
			msg.Mentions = append(msg.Mentions, uid)
		}
	}
	for _, raw := range ev.MentionRoles {
		if len(msg.MentionRoles) >= messages.MaxMentionsPerMessage {
			break
		}
		if rid, err := id.Parse(raw); err == nil {
			msg.MentionRoles = append(msg.MentionRoles, rid)
		}
	}
	msg.SetAttachments(sanitizeAttachments(ev.Attachments))
	return msg, nil
}

// applyMessageEvent stores a relayed message (idempotent on its origin id) with the
// bookkeeping a new message gets, and returns the local row.
func (s *Service) applyMessageEvent(ctx context.Context, from string, sc *roomScope, ev MessageEvent) (*messages.Message, error) {
	originMsgID, err := id.Parse(ev.Message.OriginMessageID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	if existing, err := s.repo.GetMessageByOrigin(ctx, ev.Message.OriginDomain, originMsgID); err == nil && existing != nil {
		if m, err := s.msgRepo.GetByID(ctx, existing.RoomID, existing.MessageID); err == nil && m != nil {
			return m, nil
		}
	}
	msg, err := s.messageFromEvent(ctx, from, sc, ev)
	if err != nil {
		return nil, err
	}
	if err := s.msgSvc.CreateFederated(ctx, sc.room.ID, s.roomParticipants(ctx, sc), msg); err != nil {
		return nil, err
	}
	if err := s.repo.PutMessageMapping(ctx, &MessageMapping{RoomID: sc.room.ID, MessageID: msg.ID, OriginDomain: ev.Message.OriginDomain, OriginMessageID: originMsgID}); err != nil {
		logger.Err("federation", err, map[string]any{"room_id": sc.room.ID, "message_id": msg.ID})
	}
	return msg, nil
}

// localMessageID maps a message reference to this instance's id for it (0 if unknown).
func (s *Service) localMessageID(ctx context.Context, roomID int64, ref MessageRef) int64 {
	originID, err := id.Parse(ref.OriginMessageID)
	if err != nil {
		return 0
	}
	if s.IsLocalServer(ref.OriginDomain) {
		return originID
	}
	if m, err := s.repo.GetMessageByOrigin(ctx, ref.OriginDomain, originID); err == nil && m != nil && m.RoomID == roomID {
		return m.MessageID
	}
	return 0
}

// RoomCreate POST /rooms - mirror a room the requesting instance created.
func (h *Handler) RoomCreate(c fiber.Ctx) error {
	var body RoomAnnounce
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	if body.Room.OriginDomain != requester {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "an instance may only announce rooms it created"})
	}
	if body.Type != rooms.TypePM && body.Type != rooms.TypeGroupPM {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "only PM and group rooms federate"})
	}
	if len(body.Participants) > maxAnnouncedParticipants || utf8.RuneCountInString(body.Name) > rooms.MaxRoomNameRunes {
		return fail(c, ErrPayloadTooLarge, nil)
	}
	originRoomID, err := id.Parse(body.Room.OriginRoomID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid origin_room_id"})
	}
	ctx := c.Context()
	if existing, err := h.svc.repo.GetRoomByOrigin(ctx, requester, originRoomID); err == nil && existing != nil {
		return c.JSON(RoomAnnounceReply{RoomID: id.Format(existing.RoomID)})
	}
	var ids []int64
	seen := map[int64]struct{}{}
	hasLocal := false
	creatorID := int64(0)
	for _, p := range body.Participants {
		u, err := h.svc.resolveProfile(ctx, p, requester)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": requester, "fid": p.FID})
			continue
		}
		if _, dup := seen[u.ID]; dup {
			continue
		}
		seen[u.ID] = struct{}{}
		ids = append(ids, u.ID)
		if !u.IsRemote() {
			hasLocal = true
		}
		if body.Creator != "" && p.FID == body.Creator {
			creatorID = u.ID
		}
	}
	if !hasLocal {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "no participants on this instance"})
	}
	if body.Type == rooms.TypePM && len(ids) != 2 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "a PM needs exactly two participants"})
	}
	e2ee := body.E2EEEnabled
	room := &rooms.Room{
		ID:          id.Next(),
		Type:        body.Type,
		Name:        body.Name,
		CreatorID:   creatorID,
		E2EEEnabled: &e2ee,
	}
	fed := &rooms.Federation{OriginDomain: requester, OriginID: body.Room.OriginRoomID}
	if err := h.svc.repo.PutRoomMapping(ctx, &RoomMapping{RoomID: room.ID, OriginDomain: requester, OriginRoomID: originRoomID}); err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	if _, err := h.svc.roomSvc.CreateMirror(ctx, room, ids, fed); err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	return c.Status(http.StatusCreated).JSON(RoomAnnounceReply{RoomID: id.Format(room.ID)})
}

// RoomParticipants PUT /rooms/participants - reconcile the member set.
func (h *Handler) RoomParticipants(c fiber.Ctx) error {
	var body ParticipantsUpdate
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if len(body.Participants) > maxAnnouncedParticipants {
		return fail(c, ErrPayloadTooLarge, nil)
	}
	sc, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	if sc.spaceID != 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "a space channel has no participant list"})
	}
	m := sc.mapping
	requester := RequesterDomain(c)
	ctx := c.Context()
	var target []int64
	seen := map[int64]struct{}{}
	for _, p := range body.Participants {
		u, err := h.svc.resolveProfile(ctx, p, requester)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": requester, "fid": p.FID})
			continue
		}
		if _, dup := seen[u.ID]; dup {
			continue
		}
		seen[u.ID] = struct{}{}
		target = append(target, u.ID)
	}
	if len(target) == 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "empty participant set"})
	}
	if _, err := h.svc.roomSvc.ApplyParticipants(ctx, m.RoomID, target); err != nil {
		return fail(c, err, map[string]any{"room_id": m.RoomID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// RoomPatch PATCH /rooms - name / E2EE changes.
func (h *Handler) RoomPatch(c fiber.Ctx) error {
	var body RoomPatch
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if body.Name != nil && utf8.RuneCountInString(*body.Name) > rooms.MaxRoomNameRunes {
		return fail(c, ErrPayloadTooLarge, nil)
	}
	sc, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	if sc.spaceID != 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "space channels change through /spaces/rooms"})
	}
	if _, err := h.svc.roomSvc.ApplyRoomPatch(c.Context(), sc.room.ID, body.Name, body.E2EEEnabled); err != nil {
		return fail(c, err, map[string]any{"room_id": sc.room.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// RoomTyping POST /rooms/typing.
func (h *Handler) RoomTyping(c fiber.Ctx) error {
	var body TypingEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	ctx := c.Context()
	requester := RequesterDomain(c)
	// In a channel hosted here a mirror sends its own members' typing; in a channel
	// hosted elsewhere the origin passes everyone's on.
	if _, domain, err := ParseFID(body.User); err != nil || (domain != requester && !(sc.spaceID != 0 && sc.fromOrigin)) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": errForeignUser.Error()})
	}
	uid, err := h.svc.ResolveLocalID(ctx, body.User)
	if err != nil {
		return fail(c, err, nil)
	}
	if !h.svc.inRoom(ctx, sc, uid) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": errNotInRoom.Error()})
	}
	h.svc.roomSvc.PublishTypingFrom(ctx, sc.room.ID, uid)
	if sc.spaceID != 0 && sc.hostedHere {
		// The origin tells the other mirrors (the sender's own instance already knows).
		h.svc.fanOut(WithExcludedPeer(ctx, requester), h.svc.spacePeers(ctx, sc.spaceID), http.MethodPost, "/rooms/typing", body)
	}
	return c.SendStatus(http.StatusNoContent)
}

// MessageCreate POST /rooms/messages.
func (h *Handler) MessageCreate(c fiber.Ctx) error {
	var body MessageEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	if body.Message.OriginDomain != requester {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "an instance may only relay its own messages"})
	}
	if _, err := id.Parse(body.Message.OriginMessageID); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid origin_message_id"})
	}
	sc, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	if sc.spaceID != 0 && !sc.fromOrigin {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "a channel's messages come from its origin"})
	}
	if body.Sender != nil && !h.senderAllowed(c, sc, body.Sender.FID) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "sender must belong to the requesting instance"})
	}
	msg, err := h.svc.applyMessageEvent(c.Context(), requester, sc, body)
	switch {
	case err == nil:
		return c.Status(http.StatusCreated).JSON(fiber.Map{"message_id": id.Format(msg.ID)})
	case errors.Is(err, errNotInRoom):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "sender is not in this room"})
	case errors.Is(err, messages.ErrInvalidInput):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "message needs a sender or a system_type"})
	}
	return fail(c, err, map[string]any{"room_id": sc.room.ID})
}

// localMessageID maps a message reference to this instance's id for it (0 if unknown).
func (h *Handler) localMessageID(c fiber.Ctx, roomID int64, ref MessageRef) int64 {
	return h.svc.localMessageID(c.Context(), roomID, ref)
}

// MessageEdit PATCH /rooms/messages.
func (h *Handler) MessageEdit(c fiber.Ctx) error {
	var body MessageEdit
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if body.Message.OriginDomain != RequesterDomain(c) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "an instance may only edit its own messages"})
	}
	sc, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	msgID := h.localMessageID(c, sc.room.ID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	if _, err := h.svc.msgSvc.EditFederated(c.Context(), sc.room.ID, msgID, body.Ciphertext, body.Plaintext); err != nil {
		return fail(c, err, map[string]any{"room_id": sc.room.ID, "message_id": msgID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// MessageDelete POST /rooms/messages/delete.
func (h *Handler) MessageDelete(c fiber.Ctx) error {
	var body MessageDelete
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if body.Message.OriginDomain != RequesterDomain(c) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "an instance may only delete its own messages"})
	}
	sc, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	msgID := h.localMessageID(c, sc.room.ID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	if err := h.svc.msgSvc.DeleteFederated(c.Context(), sc.room.ID, msgID); err != nil {
		return fail(c, err, map[string]any{"room_id": sc.room.ID, "message_id": msgID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// KeysQuery POST /keys/query - device keys of this instance's users.
func (h *Handler) KeysQuery(c fiber.Ctx) error {
	var body KeysQuery
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	local := map[string][]string{}
	for fid, devs := range body.DeviceKeys {
		if _, domain, err := ParseFID(fid); err == nil && h.svc.IsLocalServer(domain) {
			local[fid] = devs
		}
	}
	out, err := h.svc.devSvc.QueryKeys(c.Context(), local)
	if err != nil {
		return fail(c, err, nil)
	}
	return c.JSON(out)
}

// KeysClaim POST /keys/claim - one-time keys of this instance's users.
func (h *Handler) KeysClaim(c fiber.Ctx) error {
	var body KeysClaim
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	local := map[string]map[string]string{}
	for fid, devs := range body.OneTimeKeys {
		if _, domain, err := ParseFID(fid); err == nil && h.svc.IsLocalServer(domain) {
			local[fid] = devs
		}
	}
	out, err := h.svc.devSvc.ClaimKeys(c.Context(), local)
	if err != nil {
		return fail(c, err, nil)
	}
	return c.JSON(out)
}

// ToDevice POST /to_device - Olm/Megolm to-device traffic for this instance's users.
func (h *Handler) ToDevice(c fiber.Ctx) error {
	var body ToDevice
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	if _, domain, err := ParseFID(body.Sender.FID); err != nil || domain != requester {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "sender must belong to the requesting instance"})
	}
	sender, err := h.svc.EnsureShadow(c.Context(), requester, body.Sender)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	senderDeviceID, err := id.Parse(body.SenderDeviceID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid sender_device_id"})
	}
	if body.EventType == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "event_type required"})
	}
	if err := h.svc.devSvc.DeliverFromRemote(c.Context(), sender.ID, senderDeviceID, body.Sender.FID, body.EventType, body.Messages); err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	return c.SendStatus(http.StatusNoContent)
}

// Relationship POST /relationships - a friend request, acceptance or teardown from a user
// on the requesting instance towards one of ours.
func (h *Handler) Relationship(c fiber.Ctx) error {
	var body RelationshipEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	if _, domain, err := ParseFID(body.Actor.FID); err != nil || domain != requester {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "actor must belong to the requesting instance"})
	}
	ctx := c.Context()
	actor, err := h.svc.EnsureShadow(ctx, requester, body.Actor)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	targetID, err := h.svc.ResolveLocalID(ctx, body.Target)
	if err != nil {
		return fail(c, err, nil)
	}
	target, err := h.svc.users.GetByID(ctx, targetID)
	if err != nil {
		return fail(c, err, nil)
	}
	// A peer may only act towards our own users - never between two foreign ones.
	if target == nil || target.IsRemote() {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	switch body.Action {
	case "request":
		err = h.svc.relSvc.ApplyRemoteRequest(ctx, actor, target)
	case "accept":
		err = h.svc.relSvc.ApplyRemoteAccept(ctx, actor, target)
	case "remove":
		err = h.svc.relSvc.ApplyRemoteRemove(ctx, actor, target)
	default:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unknown action"})
	}
	switch {
	case err == nil:
		if body.Action == "accept" {
			// A new friendship: give their side our user's current status right away.
			h.svc.sendPresence(target, requester)
		}
		return c.SendStatus(http.StatusNoContent)
	case errors.Is(err, relationships.ErrRequestNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, relationships.ErrBotTarget):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	return fail(c, err, map[string]any{"peer": requester, "action": body.Action})
}

// applyReaction serves POST /rooms/reactions (add) and /rooms/reactions/delete: a
// reaction by one of the requesting instance's users on a message in a shared room.
func (h *Handler) applyReaction(c fiber.Ctx, remove bool) error {
	var body ReactionEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	if !h.senderAllowed(c, sc, body.User) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "user must belong to the requesting instance"})
	}
	ctx := c.Context()
	uid, err := h.svc.ResolveLocalID(ctx, body.User)
	if err != nil {
		return fail(c, err, nil)
	}
	if !h.svc.inRoom(ctx, sc, uid) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "user is not in this room"})
	}
	msgID := h.localMessageID(c, sc.room.ID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	if remove {
		err = h.svc.msgSvc.RemoveReactionFederated(ctx, sc.room.ID, msgID, uid, body.Emoji)
	} else {
		err = h.svc.msgSvc.AddReactionFederated(ctx, sc.room.ID, msgID, uid, body.Emoji)
	}
	switch {
	case err == nil:
		return c.SendStatus(http.StatusNoContent)
	case errors.Is(err, messages.ErrInvalidReaction), errors.Is(err, messages.ErrTooManyReactions):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	return fail(c, err, map[string]any{"room_id": sc.room.ID, "message_id": msgID})
}

func (h *Handler) ReactionAdd(c fiber.Ctx) error    { return h.applyReaction(c, false) }
func (h *Handler) ReactionRemove(c fiber.Ctx) error { return h.applyReaction(c, true) }

// maxRelayedCustomStatus mirrors the local limit (users/schema.go).
const maxRelayedCustomStatus = 128

// Presence POST /users/presence - how a user on the requesting instance now appears to
// others. Applied to their shadow row (so lists and READY carry it) and fanned out to
// their local friends like a local presence change.
func (h *Handler) Presence(c fiber.Ctx) error {
	var body PresenceEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	originID, domain, err := ParseFID(body.User)
	if err != nil || domain != requester {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "user must belong to the requesting instance"})
	}
	switch body.Presence.Status {
	case "online", "idle", "dnd", "offline":
	default:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid status"})
	}
	if utf8.RuneCountInString(body.Presence.CustomStatus) > maxRelayedCustomStatus {
		return fail(c, ErrPayloadTooLarge, nil)
	}
	ctx := c.Context()
	shadow, err := h.svc.users.GetByRemote(ctx, requester, originID)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	if shadow == nil {
		// Nobody here knows them yet; the shadow is created by the first real contact.
		return c.SendStatus(http.StatusNoContent)
	}
	online := body.Presence.Status != "offline"
	status := body.Presence.Status
	custom := strings.TrimSpace(body.Presence.CustomStatus)
	updated, err := h.svc.users.UpdateProfile(ctx, shadow.ID, &auth.ProfileUpdate{
		Presence: &auth.PresenceUpdate{Online: &online, Status: &status, CustomStatus: &custom},
	})
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester, "user_id": shadow.ID})
	}
	if updated != nil {
		stargate.PublishPresenceUpdate(ctx, h.svc.redis, h.svc.cfg.Stargate.Region, updated)
	}
	return c.SendStatus(http.StatusNoContent)
}

// Peers GET /federation/peers (client-authenticated, not S2S): instances we've talked to.
func (h *Handler) Peers(c fiber.Ctx) error {
	list, err := h.svc.repo.ListPeers(c.Context())
	if err != nil {
		return fail(c, err, nil)
	}
	out := make([]fiber.Map, 0, len(list))
	for _, p := range list {
		out = append(out, fiber.Map{
			"domain":         p.Domain,
			"federation_url": p.FederationURL,
			"api_url":        p.APIURL,
			"first_seen":     p.FirstSeen,
			"last_seen":      p.LastSeen,
			"blocked":        p.Blocked,
			"allowed":        h.svc.fcfg.IsAllowedPeer(p.Domain),
		})
	}
	return c.JSON(out)
}
