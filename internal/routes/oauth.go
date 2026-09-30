package routes

import (
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/oauth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

func SetupOAuthRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	svc := oauth.NewService(oauth.NewRepository(d.Scylla), applications.NewRepository(d.Scylla), userRepo)

	// The `bot` and `spaces.join` scopes add members to spaces; that is the spaces
	// service's job, wired the same way SetupSpacesRoutes wires it so the join notice
	// lands in the system room.
	roomRepo := rooms.NewRepository(d.Scylla)
	spaceSvc := spaces.NewService(spaces.NewRepository(d.Scylla), roomRepo, userRepo, d.Redis, d.Config)
	spaceSvc.SetSystemMessenger(spaces.SystemMessenger(newSystemMessenger(d, roomRepo, messages.NewRepository(d.Scylla))))
	svc.SetInstaller(spaceSvc)

	h := oauth.NewHandler(svc)

	// So the auth middleware can accept an OAuth2 access token as a Bearer credential.
	middleware.SetOAuthResolver(svc.ResolveToken)

	// The token endpoint is unauthenticated (the client authenticates in the body) and a
	// natural brute-force target for client secrets, so it gets a tight budget.
	tokenLimiter := limiter.New(limiter.Config{Max: 20, Expiration: time.Minute})

	d.App.Get("/oauth2/authorize/info", requireAuth, h.Info)
	d.App.Post("/oauth2/authorize", requireAuth, h.Authorize)
	d.App.Post("/oauth2/token", tokenLimiter, h.Token)
	d.App.Post("/oauth2/token/revoke", tokenLimiter, h.Revoke)
	// Any OAuth2 token may ask what it is.
	d.App.Get("/oauth2/@me", middleware.RequireAuthScoped(sessionRepo, userRepo, middleware.AnyScope), h.Me)
	d.App.Get("/oauth2/@me/grants", requireAuth, h.Grants)
	d.App.Delete("/oauth2/@me/grants/:app_id", requireAuth, h.RevokeGrant)

	// spaces.join. Registered here (before the /spaces group in SetupSpacesRoutes) so this
	// route's own auth runs; the caller is a session or a bot, the joining user's OAuth2
	// token travels in the body.
	d.App.Put("/spaces/:id/members/:user_id", requireAuth, h.AddSpaceMember)
}
