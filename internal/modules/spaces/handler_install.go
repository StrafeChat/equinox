package spaces

import (
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// MySpaces GET /users/@me/spaces - the light list of the caller's spaces (Discord's
// /users/@me/guilds): id, name, icon, whether they own it, and their space-wide
// permissions. This is the one spaces endpoint an OAuth2 token may call (with the
// `spaces` scope); sessions and bots get it too.
func (h *Handler) MySpaces(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	list, err := h.svc.ListSpaceSummaries(c.Context(), user.ID)
	if err != nil {
		return spaceError(c, err, map[string]any{"user_id": user.ID})
	}
	out := make([]fiber.Map, 0, len(list))
	for _, sm := range list {
		out = append(out, fiber.Map{
			"id":           id.Format(sm.Space.ID),
			"name":         sm.Space.Name,
			"name_acronym": sm.Space.NameAcronym,
			"icon":         sm.Space.Icon,
			"banner":       sm.Space.Banner,
			"owner":        sm.Owner,
			"permissions":  sm.Permissions,
		})
	}
	return c.JSON(out)
}
