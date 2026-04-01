package routes

import (
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

func SetupSpacesRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	spaceRepo := spaces.NewRepository(d.Scylla)
	roomRepo := rooms.NewRepository(d.Scylla)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, d.Redis, d.Config)
	spaceHandler := spaces.NewHandler(spaceSvc)

	// Public: invite preview (no auth)
	d.App.Get("/spaces/invites/:code", spaceHandler.InvitePreview)

	r := d.App.Group("/spaces", requireAuth)
	r.Get("", spaceHandler.List)
	r.Get("/:id/members", spaceHandler.Members)
	r.Put("/:id/members/:userId/roles", spaceHandler.PutMemberRoles)
	r.Get("/:id/roles", spaceHandler.ListRoles)
	r.Post("/:id/roles", spaceHandler.CreateRole)
	r.Patch("/:id/roles/:roleId", spaceHandler.UpdateRole)
	r.Delete("/:id/roles/:roleId", spaceHandler.DeleteRole)
	r.Get("/:id/rooms/:roomId/overrides", spaceHandler.ListRoomOverrides)
	r.Put("/:id/rooms/:roomId/overrides/:roleId", spaceHandler.PutRoomOverride)
	r.Delete("/:id/rooms/:roomId/overrides/:roleId", spaceHandler.DeleteRoomOverride)
	r.Post("/:id/invites", spaceHandler.CreateInvite)
	r.Post("/invites/:code/join", spaceHandler.JoinByInvite)
	r.Get("/:id/rooms", spaceHandler.GetRooms)
	r.Get("/:id", spaceHandler.Get)
	r.Post("", spaceHandler.Create)
}
