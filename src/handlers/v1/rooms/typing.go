package handlers_v1

import (
	"encoding/json"
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

// HandleTypingIndicator processes typing indicator events and broadcasts them via Redis
func HandleTypingIndicator(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")

	log.Printf("HandleTypingIndicator: Started for roomID=%s, userID=%s", roomID, user.ID)

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	// Check if user has access to the room
	var roomRecipients []models.RoomRecipientByUser
	log.Printf("HandleTypingIndicator: Checking room access for user %s", user.ID)
	if err := models.RoomRecipientByUserTable.SelectBuilder().
		Columns("user_id", "room_id").
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"user_id": user.ID,
		}).
		SelectRelease(&roomRecipients); err != nil {
		log.Printf("HandleTypingIndicator: Failed to check room access: %v", err)
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

	log.Printf("HandleTypingIndicator: User %s has access to room %s: %v", user.ID, roomID, hasAccess)
	if !hasAccess {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have access to this room",
		})
	}

	// Create typing event payload
	typingEvent := map[string]interface{}{
		"type":       "TYPING_START",
		"sender_id":  user.ID,
		"room_id":    roomID,
		"created_at": time.Now().UnixMilli(),
	}

	// Convert to JSON
	typingEventJSON, err := json.Marshal(typingEvent)
	if err != nil {
		log.Printf("HandleTypingIndicator: Failed to marshal typing event: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to process typing event",
		})
	}

	// Publish to Redis
	if err := database.Rdb.Publish("ROOM_EVENTS", string(typingEventJSON)).Err(); err != nil {
		log.Printf("HandleTypingIndicator: Failed to publish typing event: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to broadcast typing event",
		})
	}

	log.Printf("HandleTypingIndicator: Successfully published typing event for user %s in room %s", user.ID, roomID)

	return c.SendStatus(fiber.StatusNoContent)
}
