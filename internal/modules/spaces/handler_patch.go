package spaces

import (
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
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
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.JSON(spaceToJSON(space))
}
