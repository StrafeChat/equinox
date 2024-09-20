package routes_v1

import "github.com/gofiber/fiber/v3"

func SetupAuthRoutes(app *fiber.App) {
   router := app.Group("/auth");
   
   router.Post("/login", loginPost)
   router.Post("/register", registerPost)
}

func loginPost(c fiber.Ctx) error {
    return c.SendString("Hello World");
}

func registerPost(c fiber.Ctx) error {
   return c.SendString("h");
}