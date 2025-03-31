package middleware

import (
	"os"
	"strconv"

	"github.com/gofiber/fiber/v3"
)

// CloudflareIP is a middleware that extracts the real client IP from Cloudflare headers
// It checks for the CF-Connecting-IP header first, then falls back to X-Forwarded-For,
// and finally uses the default IP detection method
// The behavior can be controlled with the ENABLE_CLOUDFLARE_IP environment variable
func CloudflareIP() fiber.Handler {
	return func(c fiber.Ctx) error {
		// Check if Cloudflare IP detection is enabled
		enableCloudflareIP := getEnvBool("ENABLE_CLOUDFLARE_IP", true)
		if enableCloudflareIP {
			// Check for Cloudflare-specific header first
			if cfIP := c.Get("CF-Connecting-IP"); cfIP != "" {
				c.Request().Header.Set("X-Real-IP", cfIP) // Override IP for Fiber
				c.Locals("original_ip", cfIP)
				return c.Next()
			}

			// Fall back to X-Forwarded-For header
			if forwardedIP := c.Get("X-Forwarded-For"); forwardedIP != "" {
				c.Request().Header.Set("X-Real-IP", forwardedIP)
				c.Locals("original_ip", forwardedIP)
				return c.Next()
			}
		}

		// If Cloudflare IP detection is disabled or no special headers are found, store the default IP
		c.Request().Header.Set("X-Real-IP", c.IP())
		c.Locals("original_ip", c.IP())
		return c.Next()
	}
}

// GetRealIP returns the real client IP from the request context
// This should be used instead of c.IP() in handlers that need the real client IP
func GetRealIP(c fiber.Ctx) string {
	if ip, ok := c.Locals("original_ip").(string); ok && ip != "" {
		return ip
	}
	return c.IP()
}

// getEnvBool reads a boolean environment variable
// If the variable is not set or cannot be parsed, it returns the default value
func getEnvBool(key string, defaultValue bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}

	boolVal, err := strconv.ParseBool(val)
	if err != nil {
		return defaultValue
	}

	return boolVal
}
