package handlers_v1

import (
	"log"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/src/types"
)

func LoginPost(c fiber.Ctx) error {
	body := new(types.LoginBody)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid JSON format."})
	}

	if body.Email == "" || body.Password == "" {
		return c.Status(400).JSON(fiber.Map{"error": "You need to provide an email and password."})
	}

	log.Printf("Email: %s, Password: %s", body.Email, body.Password)

	return c.JSON(fiber.Map{
		"email":    body.Email,
		"password": body.Password,
	})
}
