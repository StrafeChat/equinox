package auth

import (
	"github.com/StrafeChat/equinox/internal/config"
	"github.com/gofiber/fiber/v3"
)

type Handler struct {
	cfg *config.Config
}

func NewHandler(cfg *config.Config) *Handler {
	return &Handler{cfg: cfg}
}

func (h *Handler) Login(c fiber.Ctx) error {
	return c.SendString("Login route")
}

func (h *Handler) Register(c fiber.Ctx) error {
	if h.cfg.Flags.InviteOnly {
		return c.Status(fiber.StatusForbidden).
			SendString("Invite-only mode enabled")
	}

	return c.SendString("Register route")
}
