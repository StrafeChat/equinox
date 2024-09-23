package routes_v1

import (
	"log"

	"github.com/gofiber/fiber/v3"
)

func SetupRoutes(app *fiber.App) {
	log.Println("Loading V1 routes.")

	SetupAuthRoutes(app)
}
