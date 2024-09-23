package handlers_v1

import "github.com/gofiber/fiber/v3"

/*_ Register Route Handler _*/
func RegisterPost(c fiber.Ctx) error {
	return c.SendString("Route in construction")
}
