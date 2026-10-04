package routes

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
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
	// Join/leave notices in a space's system room go through the same path as group
	// "X added Y" messages.
	spaceSvc.SetSystemMessenger(spaces.SystemMessenger(newSystemMessenger(d, roomRepo, messages.NewRepository(d.Scylla))))
	// Daily birthday greetings for opted-in members in spaces that set a birthday channel.
	spaceSvc.StartBirthdayWorker(context.Background())
	if d.Federation != nil {
		// Changes to a space hosted here reach the instances mirroring it; a member's
		// join/leave/invite in a space hosted elsewhere goes to that instance. The engine
		// serves remote members through this instance so their joins post the notice too.
		spaceSvc.SetFederator(d.Federation)
		d.Federation.SetSpaces(spaceSvc)
	}
	spaceHandler := spaces.NewHandler(spaceSvc, d.Config)

	// Public: invite preview and the server widget (no auth)
	d.App.Get("/spaces/invites/:code", spaceHandler.InvitePreview)
	d.App.Get("/spaces/:id/widget.json", spaceHandler.Widget)

	r := d.App.Group("/spaces", requireAuth)
	r.Get("", spaceHandler.List)
	r.Get("/:id/invites", spaceHandler.ListInvites)
	r.Delete("/:id/invites/:code", spaceHandler.DeleteInvite)
	r.Get("/:id/audit-log", spaceHandler.ListAuditLog)
	r.Get("/:id/emojis", spaceHandler.ListEmojis)
	r.Post("/:id/emojis", perUserLimiter(10, time.Minute), spaceHandler.PostEmoji)
	r.Patch("/:id/emojis/:emojiId", spaceHandler.PatchEmoji)
	r.Delete("/:id/emojis/:emojiId", spaceHandler.DeleteEmoji)

	// Custom emoji across every space the caller is in, plus by-id resolution for emoji
	// used outside their home space (a PM, another space).
	e := d.App.Group("/emojis", requireAuth)
	e.Get("", spaceHandler.ListMyEmojis)
	e.Get("/:emojiId", spaceHandler.GetEmojiByID)
	r.Get("/:id/members", spaceHandler.Members)
	r.Delete("/:id/members/:userId", spaceHandler.KickMember)
	r.Put("/:id/members/:userId/roles", spaceHandler.PutMemberRoles)
	r.Post("/:id/bans/:userId", spaceHandler.BanMember)
	r.Delete("/:id/bans/:userId", spaceHandler.UnbanMember)
	r.Get("/:id/bans", spaceHandler.ListBans)
	r.Post("/:id/leave", spaceHandler.Leave)
	r.Post("/:id/transfer-ownership", spaceHandler.TransferOwnership)
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
	r.Post("/:id/ack-all", spaceHandler.AckAll)
	r.Post("/:id/resync", spaceHandler.Resync)
	r.Post("/:id/icon", perUserLimiter(10, time.Minute), spaceHandler.PostSpaceIcon)
	r.Post("/:id/banner", perUserLimiter(10, time.Minute), spaceHandler.PostSpaceBanner)
	r.Patch("/:id", spaceHandler.PatchSpace)
	r.Delete("/:id", spaceHandler.DeleteSpace)
	r.Get("/:id", spaceHandler.Get)
	r.Post("", spaceHandler.Create)
}
