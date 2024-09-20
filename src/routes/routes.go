package routes

import (
	"fmt"

	routes_v1 "github.com/StrafeChat/equinox/src/routes/v1"
	"github.com/gofiber/fiber/v3"
)

func SetupRoutes(app *fiber.App) {
	fmt.Println("Setting up routes...")

	                     routes_v1.SetupRoutes(app);
}