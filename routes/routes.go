package routes

import (
	"github.com/StrafeChat/equinox/config"
	"github.com/gofiber/fiber/v3"
)

func SetupRoutes(app *fiber.App, cfg *config.Config) {
	SetupMiscRoutes(app, cfg)
	SetupAuthRoutes(app, cfg)
}
