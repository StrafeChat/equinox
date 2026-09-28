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

// extractToken reads the session token from the Authorization header, and only from
// there.
//
// A ?token= query parameter is deliberately not accepted: URLs end up in proxy logs,
// browser history and Referer headers, so a token there leaks. Neither is a cookie: nothing
// on the server ever sets one, the client authenticates with a Bearer header, and honouring
// a cookie that is never issued would only add an ambient-credential path - one that, with
// credentialed CORS enabled, turns any future cookie into a cross-site request forgery
// surface with no SameSite or CSRF-token check behind it. The WebSocket gateway (which
// browsers cannot send headers to) has its own extractor.
func extractToken(c fiber.Ctx) string {
	if h := c.Get("Authorization"); h != "" {
		if prefix := "Bearer "; strings.HasPrefix(h, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(h, prefix))
		}
	}
	return ""
}
