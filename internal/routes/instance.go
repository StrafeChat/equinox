package routes

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/instance"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// newInstanceService builds the instance module with moderation wired in. Both the
// instance routes and auth (which needs the ban check at login) call this, so the two
// see the same dependencies.
func newInstanceService(d Deps, userRepo auth.UserRepository, sessionRepo auth.SessionRepository) *instance.Service {
	svc := instance.NewService(d.Config, instance.NewRepository(d.Scylla))
	roomRepo := rooms.NewRepository(d.Scylla)
	spaceRepo := spaces.NewRepository(d.Scylla)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, d.Redis, d.Config)
	msgRepo := messages.NewRepository(d.Scylla)
	roomsSvc := rooms.NewService(roomRepo, userRepo, d.Redis, d.Config, nil, spaceSvc)
	msgsSvc := messages.NewService(msgRepo, roomRepo, userRepo, d.Redis, d.Config, spaceSvc)
	svc.SetModeration(instance.ModerationDeps{
		Repo:      instance.NewModerationRepository(d.Scylla),
		Users:     userRepo,
		Sessions:  sessionRepo,
		Spaces:    spaceRepo,
		Messages:  msgRepo,
		Rooms:     roomRepo,
		Remover:   spaceSvc,
		Redis:     d.Redis,
		Region:    d.Config.Stargate.Region,
		TwoFactor: auth.NewTwoFactorRepository(d.Scylla),
		Mailer:    d.Mailer,
		RoomsSvc:  roomsSvc,
		MsgsSvc:   msgsSvc,
	})
	return svc
}

func SetupInstanceRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	svc := newInstanceService(d, userRepo, sessionRepo)
	h := instance.NewHandler(svc)

	// An instance that already had accounts when invites shipped must not hand its next
	// registrant the administrator bit. Runs once per keyspace; a failure is logged and
	// retried on the next start rather than stopping the API.
	if err := svc.SealBootstrapOnExistingInstance(context.Background()); err != nil {
		logger.Err("instance", err, map[string]any{"stage": "seal_bootstrap"})
	}

	// Make sure the instance's official account exists (used for moderation outcomes and
	// admin notices). Best effort: a failure is logged and retried on the next start.
	if err := svc.ProvisionSystemAccount(context.Background()); err != nil {
		logger.Err("instance", err, map[string]any{"stage": "provision_system_account"})
	}

	// Load the admin-managed federation allow/block list into the shared runtime lists (what
	// IsAllowedPeer consults) and keep it in sync across nodes.
	svc.StartFederationPolicy(context.Background())

	// The check endpoint is unauthenticated by necessity - it runs on the registration
	// page - so it is the one that needs a budget. Ten codes a minute per address is
	// generous for a person typing one and useless for guessing a 10-character code.
	checkLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
	})
	d.App.Get("/instance/invites/:code/check", checkLimiter, h.CheckInvite)

	// Reports come from ordinary accounts. A handful a minute is plenty for a person and
	// stops one account burying the queue.
	reportLimiter := limiter.New(limiter.Config{
		Max:        5,
		Expiration: time.Minute,
	})
	d.App.Post("/reports", requireAuth, reportLimiter, h.CreateReport)

	r := d.App.Group("/instance", requireAuth)
	r.Get("/me", h.Me)
	r.Get("/stats", h.Stats)

	r.Get("/invites", h.ListInvites)
	r.Post("/invites", h.CreateInvite)
	r.Delete("/invites/:code", h.RevokeInvite)

	r.Get("/users", h.SearchUsers)
	r.Get("/users/:id", h.GetUser)
	r.Post("/users/:id/ban", h.BanUser)
	r.Delete("/users/:id/ban", h.UnbanUser)
	r.Patch("/users/:id/badges", h.SetBadges)
	r.Patch("/users/:id/email", h.SetEmail)
	r.Post("/users/:id/notice", h.SendNotice)
	r.Post("/users/:id/recovery_codes", h.RegenerateRecoveryCodes)
	r.Get("/bans", h.ListBans)
	// Network bans: enforced on /auth (see instance.Handler.BlockBannedIPs).
	r.Get("/ip_bans", h.ListIPBans)
	r.Post("/ip_bans", h.BanIP)
	r.Delete("/ip_bans", h.UnbanIP)

	r.Get("/spaces/:id", h.GetSpace)
	r.Patch("/spaces/:id/official", h.SetSpaceOfficial)
	r.Delete("/spaces/:id", h.TakeDownSpace)

	r.Get("/reports", h.ListReports)
	r.Get("/reports/:id", h.GetReport)
	r.Post("/reports/:id/resolve", h.ResolveReport)

	r.Get("/audit", h.ListAudit)

	// Federation allow/block list (admin-managed; merged with the env lists by IsAllowedPeer).
	r.Get("/federation/policy", h.ListFederationPolicy)
	r.Post("/federation/policy", h.SetFederationPolicy)
	r.Delete("/federation/policy/:domain", h.RemoveFederationPolicy)
}
