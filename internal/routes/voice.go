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

// newVoiceEvictor is the voice service the spaces and rooms route setups hand their
// member-removal hooks to. Each of them constructs its own spaces/rooms services, so a hook
// has to be set on those very instances; the voice state it acts on lives in Redis and
// LiveKit, so another voice service is just another handle on the same facts. Nil when
// voice is off.
func newVoiceEvictor(d Deps, roomSvc *rooms.Service, spaceSvc *spaces.Service, userRepo auth.UserRepository, onSystemEvent rooms.OnRoomSystemEvent) *voice.Service {
	if !d.Config.Voice.Enabled {
		return nil
	}
	store := voice.NewStore(d.Redis, d.Config.Database.Redis.CachePrefix)
	svc := voice.NewService(store, voice.NewLiveKit(d.Config.Voice), roomSvc, spaceSvc, userRepo, d.Redis, d.Config, onSystemEvent)
	if d.Federation != nil {
		svc.SetFederator(d.Federation)
	}
	return svc
}

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
	joinLimiter := limiter.New(limiter.Config{KeyGenerator: clientKey, Max: 30, Expiration: time.Minute})

	r := d.App.Group("/rooms", requireAuth)
	r.Post("/:id/voice/join", joinLimiter, h.Join)
	r.Get("/:id/voice/states", h.States)
	// Ringing and state changes fan out to every participant's devices (push included) or
	// to a whole space; a person does them a few times a minute, a script does not.
	r.Post("/:id/call/ring", perUserLimiter(30, time.Minute), h.Ring)
	r.Post("/:id/call/decline", perUserLimiter(60, time.Minute), h.Decline)

	v := d.App.Group("/voice")
	v.Post("/webhook", h.Webhook)
	v.Post("/leave", requireAuth, perUserLimiter(60, time.Minute), h.Leave)
	v.Patch("/state", requireAuth, perUserLimiter(120, time.Minute), h.UpdateSelf)

	s := d.App.Group("/spaces", requireAuth)
	s.Patch("/:id/members/:userId/voice", perUserLimiter(60, time.Minute), h.Moderate)
	s.Delete("/:id/members/:userId/voice", perUserLimiter(60, time.Minute), h.Disconnect)
}
