package rooms

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// HandleResolver turns "name#0001@domain" into a local user row (a shadow for remote
// users). Provided by internal/federation; without it only local handles resolve.
type HandleResolver interface {
	ResolveHandle(ctx context.Context, handle string) (*auth.User, error)
}

type Handler struct {
	svc      *Service
	resolver HandleResolver
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) SetHandleResolver(r HandleResolver) {
	h.resolver = r
}

// resolveHandle finds the user behind a handle. Handles with a domain need federation.
func (h *Handler) resolveHandle(c fiber.Ctx, handle string) (*auth.User, int, string) {
	handle = strings.TrimSpace(handle)
	if h.resolver != nil {
		u, err := h.resolver.ResolveHandle(c.Context(), handle)
		if err != nil {
			return nil, http.StatusNotFound, err.Error()
		}
		return u, 0, ""
	}
	name := strings.TrimSpace(strings.TrimPrefix(handle, "@"))
	if strings.Contains(name, "@") {
		return nil, http.StatusBadRequest, "this instance does not federate; use a local username"
	}
	if name == "" {
		return nil, http.StatusBadRequest, "expected a username"
	}
	u, err := h.svc.FindLocalUser(c.Context(), name)
	if err != nil {
		return nil, http.StatusInternalServerError, "internal error"
	}
	if u == nil {
		return nil, http.StatusNotFound, "user not found"
	}
	return u, 0, ""
}

