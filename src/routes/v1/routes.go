package routes_v1

import "github.com/gofiber/fiber/v3"

func SetupRoutes(app *fiber.App) {
   SetupAuthRoutes(app);
}