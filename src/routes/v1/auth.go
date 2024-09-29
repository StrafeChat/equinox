package routes_v1

import (
	"github.com/gofiber/fiber/v3"

	authHandler "github.com/StrafeChat/equinox/src/handlers/v1/auth"
)

func SetupAuthRoutes(app *fiber.App) {
	router := app.Group("/auth")

	router.Post("/login", authHandler.LoginPost)
	router.Post("/register", authHandler.RegisterPost)
}
