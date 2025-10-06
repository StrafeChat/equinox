package handlers_v1

import (
	"log"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/portal"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
)

func Call(c fiber.Ctx) error {
	roomID := c.Params(("id"))
	user := c.Locals("user").(models.User)

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	// Check if user has access to the room
	// TODO: potentially change permission type
	hasAccess, err := checkRoomAccess(roomID, user.ID, utils.READ_MESSAGE_HISTORY)
	if err != nil {
		log.Printf("EditMessage: Failed to check room access: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to check room access",
		})
	}
	log.Printf("EditMessage: User %s has access to room %s: %v", user.ID, roomID, hasAccess)
	if !hasAccess {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have access to this room",
		})
	}

	// TODO: restrict call endpoint to pms only
	portal.StartRinging(user.ID, roomID)

	return nil
}

func StopCall(c fiber.Ctx) error {
	return nil
}
