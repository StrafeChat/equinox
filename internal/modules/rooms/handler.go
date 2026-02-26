package rooms

import (
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
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
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil || body.MessageID == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "message_id required"})
	}
	msgID, err := id.Parse(body.MessageID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message_id"})
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

// CreatePM creates a 1:1 PM or group PM. Body: { "recipient_id": "123" } for DM, or { "name": "Friends", "recipient_ids": ["1","2"] } for group.
func (h *Handler) CreatePM(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var body struct {
		RecipientID  string   `json:"recipient_id"`
		Name         string   `json:"name"`
		RecipientIDs []string `json:"recipient_ids"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	if body.RecipientID != "" {
		targetID, err := id.Parse(body.RecipientID)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid recipient_id"})
		}
		room, created, err := h.svc.CreatePM(c.Context(), user.ID, targetID)
		if err != nil {
			logger.Err("rooms", err, map[string]any{"actor_id": user.ID, "target_id": targetID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
		if created {
			return c.Status(http.StatusCreated).JSON(roomToJSON(*room))
		}
		return c.Status(http.StatusOK).JSON(roomToJSON(*room))
	}
	if body.Name != "" && len(body.RecipientIDs) > 0 {
		ids := make([]int64, 0, len(body.RecipientIDs))
		for _, s := range body.RecipientIDs {
			parsed, err := id.Parse(s)
			if err != nil {
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid recipient_ids"})
			}
			ids = append(ids, parsed)
		}
		room, err := h.svc.CreateGroupPM(c.Context(), user.ID, body.Name, ids)
		if err != nil {
			switch err {
			case ErrMinParticipants:
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
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
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil || body.UserID == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "user_id required"})
	}
	targetID, err := id.Parse(body.UserID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user_id"})
	}
	if err := h.svc.AddParticipant(c.Context(), user.ID, roomID, targetID); err != nil {
		switch err {
		case ErrRoomNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
		case ErrNotParticipant:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		case ErrNotGroupRoom:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "not a group room"})
		case ErrAlreadyInGroup:
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "user already in group"})
		default:
			logger.Err("rooms", err, map[string]any{"room_id": roomID, "target_id": targetID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

func roomToJSON(r RoomWithParticipants) fiber.Map {
	m := fiber.Map{
		"id":   id.Format(r.ID),
		"type": r.Type,
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
	if len(r.Participants) > 0 {
		m["participants"] = r.Participants
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
