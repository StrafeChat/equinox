package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// BotResolver returns the bot account for a raw `Bot <token>` value, or nil if the token is
// not a valid bot token. OAuthResolver does the same for a raw OAuth2 bearer access token,
// also returning the token's granted scopes. Both are optional and injected at startup, so
// the middleware stays decoupled from the applications/oauth modules (which import auth).
type BotResolver func(ctx context.Context, rawToken string) (*auth.User, error)
type OAuthResolver func(ctx context.Context, rawToken string) (*auth.User, []string, error)

var (
	botResolver   BotResolver
	oauthResolver OAuthResolver
)

func SetBotResolver(r BotResolver)     { botResolver = r }
func SetOAuthResolver(r OAuthResolver) { oauthResolver = r }

// LocalsKeyScopes holds the OAuth2 scopes ([]string) a request was authorised with. Absent
// for session and bot auth, which are unscoped (full account / full bot access).
const LocalsKeyScopes = "oauth_scopes"

// RequireAuth returns a handler that authenticates the request and loads the user (and, for
// a session, the session) into Locals, or returns 401. It accepts three credentials:
// `Authorization: Bearer <session token>`, `Bearer <oauth token>`, and `Bot <bot token>`.
func RequireAuth(sessionRepo auth.SessionRepository, userRepo auth.UserRepository) fiber.Handler {
	return func(c fiber.Ctx) error {
		scheme, token := extractAuth(c)
		if token == "" {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "missing or invalid authorization"})
		}

		// Bot tokens are a distinct scheme and never touch the session store.
		if scheme == "Bot" {
			if botResolver == nil {
				return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "bot tokens are not enabled"})
			}
			u, err := botResolver(c.Context(), token)
			if err != nil {
				logger.Err("auth", err, map[string]any{"path": c.Path(), "scheme": "bot"})
				return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
			}
			if u == nil {
				return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid bot token"})
			}
			c.Locals(auth.LocalsKeyUser, u)
			return c.Next()
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
			// Not a session token - it may be an OAuth2 access token (same Bearer scheme).
			if oauthResolver != nil {
				u, scopes, oerr := oauthResolver(c.Context(), token)
				if oerr != nil {
					logger.Err("auth", oerr, map[string]any{"path": c.Path(), "scheme": "oauth"})
					return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
				}
				if u != nil {
					c.Locals(auth.LocalsKeyUser, u)
					c.Locals(LocalsKeyScopes, scopes)
					return c.Next()
				}
			}
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
func extractAuth(c fiber.Ctx) (scheme, token string) {
	h := c.Get("Authorization")
	if h == "" {
		return "", ""
	}
	if strings.HasPrefix(h, "Bearer ") {
		return "Bearer", strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if strings.HasPrefix(h, "Bot ") {
		return "Bot", strings.TrimSpace(strings.TrimPrefix(h, "Bot "))
	}
	return "", ""
}
