package routes

import (
	"github.com/StrafeChat/equinox/config"
	"github.com/gofiber/fiber/v3"
)

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

func SetupMiscRoutes(app *fiber.App, cfg *config.Config) {
	app.Get("/", func(c fiber.Ctx) error {
		return c.JSON(Response{
			Strafe: Strafe{
				Version: cfg.Version,
			},
			Features: Features{
				Captcha:    Feature{Enabled: cfg.Captcha},
				Email:      Feature{Enabled: cfg.Email},
				InviteOnly: Feature{Enabled: cfg.InviteOnly},
			},
		})
	})
}
