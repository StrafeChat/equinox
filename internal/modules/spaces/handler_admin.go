package spaces

import (
	"net/http"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/gofiber/fiber/v3"
)

// TransferOwnership POST /spaces/:id/transfer-ownership. Body: { "user_id": "..." }.
// Owner only; the new owner must already be a member.
func (h *Handler) TransferOwnership(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var body struct {
		UserID string `json:"user_id"`
	}
	if err := c.Bind().Body(&body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	newOwnerID, err := id.Parse(body.UserID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	space, err := h.svc.TransferOwnership(c.Context(), user.ID, spaceID, newOwnerID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID, "new_owner_id": newOwnerID})
	}
	return c.JSON(spaceToJSON(space))
}

// DeleteSpace DELETE /spaces/:id. Body: { "name": "..." } - the space's own name, typed
// back as confirmation. Owner only, and irreversible.
func (h *Handler) DeleteSpace(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var body struct {
		Name string `json:"name"`
	}
	// An empty body is fine to parse; the name check below is what rejects it.
	_ = c.Bind().Body(&body)
	if err := h.svc.DeleteSpace(c.Context(), user.ID, spaceID, body.Name); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}
