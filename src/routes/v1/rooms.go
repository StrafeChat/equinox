package routes_v1

import (
	handlers_v1 "github.com/StrafeChat/equinox/src/handlers/v1/rooms"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupRoomsRoutes(verisonRouter *fiber.Group) {
	router := verisonRouter.Group("/rooms")

	router.Use(middleware.VerifyAuth())

	router.Post("/:id/messages", handlers_v1.CreateMessage)
	router.Get("/:id/messages", handlers_v1.GetRoomMessages)
}
