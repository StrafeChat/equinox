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

// CreatePM creates or returns existing 1:1 PM. Body: { "recipient_id": "123" }
func (h *Handler) CreatePM(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var body struct {
		RecipientID string `json:"recipient_id"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil || body.RecipientID == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "recipient_id required"})
	}
	targetID, err := id.Parse(body.RecipientID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid recipient_id"})
	}
	room, created, err := h.svc.CreatePM(c.Context(), user.ID, targetID)
	if err != nil {
		if err.Error() == "cannot create PM with yourself" {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("rooms", err, map[string]any{"actor_id": user.ID, "target_id": targetID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if created {
		return c.Status(http.StatusCreated).JSON(roomToJSON(*room))
	}
	return c.Status(http.StatusOK).JSON(roomToJSON(*room))
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
	if !r.UpdatedAt.IsZero() {
		m["updated_at"] = r.UpdatedAt
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
