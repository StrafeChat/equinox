package handlers_v1

import (
	"log"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/scylladb/gocqlx/v3/qb"

	"github.com/StrafeChat/equinox/src/portal"
	"github.com/gofiber/fiber/v3"
)

func JoinPost(c fiber.Ctx) error {
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

	// Check if the user is a recipient of the specified room
	hasAccess := false
	for _, recipient := range roomRecipients {
		if recipient.RoomId == roomID {
			hasAccess = true
			break
		}
	}

	log.Printf("GetRoomMessages: User %s has access to room %s: %v", user.ID, roomID, hasAccess)
	if !hasAccess {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have access to this room",
		})
	}

	token := portal.GetJoinToken(roomID, user.ID)

	// Handle the join room logic here
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"token": token,
	})
}
