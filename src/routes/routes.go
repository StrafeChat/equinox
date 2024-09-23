package routes

import (
	"log"

	"github.com/gofiber/fiber/v3"

	routes_v1 "github.com/StrafeChat/equinox/src/routes/v1"
)

func SetupRoutes(app *fiber.App) {
	log.Println("Loading routes.")

	/*_ Load v1 routes _*/
	routes_v1.SetupRoutes(app)
}
