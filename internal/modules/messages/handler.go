package messages

import (
	"encoding/json"
	"net/http"
	"strconv"

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

// Create handles POST /rooms/:id/messages
func (h *Handler) Create(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var in CreateMessageInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	if in.Ciphertext == "" && in.Plaintext == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "ciphertext or plaintext required"})
	}
	msg, err := h.svc.Create(c.Context(), user.ID, roomID, &in)
	if err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to send messages in this channel"})
		}
		if err == ErrInvalidInput {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "plaintext required when E2EE is off; ciphertext required when E2EE is on"})
		}
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusCreated).JSON(messageToJSON(msg))
}

// Get handles GET /rooms/:id/messages/:msg_id
func (h *Handler) Get(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	msg, err := h.svc.Get(c.Context(), user.ID, roomID, msgID)
	if err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to read this channel"})
		}
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if msg == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	return c.JSON(messageToJSON(msg))
}

// List handles GET /rooms/:id/messages?before=id&limit=50
func (h *Handler) List(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var beforeID *int64
	if b := c.Query("before"); b != "" {
		bid, err := id.Parse(b)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid before"})
		}
		beforeID = &bid
	}
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	msgs, err := h.svc.List(c.Context(), user.ID, roomID, beforeID, limit)
	if err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to read this channel"})
		}
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := make([]fiber.Map, len(msgs))
	for i := range msgs {
		out[i] = messageToJSON(&msgs[i])
	}
	return c.JSON(out)
}

// Edit handles PATCH /rooms/:id/messages/:msg_id
func (h *Handler) Edit(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	var in EditMessageInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	if in.Ciphertext == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "ciphertext required"})
	}
	msg, err := h.svc.Edit(c.Context(), user.ID, roomID, msgID, &in)
	if err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrMessageNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "cannot edit this message"})
		}
		logger.Err("messages", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(messageToJSON(msg))
}

// Delete handles DELETE /rooms/:id/messages/:msg_id
func (h *Handler) Delete(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	if err := h.svc.Delete(c.Context(), user.ID, roomID, msgID); err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrMessageNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "cannot delete this message"})
		}
		logger.Err("messages", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

func messageToJSON(m *Message) fiber.Map {
	out := fiber.Map{
		"room_id":          id.Format(m.RoomID),
		"id":               id.Format(m.ID),
		"sender_id":        id.Format(m.SenderID),
		"sender_device_id": id.Format(m.SenderDeviceID),
		"ciphertext":       m.Ciphertext,
		"created_at":       m.CreatedAt,
		"updated_at":       m.UpdatedAt,
	}
	if m.Plaintext != "" {
		out["plaintext"] = m.Plaintext
	}
	if m.ReplyToID != nil {
		out["reply_to_id"] = id.Format(*m.ReplyToID)
	}
	if m.DeletedAt != nil && !m.DeletedAt.IsZero() {
		out["deleted_at"] = m.DeletedAt
	}
	if m.SystemType != "" {
		out["system_type"] = m.SystemType
		out["system_payload"] = m.SystemPayload
	}
	return out
}
