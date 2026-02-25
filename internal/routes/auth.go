package routes

import (
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

func SetupAuthRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	svc := auth.NewService(d.Config, userRepo, sessionRepo)
	h := auth.NewHandler(svc)

	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	// Rate limit auth endpoints: 10 attempts per minute per IP
	authLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
	})

	r := d.App.Group("/auth")

	r.Post("/login", authLimiter, h.Login)
	r.Post("/register", authLimiter, h.Register)

	r.Post("/logout", requireAuth, h.Logout)
	r.Post("/logout_all", requireAuth, h.LogoutAll)
}
