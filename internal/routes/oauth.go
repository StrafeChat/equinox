package routes

import (
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/oauth"
)

func SetupOAuthRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	svc := oauth.NewService(oauth.NewRepository(d.Scylla), applications.NewRepository(d.Scylla), userRepo)
	h := oauth.NewHandler(svc)

	// So the auth middleware can accept an OAuth2 access token as a Bearer credential.
	middleware.SetOAuthResolver(svc.ResolveToken)

	// The token endpoint is unauthenticated (the client authenticates in the body) and a
	// natural brute-force target for client secrets, so it gets a tight budget.
	tokenLimiter := limiter.New(limiter.Config{Max: 20, Expiration: time.Minute})

	d.App.Get("/oauth2/authorize/info", requireAuth, h.Info)
	d.App.Post("/oauth2/authorize", requireAuth, h.Authorize)
	d.App.Post("/oauth2/token", tokenLimiter, h.Token)
	d.App.Get("/oauth2/@me/grants", requireAuth, h.Grants)
	d.App.Delete("/oauth2/@me/grants/:app_id", requireAuth, h.RevokeGrant)
}
