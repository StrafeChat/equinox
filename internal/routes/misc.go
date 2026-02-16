package routes

import (
	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/misc"
	"github.com/gofiber/fiber/v3"
)

func SetupMiscRoutes(app *fiber.App, cfg *config.Config) {
	h := misc.NewHandler(cfg)

	app.Get("/", h.Index)
}
