package users

import (
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct{}

func NewHandler() *Handler {
	return &Handler{}
}

// Me returns the current user's info. Requires auth.
func (h *Handler) Me(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	return c.JSON(fiber.Map{
		"id":            id.Format(user.ID),
		"email":         user.Email,
		"username":      user.Username,
		"discriminator": user.Discriminator,
		"display_name":  user.DisplayName,
	})
}
