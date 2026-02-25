package routes

import (
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/devices"
)

func SetupDevicesRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	devRepo := devices.NewRepository(d.Scylla)
	devSvc := devices.NewService(devRepo)
	devHandler := devices.NewHandler(devSvc)

	r := d.App.Group("/devices", requireAuth)
	r.Post("", devHandler.RegisterDevice)

	// Devices and prekey bundle
	users := d.App.Group("/users", requireAuth)
	users.Get("/:user_id/devices", devHandler.ListDevices)
	users.Get("/:user_id/devices/:device_id/prekey_bundle", devHandler.GetPrekeyBundle)
}
