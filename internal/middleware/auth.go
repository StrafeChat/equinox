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

// AnyScope, passed to RequireAuthScoped, admits an OAuth2 token whatever its scopes (for
// endpoints that describe the token itself, like GET /oauth2/@me).
const AnyScope = "*"

// RequireAuth authenticates the request and loads the user (and, for a session, the
// session) into Locals, or returns 401. It accepts a session `Bearer` token or a
// `Bot <bot token>`. An OAuth2 access token is recognised but refused with 403
// insufficient_scope: a third-party app acting for a user only reaches the endpoints that
// opt in through RequireAuthScoped, so a scope can never grant more than it says.
func RequireAuth(sessionRepo auth.SessionRepository, userRepo auth.UserRepository) fiber.Handler {
	return requireAuth(sessionRepo, userRepo, nil)
}

// RequireAuthScoped is RequireAuth for an endpoint that OAuth2 access tokens may call: any
// one of the listed scopes admits the token (its full scope list is still recorded in
// Locals[LocalsKeyScopes] so the handler can gate individual fields). Sessions and bots
// pass as with RequireAuth.
func RequireAuthScoped(sessionRepo auth.SessionRepository, userRepo auth.UserRepository, scopes ...string) fiber.Handler {
	return requireAuth(sessionRepo, userRepo, scopes)
}

func requireAuth(sessionRepo auth.SessionRepository, userRepo auth.UserRepository, allowed []string) fiber.Handler {
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
					if !scopeAdmits(allowed, scopes) {
						return insufficientScope(c, allowed)
					}
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

// scopeAdmits reports whether a token with `granted` scopes may call an endpoint that
// admits `allowed` (nil = no OAuth2 access at all).
func scopeAdmits(allowed, granted []string) bool {
	for _, a := range allowed {
		if a == AnyScope {
			return true
		}
		for _, g := range granted {
			if g == a {
				return true
			}
		}
	}
	return false
}

// insufficientScope is the RFC 6750 refusal: the token is valid but not for this.
func insufficientScope(c fiber.Ctx, allowed []string) error {
	desc := "this endpoint is not available to OAuth2 access tokens"
	if len(allowed) > 0 {
		desc = "this endpoint requires the " + strings.Join(allowed, " or ") + " scope"
	}
	c.Set("WWW-Authenticate", `Bearer error="insufficient_scope", error_description="`+desc+`"`)
	return c.Status(http.StatusForbidden).JSON(fiber.Map{
		"error":             "insufficient_scope",
		"error_description": desc,
	})
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
