package routes

import (
	"github.com/StrafeChat/equinox/config"
	"github.com/gofiber/fiber/v3"
)

func SetupAuthRoutes(app *fiber.App, cfg *config.Config) {
	router := app.Group("/auth")
	router.Get("/login", func(c fiber.Ctx) error {
		return c.SendString("Login page")
	})
	router.Get("/register", func(c fiber.Ctx) error {
		return c.SendString("Register page")
	})
}
