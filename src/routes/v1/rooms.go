package routes_v1

import (
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupRoomsRoutes(verisonRouter *fiber.Group) {
	router := verisonRouter.Group("/rooms")

	router.Use(middleware.VerifyAuth())
}
