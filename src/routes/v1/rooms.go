package routes_v1

import (
	handlers_v1 "github.com/StrafeChat/equinox/src/handlers/v1/rooms"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupRoomsRoutes(app *fiber.App) {
	app.Use(middleware.VerifyAuth())
	app.Post("/rooms", handlers_v1.CreateRoom)
}
