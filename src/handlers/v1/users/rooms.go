package handlers_v1

import (
	"log"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

func GetUserRooms(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)

	// Query RoomRecipientByUser to get all rooms for the user
	roomRecipientQuery := models.RoomRecipientByUserTable.SelectBuilder().
		Columns("room_id").
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"user_id": user.ID,
		})

	var roomRecipients []models.RoomRecipientByUser
	if err := roomRecipientQuery.SelectRelease(&roomRecipients); err != nil {
		log.Printf("Error fetching user's rooms: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to fetch user's rooms",
		})
	}

	// Get the full room data for each room ID
	var rooms []types.Room
	for _, recipient := range roomRecipients {
		var room types.Room
		roomQuery := models.RoomTable.SelectBuilder().
			Columns("*").
			Where(qb.Eq("id")).
			Limit(1).
			Query(*database.Session).
			BindMap(qb.M{
				"id": recipient.RoomId,
			})

		if err := roomQuery.GetRelease(&room); err != nil {
			log.Printf("Error fetching room details for room ID %s: %v", recipient.RoomId, err)
			continue
		}

		rooms = append(rooms, room)
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"rooms": rooms,
	})
}