// List returns rooms the current user participates in.
func (h *Handler) List(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	rooms, err := h.svc.ListRooms(c.Context(), user.ID)
	if err != nil {
		logger.Err("rooms", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := make([]fiber.Map, 0, len(rooms))
	for _, r := range rooms {
		out = append(out, roomToJSON(r))
	}
	return c.JSON(out)
}

// Get returns a room by ID (user must be participant).
func (h *Handler) Get(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	room, err := h.svc.GetRoom(c.Context(), user.ID, roomID)
	if err != nil {
		if err == ErrRoomNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
		}
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		logger.Err("rooms", err, map[string]any{"room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(roomToJSON(*room))
}

// Ack marks messages as read. POST /rooms/:id/ack. Body: { "message_id": "123" }. Returns 204.
func (h *Handler) Ack(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var body struct {
		MessageID  string `json:"message_id"`
		LastReadID string `json:"last_read_message_id"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	ackID := strings.TrimSpace(body.LastReadID)
	if ackID == "" {
		ackID = strings.TrimSpace(body.MessageID)
	}
	if ackID == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "last_read_message_id required"})
	}
	msgID, err := id.Parse(ackID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid last_read_message_id"})
	}
	if err := h.svc.Ack(c.Context(), user.ID, roomID, msgID); err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		logger.Err("rooms", err, map[string]any{"user_id": user.ID, "room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// SetNotifySettings updates the caller's own mute/notify-mode override for a room.
// PATCH /rooms/:id/notify-settings. Body: { "muted"?: bool, "muted_until"?: string|null,
// "notify_mode"?: 0-3 }. Any field not present keeps its current value.
func (h *Handler) SetNotifySettings(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	// Decoded as raw fields (not a plain struct with *string) so a JSON `null` for
	// muted_until ("clear the timed mute") can be told apart from the key being absent
	// ("leave it alone") - a plain *string field unmarshals both to nil, which previously
	// meant PATCHing {"muted_until": null} silently kept the old value forever.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(c.Body(), &raw); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	row, err := h.svc.GetUserRoomRow(c.Context(), user.ID, roomID)
	if err != nil {
		logger.Err("rooms", err, map[string]any{"user_id": user.ID, "room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	muted, mutedUntil, notifyMode := false, (*time.Time)(nil), NotifyModeDefault
	if row != nil {
		muted, mutedUntil, notifyMode = row.Muted, row.MutedUntil, row.NotifyMode
	}
	if v, ok := raw["muted"]; ok {
		if err := json.Unmarshal(v, &muted); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid muted"})
		}
	}
	if v, ok := raw["muted_until"]; ok {
		var s *string
		if err := json.Unmarshal(v, &s); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid muted_until"})
		}
		if s == nil || *s == "" {
			mutedUntil = nil
		} else {
			parsed, err := time.Parse(time.RFC3339, *s)
			if err != nil {
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid muted_until"})
			}
			mutedUntil = &parsed
		}
	}
	if v, ok := raw["notify_mode"]; ok {
		if err := json.Unmarshal(v, &notifyMode); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid notify_mode"})
		}
	}
	if err := h.svc.SetRoomNotifySettings(c.Context(), user.ID, roomID, muted, mutedUntil, notifyMode); err != nil {
		switch err {
		case ErrRoomNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
		case ErrNotParticipant:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		case ErrInvalidNotifyMode:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("rooms", err, map[string]any{"user_id": user.ID, "room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// Typing triggers TYPING_START for the room. POST /rooms/:id/typing. Returns 204.
func (h *Handler) Typing(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	if err := h.svc.Typing(c.Context(), user.ID, roomID); err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		logger.Err("rooms", err, map[string]any{"user_id": user.ID, "room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// GetNotes returns the current user's notes room (self-PM), creating it if it doesn't exist.
// GET /rooms/notes
func (h *Handler) GetNotes(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	room, _, err := h.svc.CreatePM(c.Context(), user.ID, user.ID)
	if err != nil {
		logger.Err("rooms", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(roomToJSON(*room))
}

// CreatePM creates a 1:1 PM or group PM. Body: { "recipient_id": "123" } for DM, or { "recipient_ids": ["1","2"] } for group (optional "name").
func (h *Handler) CreatePM(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var body struct {
		RecipientID  string   `json:"recipient_id"`
		Name         string   `json:"name"`
		RecipientIDs []string `json:"recipient_ids"`
		// Handles ("name#0001" or "name#0001@other.instance") as an alternative to ids -
		// the way to reach someone on another instance for the first time.
		RecipientHandle  string   `json:"recipient_handle"`
		RecipientHandles []string `json:"recipient_handles"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	if body.RecipientID == "" && body.RecipientHandle != "" {
		target, status, msg := h.resolveHandle(c, body.RecipientHandle)
		if target == nil {
			return c.Status(status).JSON(fiber.Map{"error": msg})
		}
		body.RecipientID = id.Format(target.ID)
	}
	for _, handle := range body.RecipientHandles {
		target, status, msg := h.resolveHandle(c, handle)
		if target == nil {
			return c.Status(status).JSON(fiber.Map{"error": msg})
		}
		body.RecipientIDs = append(body.RecipientIDs, id.Format(target.ID))
	}
	if body.RecipientID != "" {
		targetID, err := id.Parse(body.RecipientID)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid recipient_id"})
		}
		room, created, err := h.svc.CreatePM(c.Context(), user.ID, targetID)
		if err != nil {
			if err == ErrBlocked || err == ErrPMNotAllowed {
				return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
			}
			if err == ErrUserNotFound {
				return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
			}
			logger.Err("rooms", err, map[string]any{"actor_id": user.ID, "target_id": targetID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
		if created {
			return c.Status(http.StatusCreated).JSON(roomToJSON(*room))
		}
		return c.Status(http.StatusOK).JSON(roomToJSON(*room))
	}
	if len(body.RecipientIDs) > 0 {
		ids := make([]int64, 0, len(body.RecipientIDs))
		for _, s := range body.RecipientIDs {
			parsed, err := id.Parse(s)
			if err != nil {
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid recipient_ids"})
			}
			ids = append(ids, parsed)
		}
		name := strings.TrimSpace(body.Name)
		room, err := h.svc.CreateGroupPM(c.Context(), user.ID, name, ids)
		if err != nil {
			switch err {
			case ErrBlocked, ErrPMNotAllowed:
				return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
			case ErrMinParticipants, ErrTooManyParticipants, ErrInvalidName:
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
			case ErrUserNotFound:
				return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
			default:
				logger.Err("rooms", err, map[string]any{"actor_id": user.ID})
				return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
			}
		}
		return c.Status(http.StatusCreated).JSON(roomToJSON(*room))
	}
	return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "recipient_id or (name and recipient_ids) required"})
}

// AddParticipant adds a user to a group PM. POST /rooms/:id/participants. Body: { "user_id": "123" }
func (h *Handler) AddParticipant(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var body struct {
		UserID string `json:"user_id"`
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil || (body.UserID == "" && body.Handle == "") {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "user_id or handle required"})
	}
	if body.UserID == "" {
		target, status, msg := h.resolveHandle(c, body.Handle)
		if target == nil {
			return c.Status(status).JSON(fiber.Map{"error": msg})
		}
		body.UserID = id.Format(target.ID)
	}
	targetID, err := id.Parse(body.UserID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user_id"})
	}
	if err := h.svc.AddParticipant(c.Context(), user.ID, roomID, targetID); err != nil {
		switch err {
		case ErrBlocked, ErrPMNotAllowed:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		case ErrRoomNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
		case ErrNotParticipant:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		case ErrNotGroupRoom:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "not a group room"})
		case ErrAlreadyInGroup:
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "user already in group"})
		case ErrTooManyParticipants:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		case ErrUserNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
		default:
			logger.Err("rooms", err, map[string]any{"room_id": roomID, "target_id": targetID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// RemoveParticipant removes a user from a group. DELETE /rooms/:id/participants/:user_id. Only group creator.
func (h *Handler) RemoveParticipant(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	targetID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user_id"})
	}
	if err := h.svc.RemoveParticipant(c.Context(), user.ID, roomID, targetID); err != nil {
		switch err {
		case ErrRoomNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
		case ErrNotParticipant:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		case ErrNotGroupRoom:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "not a group room"})
		case ErrNotCreator:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "only the group creator can remove members"})
		case ErrCannotRemoveSelf:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "creator cannot remove themselves"})
		case ErrMinParticipants:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "group must have at least 2 participants"})
		default:
			logger.Err("rooms", err, map[string]any{"room_id": roomID, "target_id": targetID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// UpdateRoom updates room properties. PATCH /rooms/:id. Body: { "name": "...", "e2ee_enabled": true|false }. Only group creator.
func (h *Handler) UpdateRoom(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var body struct {
		Name        string `json:"name"`
		E2EEEnabled *bool  `json:"e2ee_enabled"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	if body.Name != "" {
		name := strings.TrimSpace(body.Name)
		if name == "" {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "name cannot be empty"})
		}
		if err := h.svc.UpdateRoomName(c.Context(), user.ID, roomID, name); err != nil {
			switch err {
			case ErrRoomNotFound:
				return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
			case ErrNotParticipant:
				return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
			case ErrNotGroupRoom:
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "not a group room"})
			case ErrNotCreator:
				return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "only the group creator can rename the group"})
			case ErrInvalidName:
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
			default:
				logger.Err("rooms", err, map[string]any{"room_id": roomID})
				return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
			}
		}
	}
	if body.E2EEEnabled != nil {
		if err := h.svc.UpdateRoomE2EEEnabled(c.Context(), user.ID, roomID, *body.E2EEEnabled); err != nil {
			switch err {
			case ErrRoomNotFound:
				return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
			case ErrNotParticipant:
				return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
			case ErrNotGroupRoom:
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "not a group room"})
			case ErrNotCreator:
				return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "only the group creator can change this setting"})
			default:
				logger.Err("rooms", err, map[string]any{"room_id": roomID})
				return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
			}
		}
	}
	room, _ := h.svc.GetRoom(c.Context(), user.ID, roomID)
	if room != nil {
		return c.JSON(roomToJSON(*room))
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

func roomToJSON(r RoomWithParticipants) fiber.Map {
	m := fiber.Map{
		"id":         id.Format(r.ID),
		"type":       r.Type,
		"recipients": recipientIDsToJSON(r.ParticipantIDs),
		"created_at": r.CreatedAt,
	}
	if r.SpaceID != nil {
		m["space_id"] = id.Format(*r.SpaceID)
	}
	if r.ParentID != nil {
		m["parent_id"] = id.Format(*r.ParentID)
	}
	if r.Name != "" {
		m["name"] = r.Name
	}
	if r.Topic != "" {
		m["topic"] = r.Topic
	}
	if r.Position != 0 {
		m["position"] = r.Position
	}
	if r.LastMessageID != nil {
		m["last_message_id"] = id.Format(*r.LastMessageID)
	}
	if r.LastReadMessageID != nil {
		m["last_read_message_id"] = id.Format(*r.LastReadMessageID)
	}
	if r.MentionCount > 0 {
		m["mention_count"] = r.MentionCount
	}
	if !r.UpdatedAt.IsZero() {
		m["updated_at"] = r.UpdatedAt
	}
	// Always include creator_id for group rooms so the client can show owner crown and settings
	if r.Type == TypeGroupPM {
		m["creator_id"] = id.Format(r.CreatorID)
	} else if r.CreatorID != 0 {
		m["creator_id"] = id.Format(r.CreatorID)
	}
	e2eeEnabled := true
	if r.E2EEEnabled != nil {
		e2eeEnabled = *r.E2EEEnabled
	}
	m["e2ee_enabled"] = e2eeEnabled
	if len(r.Participants) > 0 {
		m["participants"] = r.Participants
	}
	if r.Federation != nil {
		m["federation"] = r.Federation
	}
	if r.Muted {
		m["muted"] = true
	}
	if r.MutedUntil != nil {
		m["muted_until"] = r.MutedUntil
	}
	if r.NotifyMode != 0 {
		m["notify_mode"] = r.NotifyMode
	}
	return m
}

func recipientIDsToJSON(ids []int64) []string {
	out := make([]string, len(ids))
	for i, uid := range ids {
		out[i] = id.Format(uid)
	}
	return out
}
