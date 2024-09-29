package handlers_v1

import (
	"log"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/src/types"
)

func RegisterPost(c fiber.Ctx) error {
	body := new(types.RegisterBody)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid JSON format."})
	}

	log.Println(body.Email)

	return c.SendStatus(200)
}
