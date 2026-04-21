package spaces

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// ReorderRooms POST /spaces/:id/rooms/reorder — body: ReorderRoomsInput (registered before generic /rooms routes).
func (h *Handler) ReorderRooms(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var body ReorderRoomsInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if err := h.svc.ReorderRooms(c.Context(), user.ID, spaceID, &body); err != nil {
		if errors.Is(err, ErrMissingPerm) || errors.Is(err, ErrNotMember) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if errors.Is(err, ErrSpaceNotFound) {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "space not found"})
		}
		if errors.Is(err, ErrInvalidReorder) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.SendStatus(http.StatusNoContent)
}
