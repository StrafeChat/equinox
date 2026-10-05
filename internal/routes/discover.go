package routes

import (
	"time"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/discover"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// SetupDiscoverRoutes: the instance's directory of spaces and bots - the public page,
// the applicant's side under the space and application resources, and the review queue
// under the instance administration.
func SetupDiscoverRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	// Joining from Discover is a member add like any other: the join notice lands in the
	// system room and, for a space with mirrors elsewhere, the member reaches them.
	roomRepo := rooms.NewRepository(d.Scylla)
	spaceSvc := spaces.NewService(spaces.NewRepository(d.Scylla), roomRepo, userRepo, d.Redis, d.Config)
	spaceSvc.SetSystemMessenger(spaces.SystemMessenger(newSystemMessenger(d, roomRepo, messages.NewRepository(d.Scylla))))
	if d.Federation != nil {
		spaceSvc.SetFederator(d.Federation)
	}
	inst := newInstanceService(d, userRepo, sessionRepo)
	svc := discover.NewService(discover.NewRepository(d.Scylla), spaceSvc, applications.NewRepository(d.Scylla), userRepo, inst, d.Redis, d.Config.Database.Redis.CachePrefix)
	h := discover.NewHandler(svc)

	g := d.App.Group("/discover", requireAuth)
	// Directory browsing and joins per account - caps scraping and join-flooding.
	g.Get("/spaces", perUserLimiter(60, time.Minute), h.Spaces)
	g.Get("/bots", perUserLimiter(60, time.Minute), h.Bots)
	g.Post("/spaces/:id/join", perUserLimiter(20, time.Minute), h.JoinSpace)

	d.App.Get("/spaces/:id/discover", requireAuth, h.SpaceStatus)
	d.App.Put("/spaces/:id/discover", requireAuth, h.SpaceApply)
	d.App.Delete("/spaces/:id/discover", requireAuth, h.SpaceWithdraw)
	d.App.Get("/applications/:id/discover", requireAuth, h.BotStatus)
	d.App.Put("/applications/:id/discover", requireAuth, h.BotApply)
	d.App.Delete("/applications/:id/discover", requireAuth, h.BotWithdraw)

	d.App.Get("/instance/discover", requireAuth, h.Queue)
	d.App.Post("/instance/discover/:kind/:id/review", requireAuth, h.Review)
}
