package routes_v1

import (
	handlers_v1 "github.com/StrafeChat/equinox/src/handlers/v1/rooms"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupRoomsRoutes(verisonRouter *fiber.Group) {
	router := verisonRouter.Group("/rooms")

	router.Use(middleware.VerifyAuth())

	router.Post("/:id/messages", handlers_v1.CreateMessage)
	router.Get("/:id/messages", handlers_v1.GetRoomMessages)
	router.Delete("/:roomID/messages/:messageID", handlers_v1.DeleteMessage)
	router.Patch("/:roomID/messages/:messageID", handlers_v1.EditMessage)
	router.Get("/:id/unreads", handlers_v1.GetUnreadMessages)
	router.Post("/:id/ack", handlers_v1.AcknowledgeMessages)
	router.Post("/:id/typing", handlers_v1.HandleTypingIndicator)

	// Room management routes
	router.Patch("/:id", handlers_v1.UpdateRoom)
	router.Delete("/:id", handlers_v1.DeleteRoom)
	router.Post("/:id/members", handlers_v1.AddMember)
	router.Delete("/:id/members", handlers_v1.RemoveMember)
	router.Post("/:id/transfer-ownership", handlers_v1.TransferOwnership)

	// voice
	router.Post("/join/:id", handlers_v1.JoinPost)

}
