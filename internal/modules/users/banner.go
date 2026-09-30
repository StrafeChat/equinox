package users

import (
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// PostBanner accepts multipart field "file", uploads to Nebula, sets user banner URL, broadcasts USER_UPDATE.
func (h *Handler) PostBanner(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	updated := h.uploadProfileImage(c, user, imageBanner)
	if updated == nil {
		return nil
	}
	return c.JSON(ownProfileJSON(updated))
}
