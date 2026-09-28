package routes

import (
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

func SetupMessagesRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	msgRepo := messages.NewRepository(d.Scylla)
	roomRepo := rooms.NewRepository(d.Scylla)
	spaceRepo := spaces.NewRepository(d.Scylla)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, d.Redis, d.Config)
	msgSvc := messages.NewService(msgRepo, roomRepo, userRepo, d.Redis, d.Config, spaceSvc)
	if d.Federation != nil {
		msgSvc.SetFederator(d.Federation)
	}
	msgHandler := messages.NewHandler(msgSvc)

	r := d.App.Group("/rooms", requireAuth)
	r.Post("/:id/attachments", msgHandler.UploadAttachment)
	r.Post("/:id/messages", msgHandler.Create)
	r.Get("/:id/messages", msgHandler.List)
	r.Get("/:id/messages/search", msgHandler.Search)
	r.Get("/:id/messages/:msg_id", msgHandler.Get)
	r.Patch("/:id/messages/:msg_id", msgHandler.Edit)
	r.Delete("/:id/messages/:msg_id", msgHandler.Delete)
	r.Put("/:id/messages/:msg_id/reactions/:emoji", msgHandler.AddReaction)
	r.Delete("/:id/messages/:msg_id/reactions/:emoji", msgHandler.RemoveReaction)
	r.Get("/:id/messages/:msg_id/reactions/:emoji", msgHandler.ListReactors)

	// Space-wide search lives with the messages handler rather than the spaces one: it is
	// the same scanner as the per-room search, just fanned out over a space's channels.
	d.App.Group("/spaces", requireAuth).Get("/:id/messages/search", msgHandler.SearchSpace)
}
