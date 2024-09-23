package routes_v1

import (
	"github.com/gofiber/fiber/v3"

	handlers_v1 "github.com/StrafeChat/equinox/src/handlers/v1/auth"
)

func SetupAuthRoutes(app *fiber.App) {
	router := app.Group("/auth")

	router.Post("/login", handlers_v1.LoginPost)
	router.Post("/register", handlers_v1.RegisterPost)
}
