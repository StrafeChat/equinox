package routes

import (
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/unfurl"
)

// SetupUnfurlRoutes wires GET /unfurl, the link-preview metadata endpoint the client calls to
// render a card for a URL. Auth-required (only members should be able to make the instance
// fetch outbound), and rate-limited since each call is an outbound request.
func SetupUnfurlRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	h := unfurl.NewHandler(unfurl.NewService(d.Redis))
	lim := limiter.New(limiter.Config{Max: 60, Expiration: time.Minute})

	d.App.Get("/unfurl", requireAuth, lim, h.Get)
}
