package routes

import (
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

func SetupAuthRoutes(d Deps) {
	userRepo := auth.NewUserRepository(d.Scylla)
	sessionRepo := auth.NewSessionRepository(d.Scylla)
	svc := auth.NewService(d.Config, userRepo, sessionRepo)
	h := auth.NewHandler(svc)

	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	r := d.App.Group("/auth")

	r.Post("/login", h.Login)
	r.Post("/register", h.Register)

	r.Post("/logout", requireAuth, h.Logout)
	r.Post("/logout_all", requireAuth, h.LogoutAll)
}
