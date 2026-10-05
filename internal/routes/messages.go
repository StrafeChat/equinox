package routes

import (
	"context"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"time"
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
	msgSvc.StartOrphanSweeper(context.Background())

	r := d.App.Group("/rooms", requireAuth)
	// Uploads are the one message action that costs disk: cap them per account so a flood
	// cannot fill storage (unclaimed uploads are also swept, see messages/orphans.go).
	r.Post("/:id/attachments", perUserLimiter(30, time.Minute), msgHandler.UploadAttachment)
	// Anti-spam. Sending is capped per account across every room (slowmode is per channel,
	// opt-in and space-only, so on its own it does nothing against DM floods): 15 messages per
	// 10s is well above any human's pace but shuts a script down within seconds. History,
	// search and reactor listing are capped too - they are the scraping/expensive paths.
	r.Post("/:id/messages", perUserLimiter(15, 10*time.Second), msgHandler.Create)
	r.Get("/:id/messages", perUserLimiter(120, time.Minute), msgHandler.List)
	r.Get("/:id/messages/search", perUserLimiter(30, time.Minute), msgHandler.Search)
	r.Get("/:id/messages/:msg_id", msgHandler.Get)
	r.Patch("/:id/messages/:msg_id", perUserLimiter(30, time.Minute), msgHandler.Edit)
	r.Delete("/:id/messages/:msg_id", msgHandler.Delete)
	r.Put("/:id/messages/:msg_id/reactions/:emoji", perUserLimiter(60, time.Minute), msgHandler.AddReaction)
	r.Delete("/:id/messages/:msg_id/reactions/:emoji", perUserLimiter(60, time.Minute), msgHandler.RemoveReaction)
	r.Get("/:id/messages/:msg_id/reactions/:emoji", perUserLimiter(60, time.Minute), msgHandler.ListReactors)

	// Space-wide search lives with the messages handler rather than the spaces one: it is
	// the same scanner as the per-room search, just fanned out over a space's channels.
	d.App.Group("/spaces", requireAuth).Get("/:id/messages/search", perUserLimiter(30, time.Minute), msgHandler.SearchSpace)
}
