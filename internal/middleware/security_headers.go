package middleware

import (
	"net/http"

	"github.com/gofiber/fiber/v3"
)

func SecurityHeaders() fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("X-Frame-Options", "DENY")
		c.Set("X-XSS-Protection", "1; mode=block")
		c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		return c.Next()
	}
}

// CORS returns a CORS handler. If origins is nil/empty, allows any origin (dev).
func CORS(origins []string) fiber.Handler {
	return func(c fiber.Ctx) error {
		origin := c.Get("Origin")
		if len(origins) == 0 && origin != "" {
			c.Set("Access-Control-Allow-Origin", origin)
		} else if len(origins) > 0 {
			for _, o := range origins {
				if o == origin {
					c.Set("Access-Control-Allow-Origin", origin)
					break
				}
			}
		}
		c.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
		c.Set("Access-Control-Allow-Credentials", "true")

		if c.Method() == http.MethodOptions {
			return c.SendStatus(http.StatusNoContent)
		}
		return c.Next()
	}
}
