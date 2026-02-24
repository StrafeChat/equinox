package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// RequireAuth returns a handler that validates the session token (Bearer or cookie),
// loads the user and session into Locals, or returns 401.
func RequireAuth(sessionRepo auth.SessionRepository, userRepo auth.UserRepository) fiber.Handler {
	return func(c fiber.Ctx) error {
		token := extractToken(c)
		if token == "" {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "missing or invalid authorization"})
		}

		raw, err := hex.DecodeString(token)
		if err != nil || len(raw) == 0 {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token format"})
		}

		hash := sha256.Sum256(raw)
		tokenHash := hex.EncodeToString(hash[:])

		sess, err := sessionRepo.GetByTokenHash(c.Context(), tokenHash)
		if err != nil {
			logger.Err("auth", err, map[string]any{"path": c.Path()})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
		if sess == nil {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid or expired session"})
		}

		if !sess.RevokedAt.IsZero() {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "session revoked"})
		}
		if time.Now().UTC().After(sess.ExpiresAt) {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "session expired"})
		}

		u, err := userRepo.GetByID(c.Context(), sess.UserID)
		if err != nil {
			logger.Err("auth", err, map[string]any{"user_id": sess.UserID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
		if u == nil {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "user not found"})
		}

		c.Locals(auth.LocalsKeyUser, u)
		c.Locals(auth.LocalsKeySession, sess)
		return c.Next()
	}
}

func extractToken(c fiber.Ctx) string {
	// Authorization: Bearer <token>
	if h := c.Get("Authorization"); h != "" {
		if prefix := "Bearer "; strings.HasPrefix(h, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(h, prefix))
		}
	}
	// Cookie (optional, same-origin WebSocket)
	if t := c.Cookies("session_token"); t != "" {
		return t
	}
	// Query param (for WebSocket from clients that can't set headers)
	if t := c.Query("token"); t != "" {
		return t
	}
	return ""
}
