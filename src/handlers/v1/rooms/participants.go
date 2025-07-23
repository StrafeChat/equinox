package handlers_v1

import (
	"log"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/portal"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
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
	var roomRecipients []models.RoomRecipientByUser
	log.Printf("GetRoomMessages: Checking room access for user %s", user.ID)
	if err := models.RoomRecipientByUserTable.SelectBuilder().
		Columns("user_id", "room_id").
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"user_id": user.ID,
		}).
		SelectRelease(&roomRecipients); err != nil {
		log.Printf("GetRoomMessages: Failed to check room access: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to check room access",
		})
	}

	// Check if user has access to the room
	log.Printf("EditMessage: Checking room access for user %s", user.ID)
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

	participants := portal.GetParticipants(roomID)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"participants": participants,
	})
}
