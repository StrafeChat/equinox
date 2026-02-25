package routes

import (
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/users"
)

func SetupUsersRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	relRepo := relationships.NewRepository(d.Scylla)
	relSvc := relationships.NewService(relRepo, userRepo, d.Redis, d.Config)
	relHandler := relationships.NewHandler(relSvc)

	usersHandler := users.NewHandler(userRepo, d.Redis, d.Config)

	// /users/@me - current user
	me := d.App.Group("/users/@me", requireAuth)
	me.Get("", usersHandler.Me)
	me.Patch("", usersHandler.PatchMe)

	// /users/@me/relationships
	r := me.Group("/relationships")
	r.Get("", relHandler.Get)
	r.Post("", relHandler.Post)
	r.Put("/:user_id", relHandler.PutByID)
	r.Put("/:user_id/ignore", relHandler.PutIgnore)
	r.Delete("/:user_id/ignore", relHandler.DeleteIgnore)
	r.Delete("/:user_id", relHandler.Delete)
	r.Delete("", relHandler.BulkDelete)
	r.Patch("/:user_id", relHandler.Patch)
}
