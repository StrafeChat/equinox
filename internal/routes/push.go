package routes

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/push"
	"github.com/StrafeChat/equinox/internal/modules/push/pushdir"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// SetupPushRoutes wires push notifications: the subscription API, the session-revocation
// hook, the producers' switch and the delivery workers. With PUSH_ENABLED off every route
// answers 404 and nothing is queued.
func SetupPushRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	if !d.Config.Push.Enabled {
		disablePushRoutes(d, requireAuth)
		return
	}

	repo := push.NewRepository(d.Scylla)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	vapid, err := push.LoadVAPID(ctx, d.Config.Push.VAPIDPrivateKey, d.Config.Push.VAPIDSubject, repo)
	cancel()
	if err != nil {
		// The configured key was checked at boot (config.validatePush), so this is the
		// database failing to store or return a generated one: that should cost push, not
		// the whole API. A restart retries.
		logger.Err("push", err, map[string]any{"step": "load VAPID key"})
		disablePushRoutes(d, requireAuth)
		return
	}

	roomRepo, spaceRepo := rooms.NewRepository(d.Scylla), spaces.NewRepository(d.Scylla)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, d.Redis, d.Config)
	dir := pushdir.New(userRepo, sessionRepo, roomRepo, spaceRepo, spaceSvc)
	svc := push.NewService(repo, dir, d.Redis, d.Config.Push, vapid)
	h := push.NewHandler(svc)

	// Whatever revokes a session - sign-out, sign-out everywhere, a password reset, an
	// instance ban - takes its push subscription with it.
	auth.SetSessionRevokedHook(svc.ForgetSession)
	push.SetProducing(true)
	svc.StartWorkers(context.Background())

	d.App.Get("/push/vapid-key", h.VAPIDKey)
	me := d.App.Group("/users/@me/push", requireAuth)
	me.Get("", perUserLimiter(60, time.Minute), h.Get)
	me.Put("", perUserLimiter(10, time.Minute), h.Put)
	me.Patch("", perUserLimiter(30, time.Minute), h.Patch)
	me.Delete("", perUserLimiter(30, time.Minute), h.Delete)
	// Each test is a real push through the device's push service: keep them rare.
	me.Post("/test", perUserLimiter(5, time.Minute), h.Test)
}

// disablePushRoutes answers every push route with 404 "push is disabled" and keeps the
// producers off.
func disablePushRoutes(d Deps, requireAuth fiber.Handler) {
	push.SetProducing(false)
	d.App.Get("/push/vapid-key", push.Disabled)
	d.App.All("/users/@me/push", requireAuth, push.Disabled)
	d.App.All("/users/@me/push/test", requireAuth, push.Disabled)
}
