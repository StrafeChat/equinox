package federation

import (
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
	"github.com/StrafeChat/equinox/internal/modules/devices"
	"github.com/StrafeChat/equinox/internal/modules/messages"
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
	}
	return c.SendStatus(http.StatusNoContent)
}

// roomFor resolves a RoomRef to the local mirror and checks the requesting instance is
// entitled to act on it (it created the room, or one of its users is a participant).
func (h *Handler) roomFor(c fiber.Ctx, ref RoomRef) (*RoomMapping, []int64, error) {
	originRoomID, err := id.Parse(ref.OriginRoomID)
	if err != nil {
		return nil, nil, ErrInvalidFID
	}
	m, err := h.svc.repo.GetRoomByOrigin(c.Context(), ref.OriginDomain, originRoomID)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, nil, rooms.ErrRoomNotFound
	}
	participants, err := h.svc.roomRepo.GetParticipants(c.Context(), m.RoomID)
	if err != nil {
		return nil, nil, err
	}
	requester := RequesterDomain(c)
	if ref.OriginDomain != requester {
		byDomain, _, err := h.svc.remotePeers(c.Context(), participants)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := byDomain[requester]; !ok {
			return nil, nil, ErrPeerNotAllowed
		}
	}
	return m, participants, nil
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
	m, _, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
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
	m, _, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	if _, err := h.svc.roomSvc.ApplyRoomPatch(c.Context(), m.RoomID, body.Name, body.E2EEEnabled); err != nil {
		return fail(c, err, map[string]any{"room_id": m.RoomID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// RoomTyping POST /rooms/typing.
func (h *Handler) RoomTyping(c fiber.Ctx) error {
	var body TypingEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, _, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	_, domain, err := ParseFID(body.User)
	if err != nil || domain != RequesterDomain(c) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "user must belong to the requesting instance"})
	}
	uid, err := h.svc.ResolveLocalID(c.Context(), body.User)
	if err != nil {
		return fail(c, err, nil)
	}
	h.svc.roomSvc.PublishTypingFrom(c.Context(), m.RoomID, uid)
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
	originMsgID, err := id.Parse(body.Message.OriginMessageID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid origin_message_id"})
	}
	m, participants, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	ctx := c.Context()
	if existing, err := h.svc.repo.GetMessageByOrigin(ctx, requester, originMsgID); err == nil && existing != nil {
		return c.JSON(fiber.Map{"message_id": id.Format(existing.MessageID)})
	}
	senderID := int64(0)
	if body.Sender != nil {
		if _, domain, err := ParseFID(body.Sender.FID); err != nil || domain != requester {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "sender must belong to the requesting instance"})
		}
		sender, err := h.svc.EnsureShadow(ctx, requester, *body.Sender)
		if err != nil {
			return fail(c, err, map[string]any{"peer": requester})
		}
		isParticipant := false
		for _, pid := range participants {
			if pid == sender.ID {
				isParticipant = true
				break
			}
		}
		if !isParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "sender is not in this room"})
		}
		senderID = sender.ID
	} else if body.SystemType == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "message needs a sender or a system_type"})
	}
	msg := &messages.Message{
		ID:              id.Next(),
		SenderID:        senderID,
		Ciphertext:      body.Ciphertext,
		Plaintext:       body.Plaintext,
		MentionEveryone: body.MentionEveryone,
		SystemType:      body.SystemType,
		SystemPayload:   body.SystemPayload,
		CreatedAt:       body.CreatedAt.UTC(),
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	msg.UpdatedAt = msg.CreatedAt
	if body.SenderDeviceID != "" {
		if did, err := id.Parse(body.SenderDeviceID); err == nil {
			msg.SenderDeviceID = did
		}
	}
	if body.ReplyTo != nil {
		if rid := h.localMessageID(c, m.RoomID, *body.ReplyTo); rid != 0 {
			msg.ReplyToID = &rid
		}
	}
	for _, fid := range body.Mentions {
		if len(msg.Mentions) >= messages.MaxMentionsPerMessage {
			break
		}
		if uid, err := h.svc.ResolveLocalID(ctx, fid); err == nil {
			msg.Mentions = append(msg.Mentions, uid)
		}
	}
	msg.SetAttachments(sanitizeAttachments(body.Attachments))
	if err := h.svc.msgSvc.CreateFederated(ctx, m.RoomID, participants, msg); err != nil {
		return fail(c, err, map[string]any{"room_id": m.RoomID})
	}
	if err := h.svc.repo.PutMessageMapping(ctx, &MessageMapping{RoomID: m.RoomID, MessageID: msg.ID, OriginDomain: requester, OriginMessageID: originMsgID}); err != nil {
		logger.Err("federation", err, map[string]any{"room_id": m.RoomID, "message_id": msg.ID})
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{"message_id": id.Format(msg.ID)})
}

// localMessageID maps a message reference to this instance's id for it (0 if unknown).
func (h *Handler) localMessageID(c fiber.Ctx, roomID int64, ref MessageRef) int64 {
	originID, err := id.Parse(ref.OriginMessageID)
	if err != nil {
		return 0
	}
	if h.svc.IsLocalServer(ref.OriginDomain) {
		return originID
	}
	if m, err := h.svc.repo.GetMessageByOrigin(c.Context(), ref.OriginDomain, originID); err == nil && m != nil && m.RoomID == roomID {
		return m.MessageID
	}
	return 0
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
	m, _, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	msgID := h.localMessageID(c, m.RoomID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	if _, err := h.svc.msgSvc.EditFederated(c.Context(), m.RoomID, msgID, body.Ciphertext, body.Plaintext); err != nil {
		return fail(c, err, map[string]any{"room_id": m.RoomID, "message_id": msgID})
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
	m, _, err := h.roomFor(c, body.Room)
	if err != nil {
		return fail(c, err, nil)
	}
	msgID := h.localMessageID(c, m.RoomID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	if err := h.svc.msgSvc.DeleteFederated(c.Context(), m.RoomID, msgID); err != nil {
		return fail(c, err, map[string]any{"room_id": m.RoomID, "message_id": msgID})
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
