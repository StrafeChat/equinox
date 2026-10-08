package messages

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// Pin handles PUT /rooms/:id/messages/:msg_id/pin - pin for everyone in the room.
func (h *Handler) Pin(c fiber.Ctx) error { return h.setPinned(c, true) }

// Unpin handles DELETE /rooms/:id/messages/:msg_id/pin.
func (h *Handler) Unpin(c fiber.Ctx) error { return h.setPinned(c, false) }

func (h *Handler) setPinned(c fiber.Ctx, pinned bool) error {
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
	if pinned {
		_, err = h.svc.Pin(c.Context(), user.ID, roomID, msgID)
	} else {
		err = h.svc.Unpin(c.Context(), user.ID, roomID, msgID)
	}
	if err != nil {
		return pinError(c, err, roomID, "missing permission to pin messages in this channel")
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListPins handles GET /rooms/:id/pins - the room's pinned messages, newest pin first:
// {"items": [{"pinned_at", "pinned_by", "message"}], "has_more": false}.
func (h *Handler) ListPins(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	pins, err := h.svc.ListPins(c.Context(), user.ID, roomID)
	if err != nil {
		return pinError(c, err, roomID, "missing permission to read this channel")
	}
	items := make([]fiber.Map, 0, len(pins))
	for i := range pins {
		p := &pins[i]
		items = append(items, fiber.Map{
			"pinned_at": p.PinnedAt.UTC(),
			"pinned_by": id.Format(p.PinnedBy),
			"message":   h.svc.MessageJSON(c.Context(), &p.Message, p.Reactions),
		})
	}
	return c.JSON(fiber.Map{"items": items, "has_more": false})
}

func pinError(c fiber.Ctx, err error, roomID int64, forbidden string) error {
	if res, ok := originError(c, err); ok {
		return res
	}
	switch {
	case errors.Is(err, ErrNotParticipant):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
	case errors.Is(err, ErrThreadLocked), errors.Is(err, ErrThreadArchived):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrForbidden):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": forbidden})
	case errors.Is(err, ErrRoomNotFound), errors.Is(err, ErrMessageNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrTooManyPins):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	logger.Err("messages", err, map[string]any{"room_id": roomID, "action": "pin"})
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}
