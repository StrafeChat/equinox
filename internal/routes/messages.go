package routes

import (
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

func SetupMessagesRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	msgRepo := messages.NewRepository(d.Scylla)
	roomRepo := rooms.NewRepository(d.Scylla)
	msgSvc := messages.NewService(msgRepo, roomRepo, d.Redis, d.Config)
	msgHandler := messages.NewHandler(msgSvc)

	r := d.App.Group("/rooms", requireAuth)
	r.Post("/:id/messages", msgHandler.Create)
	r.Get("/:id/messages", msgHandler.List)
	r.Get("/:id/messages/:msg_id", msgHandler.Get)
	r.Patch("/:id/messages/:msg_id", msgHandler.Edit)
	r.Delete("/:id/messages/:msg_id", msgHandler.Delete)
}
