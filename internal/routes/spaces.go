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
	spaceHandler := spaces.NewHandler(spaceSvc, d.Config)

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
	r.Patch("/:id/rooms/:roomId", spaceHandler.PatchRoom)
	r.Delete("/:id/rooms/:roomId", spaceHandler.DeleteRoom)
	r.Get("/:id/rooms/:roomId/overrides/users", spaceHandler.ListRoomUserOverrides)
	r.Put("/:id/rooms/:roomId/overrides/users/:userId", spaceHandler.PutRoomUserOverride)
	r.Delete("/:id/rooms/:roomId/overrides/users/:userId", spaceHandler.DeleteRoomUserOverride)
	r.Post("/:id/invites", spaceHandler.CreateInvite)
	r.Post("/invites/:code/join", spaceHandler.JoinByInvite)
	r.Post("/:id/rooms/reorder", spaceHandler.ReorderRooms)
	r.Post("/:id/rooms/move", spaceHandler.MoveChannel)
	r.Post("/:id/rooms", spaceHandler.PostRoom)
	r.Get("/:id/rooms", spaceHandler.GetRooms)
	r.Post("/:id/icon", spaceHandler.PostSpaceIcon)
	r.Patch("/:id", spaceHandler.PatchSpace)
	r.Get("/:id", spaceHandler.Get)
	r.Post("", spaceHandler.Create)
}
