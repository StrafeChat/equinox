package routes

import (
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/oauth"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/users"
)

func SetupUsersRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	relRepo := relationships.NewRepository(d.Scylla)
	relSvc := relationships.NewService(relRepo, userRepo, d.Redis, d.Config)
	relHandler := relationships.NewHandler(relSvc)

	roomsRepo := rooms.NewRepository(d.Scylla)
	usersHandler := users.NewHandler(userRepo, roomsRepo, d.Redis, d.Config)
	if d.Federation != nil {
		usersHandler.SetFederator(d.Federation)
	}

	// The two endpoints an OAuth2 access token may call about the account. They are
	// registered before the /users/@me group below so their scoped auth runs instead of
	// the group's session-or-bot-only auth (Fiber matches in registration order).
	d.App.Get("/users/@me", middleware.RequireAuthScoped(sessionRepo, userRepo, oauth.ScopeIdentify, oauth.ScopeEmail), usersHandler.Me)
	spaceHandler := spaces.NewHandler(spaces.NewService(spaces.NewRepository(d.Scylla), roomsRepo, userRepo, d.Redis, d.Config), d.Config)
	d.App.Get("/users/@me/spaces", middleware.RequireAuthScoped(sessionRepo, userRepo, oauth.ScopeSpaces), spaceHandler.MySpaces)

	// /users/@me - current user
	me := d.App.Group("/users/@me", requireAuth)
	me.Patch("", usersHandler.PatchMe)
	me.Post("/avatar", usersHandler.PostAvatar)
	me.Post("/banner", usersHandler.PostBanner)

	// /users/@me/relationships
	r := me.Group("/relationships")
	r.Get("", relHandler.Get)
	r.Post("", relHandler.Post)
	r.Put("/:user_id", relHandler.PutByID)
	r.Put("/:user_id/block", relHandler.PutBlock)
	r.Delete("/:user_id/block", relHandler.DeleteBlock)
	r.Put("/:user_id/ignore", relHandler.PutIgnore)
	r.Delete("/:user_id/ignore", relHandler.DeleteIgnore)
	r.Delete("/:user_id", relHandler.Delete)
	r.Delete("", relHandler.BulkDelete)
	r.Patch("/:user_id", relHandler.Patch)
}
