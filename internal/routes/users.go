package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/federation"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/oauth"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/users"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

// relationshipResolver adapts federation's handle lookup to the relationships handler,
// which wants an HTTP status per failure but must not import the federation package
// (federation imports relationships for the inbound side).
type relationshipResolver struct {
	fed *federation.Service
}

func (r relationshipResolver) ResolveHandle(ctx context.Context, handle string) (*auth.User, error) {
	u, err := r.fed.ResolveHandle(ctx, handle)
	if err == nil {
		return u, nil
	}
	status, msg := http.StatusInternalServerError, "internal error"
	var peerReply *federation.StatusError
	switch {
	case errors.Is(err, federation.ErrInvalidHandle):
		status, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, federation.ErrRemoteUserNotFound):
		status, msg = http.StatusNotFound, err.Error()
		if !strings.Contains(handle, "@") {
			msg = "user not found"
		}
	case errors.Is(err, federation.ErrPeerNotAllowed):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, federation.ErrRemoteUnavailable):
		status, msg = http.StatusBadGateway, federation.ErrRemoteUnavailable.Error()
	case errors.As(err, &peerReply):
		status, msg = http.StatusBadGateway, "the other instance refused the request"
		logger.Err("relationships", err, map[string]any{"handle": handle})
	default:
		logger.Err("relationships", err, map[string]any{"handle": handle})
	}
	return nil, &relationships.ResolveError{Status: status, Message: msg}
}

func SetupUsersRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	twoFactorRepo := auth.NewTwoFactorRepository(d.Scylla)
	authSvc := auth.NewService(d.Config, userRepo, sessionRepo, twoFactorRepo, d.Redis)
	wireMail(d, authSvc)
	authHandler := auth.NewHandler(authSvc)

	relRepo := relationships.NewRepository(d.Scylla)
	relSvc := relationships.NewService(relRepo, userRepo, d.Redis, d.Config)
	relHandler := relationships.NewHandler(relSvc)
	if d.Federation != nil {
		relSvc.SetFederator(d.Federation)
		relHandler.SetHandleResolver(relationshipResolver{d.Federation})
	}

	roomsRepo := rooms.NewRepository(d.Scylla)
	usersHandler := users.NewHandler(userRepo, roomsRepo, d.Redis, d.Config)
	if d.Federation != nil {
		usersHandler.SetFederator(d.Federation)
	}
	// A bot's profile is edited by its application's owner with the same machinery as a
	// person's own (limits, Nebula storage, USER_UPDATE fan-out).
	usersHandler.SetApplications(applications.NewRepository(d.Scylla))
	d.App.Patch("/applications/:id/bot", requireAuth, usersHandler.PatchBotProfile)
	d.App.Post("/applications/:id/bot/avatar", requireAuth, usersHandler.PostBotAvatar)
	d.App.Post("/applications/:id/bot/banner", requireAuth, usersHandler.PostBotBanner)

	// The two endpoints an OAuth2 access token may call about the account. They are
	// registered before the /users/@me group below so their scoped auth runs instead of
	// the group's session-or-bot-only auth (Fiber matches in registration order).
	d.App.Get("/users/@me", middleware.RequireAuthScoped(sessionRepo, userRepo, oauth.ScopeIdentify, oauth.ScopeEmail), usersHandler.Me)
	spaceHandler := spaces.NewHandler(spaces.NewService(spaces.NewRepository(d.Scylla), roomsRepo, userRepo, d.Redis, d.Config), d.Config)
	d.App.Get("/users/@me/spaces", middleware.RequireAuthScoped(sessionRepo, userRepo, oauth.ScopeSpaces), spaceHandler.MySpaces)

	// /users/@me - current user
	me := d.App.Group("/users/@me", requireAuth)
	me.Patch("", usersHandler.PatchMe)
	me.Post("/avatar", usersHandler.PostAvatar)
	me.Post("/banner", usersHandler.PostBanner)

	// /users/@me/2fa - TOTP, passkeys, recovery codes.
	//
	// The endpoints that take the account password (disable, remove a passkey, regenerate
	// codes) re-verify it with bcrypt, which makes them an online password-guessing
	// surface for anyone holding a stolen session - requireAuth alone put no ceiling on
	// that. Same budget as the login-time MFA routes; enable gets it too, since it accepts
	// a six-digit code.
	twoFactorLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
	})
	me.Get("/2fa", authHandler.TwoFactorStatus)
	me.Post("/2fa/totp/setup", authHandler.SetupTOTP)
	me.Post("/2fa/totp/enable", twoFactorLimiter, authHandler.EnableTOTP)
	me.Post("/2fa/totp/disable", twoFactorLimiter, authHandler.DisableTOTP)
	me.Post("/2fa/webauthn/register/begin", authHandler.BeginWebAuthnRegistration)
	me.Post("/2fa/webauthn/register/finish", authHandler.FinishWebAuthnRegistration)
	me.Delete("/2fa/webauthn/:credential_id", twoFactorLimiter, authHandler.DeleteWebAuthnCredential)
	me.Post("/2fa/recovery_codes/regenerate", twoFactorLimiter, authHandler.RegenerateRecoveryCodes)

	// Ask for the email verification link again. Each call is a delivery, so the same
	// budget as the unauthenticated forgot-password route, on top of the per-account
	// one-a-minute cooldown in the service.
	emailRequestLimiter := limiter.New(limiter.Config{
		Max:        5,
		Expiration: 15 * time.Minute,
	})
	me.Post("/email/verification", emailRequestLimiter, authHandler.SendVerificationEmail)

	// /users/@me/relationships
	r := me.Group("/relationships")
	r.Get("", relHandler.Get)
	r.Post("", relHandler.Post)
	r.Put("/:user_id", relHandler.PutByID)
	r.Put("/:user_id/block", relHandler.PutBlock)
	r.Delete("/:user_id/block", relHandler.DeleteBlock)
	r.Put("/:user_id/ignore", relHandler.PutIgnore)
	r.Delete("/:user_id/ignore", relHandler.DeleteIgnore)
	r.Delete("/:user_id", relHandler.Delete)
	r.Delete("", relHandler.BulkDelete)
	r.Patch("/:user_id", relHandler.Patch)
}
