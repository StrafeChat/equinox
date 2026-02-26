package routes

import (
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

func SetupRoomsRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	roomRepo := rooms.NewRepository(d.Scylla)
	roomSvc := rooms.NewService(roomRepo, userRepo, d.Redis, d.Config)
	roomHandler := rooms.NewHandler(roomSvc)

	r := d.App.Group("/rooms", requireAuth)
	r.Get("", roomHandler.List)
	r.Get("/notes", roomHandler.GetNotes)
	r.Get("/:id", roomHandler.Get)
	r.Post("/:id/ack", roomHandler.Ack)
	r.Post("/:id/typing", roomHandler.Typing)
	r.Post("/:id/participants", roomHandler.AddParticipant)
	r.Post("", roomHandler.CreatePM)
}

