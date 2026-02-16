package routes

import (
	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/gofiber/fiber/v3"
)

func SetupAuthRoutes(app *fiber.App, cfg *config.Config) {
	h := auth.NewHandler(cfg)

	r := app.Group("/auth")

	r.Get("/login", h.Login)
	r.Get("/register", h.Register)
}
