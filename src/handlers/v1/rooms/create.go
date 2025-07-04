package handlers_v1

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gofiber/fiber/v3"
)

func CreateRoom(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	body := new(types.CreateRoomInput)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body.",
		})
	}

	if len(body.Recipients) == 0 || body.Recipients[0] == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body, recipients is required.",
		})
	}
	// For PMs, check if a room already exists with the same recipient
	if !body.IsGroup && len(body.Recipients) == 1 {
		// First, get all rooms for the current user
		var userRooms []models.RoomRecipientByUser
		q := models.RoomRecipientByUserTable.SelectQuery(*database.Session)

		// Include both primary key components and use proper binding
		if err := q.BindStruct(&models.RoomRecipientByUser{
			UserId: user.ID,
		}).Exec(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to fetch user rooms.",
				"error":   err.Error(),
			})
		}

		// Get results and handle errors properly
		if err := q.Select(&userRooms); err != nil {
			q.Release()
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to get user rooms.",
				"error":   err.Error(),
			})
		}
		q.Release()

		// Then, for each room, check if the other user is also a recipient
		for _, userRoom := range userRooms {
			// Get the room details to check recipients
			var room types.Room
			roomQ := models.RoomTable.SelectQuery(*database.Session)
			if err := roomQ.BindMap(map[string]interface{}{
				"id": userRoom.RoomId,
			}).Exec(); err != nil {
				continue // Skip if we can't get this room
			}

			if err := roomQ.Get(&room); err != nil {
				roomQ.Release()
				continue // Skip if we can't get this room
			}
			roomQ.Release()

			// Check if this is a PM and contains the recipient we're looking for
			if room.Type == types.RoomTypePM && len(room.Recipients) == 2 {
				// Check if the other recipient is the one we're trying to create a PM with
				for _, recipient := range room.Recipients {
					if recipient == body.Recipients[0] {
						return c.Status(fiber.StatusConflict).JSON(fiber.Map{
							"message": "A direct message room already exists with this recipient.",
						})
					}
				}
			}
		}
	}

	// Generate a new room ID
	roomID := helpers.GenerateRoomID().String()

	// Create the room object
	room := types.Room{
		ID:            roomID,
		Recipients:    append(body.Recipients, user.ID),
		LastMessageId: fmt.Sprint(-1),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	fmt.Println(body)

	// Set creator and type based on whether it's a group chat
  if body.IsGroup {
    room.Creator = &user.ID
    room.Type = types.RoomTypeGroupPM
  } else {
    // If there's only one recipient (plus the creator), it's a PM
    room.Type = types.RoomTypePM
    // For PMs, the creator should still be set to identify who initiated the conversation
    room.Creator = &user.ID
  }

	// Insert the room into the database
	q := models.RoomTable.InsertQuery(*database.Session)
	if err := q.BindStruct(room).ExecRelease(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to create room.",
			"error":   err.Error(),
		})
	}
	// Create RoomRecipientByUser entries for each recipient including the creator
	now := time.Now()
	for _, recipientID := range room.Recipients {
		recipientEntry := models.RoomRecipientByUser{
			UserId:    recipientID,
			RoomId:    roomID,
			CreatedAt: now,
			LastSeen:  now,
		}

		q := models.RoomRecipientByUserTable.InsertQuery(*database.Session)
		if err := q.BindStruct(recipientEntry).ExecRelease(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to create room recipient entry",
				"error":   err.Error(),
			})
		}
	}

	// Publish room creation event to Redis
	eventData := map[string]interface{}{
		"type": "ROOM_CREATE",
		"data": room,
	}

	eventBytes, err := json.Marshal(eventData)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to create room event",
			"error":   err.Error(),
		})
	}

	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventBytes)).Err(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish room event",
			"error":   err.Error(),
		})
	}

	return c.Status(fiber.StatusCreated).JSON(room)
}
