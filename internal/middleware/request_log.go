package middleware

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
)

// RequestLog logs each HTTP request when LOG_LEVEL is info or debug.
// In production, set LOG_LEVEL=off to disable.
func RequestLog() fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		status := c.Response().StatusCode()
		logger.Request(c.Method(), c.Path(), status, time.Since(start))
		return err
	}
}
