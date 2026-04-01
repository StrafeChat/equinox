package routes

import (
	"context"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/stargate"
)

func SetupRoomsRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	roomRepo := rooms.NewRepository(d.Scylla)
	spaceRepo := spaces.NewRepository(d.Scylla)
	msgRepo := messages.NewRepository(d.Scylla)
	region := "default"
	if d.Config != nil && d.Config.Stargate.Region != "" {
		region = d.Config.Stargate.Region
	}
	onSystemEvent := func(ctx context.Context, roomID int64, participantIDs []int64, eventType, payload string) (int64, error) {
		msg := &messages.Message{
			RoomID:        roomID,
			ID:            id.Next(),
			SenderID:      0,
			SystemType:    eventType,
			SystemPayload: payload,
		}
		if err := msgRepo.Create(ctx, msg); err != nil {
			return 0, err
		}
		if err := roomRepo.UpdateLastMessageID(ctx, roomID, participantIDs, msg.ID); err != nil {
			// non-fatal
		}
		if d.Redis != nil {
			pl := map[string]interface{}{
				"room_id":         id.Format(roomID),
				"id":              id.Format(msg.ID),
				"sender_id":       "0",
				"system_type":     msg.SystemType,
				"system_payload": msg.SystemPayload,
				"created_at":      msg.CreatedAt,
				"updated_at":      msg.UpdatedAt,
			}
			stargate.PublishToSpace(ctx, d.Redis, roomID, "MESSAGE_CREATE", pl, region)
		}
		return msg.ID, nil
	}
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, d.Redis, d.Config)
	roomSvc := rooms.NewService(roomRepo, userRepo, d.Redis, d.Config, onSystemEvent, spaceSvc)
	roomHandler := rooms.NewHandler(roomSvc)

	r := d.App.Group("/rooms", requireAuth)
	r.Get("", roomHandler.List)
	r.Get("/notes", roomHandler.GetNotes)
	r.Get("/:id", roomHandler.Get)
	r.Patch("/:id", roomHandler.UpdateRoom)
	r.Post("/:id/ack", roomHandler.Ack)
	r.Post("/:id/typing", roomHandler.Typing)
	r.Post("/:id/participants", roomHandler.AddParticipant)
	r.Delete("/:id/participants/:user_id", roomHandler.RemoveParticipant)
	r.Post("", roomHandler.CreatePM)
}

