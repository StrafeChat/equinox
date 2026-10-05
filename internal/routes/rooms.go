package routes

import (
	"time"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

func SetupRoomsRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	roomRepo := rooms.NewRepository(d.Scylla)
	spaceRepo := spaces.NewRepository(d.Scylla)
	msgRepo := messages.NewRepository(d.Scylla)
	onSystemEvent := newSystemMessenger(d, roomRepo, msgRepo)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, d.Redis, d.Config)
	roomSvc := rooms.NewService(roomRepo, userRepo, d.Redis, d.Config, onSystemEvent, spaceSvc)
	roomSvc.SetSharedSpaceChecker(&sharedSpaceChecker{spaces: spaceSvc})
	roomHandler := rooms.NewHandler(roomSvc)
	if d.Federation != nil {
		roomSvc.SetFederator(d.Federation)
		roomHandler.SetHandleResolver(d.Federation)
	}

	r := d.App.Group("/rooms", requireAuth)
	r.Get("", roomHandler.List)
	r.Get("/notes", roomHandler.GetNotes)
	r.Get("/:id", roomHandler.Get)
	r.Patch("/:id", perUserLimiter(20, time.Minute), roomHandler.UpdateRoom)
	r.Post("/:id/ack", roomHandler.Ack)
	r.Patch("/:id/notify-settings", roomHandler.SetNotifySettings)
	r.Post("/:id/typing", roomHandler.Typing)
	// Anti-spam: opening DMs and adding people to groups are how one account reaches many
	// strangers. 10 new conversations a minute is plenty for a person; a mass-DM script is not.
	r.Post("/:id/participants", perUserLimiter(20, time.Minute), roomHandler.AddParticipant)
	r.Delete("/:id/participants/:user_id", roomHandler.RemoveParticipant)
	r.Post("", perUserLimiter(10, time.Minute), roomHandler.CreatePM)
}
