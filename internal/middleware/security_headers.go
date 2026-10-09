package middleware

import (
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/weborigin"
)

func SecurityHeaders() fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("X-Frame-Options", "DENY")
		// "0" is the current recommendation: the legacy XSS auditor this header enabled has
		// been removed from every modern browser, and in the ones that still had it,
		// "1; mode=block" could itself be abused to leak information.
		c.Set("X-XSS-Protection", "0")
		c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		return c.Next()
	}
}

// CORS returns a CORS handler. If origins is nil/empty, allows any origin (dev).
//
// Vary: Origin is set unconditionally so a shared cache never serves one origin's
// Access-Control-Allow-Origin to another.
func CORS(origins []string) fiber.Handler {
	return func(c fiber.Ctx) error {
		origin := c.Get("Origin")
		c.Set("Vary", "Origin")
		if len(origins) == 0 && origin != "" {
			c.Set("Access-Control-Allow-Origin", origin)
		} else if weborigin.IsDesktop(origin) {
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
		// X-MFA-Token: the pending-login token for the two WebAuthn login calls (see
		// auth/handler_2fa.go) - kept out of the body/query so it never lands in a proxy
		// log, which means it travels as a header and needs to clear preflight like any
		// other non-simple one.
		c.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-MFA-Token")
		c.Set("Access-Control-Allow-Credentials", "true")

		if c.Method() == http.MethodOptions {
			return c.SendStatus(http.StatusNoContent)
		}
		return c.Next()
	}
}
