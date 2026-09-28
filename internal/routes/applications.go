package routes

import (
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"time"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

func SetupApplicationRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	svc := applications.NewService(applications.NewRepository(d.Scylla), userRepo)
	h := applications.NewHandler(svc)

	// A bot authenticates its REST and gateway calls with `Authorization: Bot <token>`;
	// this is the one place that resolves such a token to its bot account.
	middleware.SetBotResolver(svc.ResolveBotToken)

	// Creating an app mints a bot user and secrets; keep a runaway client in check.
	writeLimiter := limiter.New(limiter.Config{Max: 20, Expiration: time.Minute})

	r := d.App.Group("/applications", requireAuth)
	r.Get("", h.List)
	r.Post("", writeLimiter, h.Create)
	r.Get("/:id", h.Get)
	r.Patch("/:id", h.Patch)
	r.Delete("/:id", h.Delete)
	r.Post("/:id/secret", writeLimiter, h.ResetSecret)
	r.Post("/:id/bot", writeLimiter, h.AddBot)
	r.Post("/:id/bot/token", writeLimiter, h.ResetBotToken)
}
