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

// PatchSpace PATCH /spaces/:id — update space metadata (e.g. name).
func (h *Handler) PatchSpace(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var body PatchSpaceInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	space, err := h.svc.PatchSpace(c.Context(), user.ID, spaceID, &body)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a member of this space"})
		}
		if errors.Is(err, ErrInsufficientSpacePermission) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if errors.Is(err, ErrSpaceNotFound) {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "space not found"})
		}
		if errors.Is(err, ErrNothingToPatch) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "no fields to update"})
		}
		if errors.Is(err, ErrInvalidSpaceName) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid name"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(spaceToJSON(space))
}
