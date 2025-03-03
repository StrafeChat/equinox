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
		room.Type = types.RoomTypeGroupDM
	} else {
		// If there's only one recipient (plus the creator), it's a DM
		room.Type = types.RoomTypeDM
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
