package misc

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

type Feature struct {
	Enabled bool `json:"enabled"`
}

type Features struct {
	Captcha    Feature `json:"captcha"`
	Email      Feature `json:"email"`
	InviteOnly Feature `json:"invite_only"`
}

type Response struct {
	Strafe   Strafe   `json:"strafe"`
	Features Features `json:"features"`
}

type Strafe struct {
	Version string `json:"version"`
}

func (h *Handler) Index(c fiber.Ctx) error {
	return c.JSON(Response{
		Strafe: Strafe{
			Version: h.cfg.App.Version,
		},
		Features: Features{
			Captcha:    Feature{Enabled: h.cfg.Flags.Captcha},
			Email:      Feature{Enabled: h.cfg.Flags.Email},
			InviteOnly: Feature{Enabled: h.cfg.Flags.InviteOnly},
		},
	})
}
