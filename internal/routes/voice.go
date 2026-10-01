package routes

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/voice"
)

// SetupVoiceRoutes wires voice/video calling. Without LiveKit configured nothing is
// registered: the client sees features.voice.enabled=false on GET / and hides every
// call control, and the routes 404 like any other unknown path.
func SetupVoiceRoutes(d Deps) {
	if !d.Config.Voice.Enabled {
		return
	}
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	roomRepo := rooms.NewRepository(d.Scylla)
	spaceRepo := spaces.NewRepository(d.Scylla)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, d.Redis, d.Config)
	onSystemEvent := newSystemMessenger(d, roomRepo, messages.NewRepository(d.Scylla))
	roomSvc := rooms.NewService(roomRepo, userRepo, d.Redis, d.Config, onSystemEvent, spaceSvc)
	if d.Federation != nil {
		roomSvc.SetFederationInfo(d.Federation)
	}

	store := voice.NewStore(d.Redis, d.Config.Database.Redis.CachePrefix)
	svc := voice.NewService(store, voice.NewLiveKit(d.Config.Voice), roomSvc, spaceSvc, userRepo, d.Redis, d.Config, onSystemEvent)
	h := voice.NewHandler(svc)
	if d.Federation != nil {
		// Calls in federated PMs/groups: the room's origin hosts them and the other
		// instances mirror; both directions go through the federation engine.
		svc.SetFederator(d.Federation)
		d.Federation.SetVoice(svc)
	}

	// Existing spaces predate the voice permission bits; give their @everyone role the
	// defaults once so members can actually connect.
	if err := spaceSvc.BackfillVoicePermissions(context.Background()); err != nil {
		logger.Err("voice", err, map[string]any{"step": "backfill voice permissions"})
	}
	svc.RunReconciler(context.Background())

	// Joining mints a token and touches LiveKit; keep a runaway client from hammering it.
	joinLimiter := limiter.New(limiter.Config{Max: 30, Expiration: time.Minute})

	r := d.App.Group("/rooms", requireAuth)
	r.Post("/:id/voice/join", joinLimiter, h.Join)
	r.Get("/:id/voice/states", h.States)
	r.Post("/:id/call/ring", h.Ring)
	r.Post("/:id/call/decline", h.Decline)

	v := d.App.Group("/voice")
	v.Post("/webhook", h.Webhook)
	v.Post("/leave", requireAuth, h.Leave)
	v.Patch("/state", requireAuth, h.UpdateSelf)

	s := d.App.Group("/spaces", requireAuth)
	s.Patch("/:id/members/:userId/voice", h.Moderate)
	s.Delete("/:id/members/:userId/voice", h.Disconnect)
}
