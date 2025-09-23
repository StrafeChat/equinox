package handlers_v1

import (
	"log"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/portal"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
)

func ParticipantsPost(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	// Check if user has access to the room
	log.Printf("GetRoomParticipants: Checking room access for user %s in room %s", user.ID, roomID)
	hasAccess, err := checkRoomAccess(roomID, user.ID, utils.READ_MESSAGE_HISTORY)
	if err != nil {
		log.Printf("GetRoomParticipants: Failed to check room access: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to check room access",
		})
	}

	if !hasAccess {
		log.Printf("GetRoomParticipants: User %s denied access to room %s", user.ID, roomID)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have access to this room",
		})
	}

	// Get current voice participants from portal
	participants := portal.GetParticipants(roomID)
	log.Printf("GetRoomParticipants: Found %d participants in room %s", len(participants), roomID)

	return c.Status(fiber.StatusOK).JSON(participants)
}
