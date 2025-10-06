package routes_v1

import (
	handlers_v1 "github.com/StrafeChat/equinox/src/handlers/v1/rooms"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupRoomsRoutes(verisonRouter *fiber.Group) {
	router := verisonRouter.Group("/rooms")

	router.Use(middleware.VerifyAuth())

	// Room positions route (must come before /:id route)
	router.Patch("/positions", handlers_v1.UpdateRoomPositions)

	// Permission override routes (must come before /:id routes to avoid conflicts)
	router.Get("/:roomId/permissions", handlers_v1.GetRoomPermissionOverrides)
	router.Post("/:roomId/permissions/roles", handlers_v1.SetRolePermissionOverrides)
	router.Post("/:roomId/permissions/members", handlers_v1.SetMemberPermissionOverrides)
	router.Put("/:roomId/permissions/roles/:roleId/:permissionId", handlers_v1.UpdateRolePermissionOverride)
	router.Put("/:roomId/permissions/members/:userId/:permissionId", handlers_v1.UpdateMemberPermissionOverride)
	router.Delete("/:roomId/permissions/roles/:roleId", handlers_v1.DeleteRolePermissionOverrides)
	router.Delete("/:roomId/permissions/members/:userId", handlers_v1.DeleteMemberPermissionOverrides)

	// Message routes
	router.Post("/:id/messages", handlers_v1.CreateMessage)
	router.Get("/:id/messages", handlers_v1.GetRoomMessages)
	router.Delete("/:roomID/messages/:messageID", handlers_v1.DeleteMessage)
	router.Patch("/:roomID/messages/:messageID", handlers_v1.EditMessage)
	router.Get("/:id/unreads", handlers_v1.GetUnreadMessages)
	router.Post("/:id/ack", handlers_v1.AcknowledgeMessages)
	router.Post("/:id/typing", handlers_v1.HandleTypingIndicator)

	// Message reaction routes
	router.Post("/:room_id/messages/:message_id/reactions", handlers_v1.AddReaction)
	router.Delete("/:room_id/messages/:message_id/reactions/:emoji", handlers_v1.RemoveReaction)
	router.Get("/:room_id/messages/:message_id/reactions", handlers_v1.GetMessageReactions)

	// Room management routes
	router.Patch("/:id", handlers_v1.UpdateRoom)
	router.Delete("/:id", handlers_v1.DeleteRoom)
	router.Post("/:id/members", handlers_v1.AddMember)
	router.Delete("/:id/members", handlers_v1.RemoveMember)
	router.Post("/:id/transfer-ownership", handlers_v1.TransferOwnership)

	// voice
	router.Post("/call/:id", handlers_v1.Call)
	router.Post("/call/:id/stop", handlers_v1.StopCall)
	router.Post("/stopcall/:id", handlers_v1.StopCall)
	router.Post("/join/:id", handlers_v1.JoinPost)
	router.Post("/:id/participants", handlers_v1.ParticipantsPost)
}
