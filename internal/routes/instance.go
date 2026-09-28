package routes

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/instance"
)

func SetupInstanceRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	svc := instance.NewService(d.Config, instance.NewRepository(d.Scylla))
	h := instance.NewHandler(svc)

	// An instance that already had accounts when invites shipped must not hand its next
	// registrant the administrator bit. Runs once per keyspace; a failure is logged and
	// retried on the next start rather than stopping the API.
	if err := svc.SealBootstrapOnExistingInstance(context.Background()); err != nil {
		logger.Err("instance", err, map[string]any{"stage": "seal_bootstrap"})
	}

	// The check endpoint is unauthenticated by necessity - it runs on the registration
	// page - so it is the one that needs a budget. Ten codes a minute per address is
	// generous for a person typing one and useless for guessing a 10-character code.
	checkLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
	})
	d.App.Get("/instance/invites/:code/check", checkLimiter, h.CheckInvite)

	r := d.App.Group("/instance", requireAuth)
	r.Get("/me", h.Me)
	r.Get("/invites", h.ListInvites)
	r.Post("/invites", h.CreateInvite)
	r.Delete("/invites/:code", h.RevokeInvite)
}
