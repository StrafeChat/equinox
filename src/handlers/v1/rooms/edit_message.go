package handlers_v1

import (
	"encoding/json"
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type EditMessageInput struct {
	Content string `json:"content" validate:"required"`
}

func EditMessage(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	body := new(EditMessageInput)
	roomID := c.Params("roomID")
	messageID := c.Params("messageID")

	log.Printf("EditMessage: Started for roomID=%s, messageID=%s, userID=%s", roomID, messageID, user.ID)

	if roomID == "" || messageID == "" {
		log.Printf("EditMessage: Missing required parameters")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID and Message ID are required",
		})
	}

	if err := c.Bind().Body(body); err != nil {
		log.Printf("EditMessage: Invalid request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
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

	// Get the message to check ownership
	var message models.Message
	if err := models.MessageTable.SelectBuilder().
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{"id": messageID}).
		GetRelease(&message); err != nil {
		log.Printf("EditMessage: Failed to fetch message: %v", err)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "Message not found",
		})
	}

	// Check if user is the author of the message
	if *message.AuthorID != user.ID {
		log.Printf("EditMessage: User %s is not the author of message %s", user.ID, messageID)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You can only edit your own messages",
		})
	}

	// Parse mentions from the new content
	mentions := utils.ParseMentions(body.Content)
	
	// Check if user has permission to mention @everyone
	canMentionEveryone := false
	if mentions.MentionEveryone {
		userPermissions, err := getUserPermissionsForRoom(roomID, user.ID)
		if err != nil {
			log.Printf("EditMessage: Failed to get user permissions: %v", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to check permissions",
			})
		}
		canMentionEveryone = utils.HasPermission(userPermissions, utils.MENTION_EVERYONE)
	}
	
	// If user tries to mention @everyone without permission, remove it
	if mentions.MentionEveryone && !canMentionEveryone {
		mentions.MentionEveryone = false
	}

	// Update the message content and mentions
	editedAt := time.Now()
	if err := models.MessageTable.UpdateBuilder().
		Set("content", "edited_at", "mention_everyone", "mention_roles", "mention_rooms", "mentions").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"id":               messageID,
			"content":          body.Content,
			"edited_at":        editedAt,
			"mention_everyone": mentions.MentionEveryone,
			"mention_roles":    mentions.RoleMentions,
			"mention_rooms":    mentions.RoomMentions,
			"mentions":         mentions.UserMentions,
			"created_at":       message.CreatedAt, // Include created_at from the original message as it's part of the primary key
		}).ExecRelease(); err != nil {
		log.Printf("EditMessage: Error updating message: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to update message",
		})
	}

	// Get the updated message to return to the client
	var updatedMessage models.Message
	if err := models.MessageTable.SelectBuilder().
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{"id": messageID}).
		GetRelease(&updatedMessage); err != nil {
		log.Printf("EditMessage: Failed to fetch updated message: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch updated message",
		})
	}

	// Publish message edit event to Redis for Stargate
	event := map[string]interface{}{
		"type": "MESSAGE_EDIT",
		"data": map[string]interface{}{
			"id":        messageID,
			"room_id":   roomID,
			"content":   body.Content,
			"edited_at": editedAt,
			"author_id": user.ID,
		},
	}

	log.Printf("EditMessage: Preparing to publish message edit event to Redis")
	eventJson, err := json.Marshal(event)
	if err != nil {
		log.Printf("EditMessage: Error marshaling message edit event: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to marshal message edit event",
		})
	}

	log.Printf("EditMessage: Publishing message edit event to Redis ROOM_EVENTS channel")
	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventJson)).Err(); err != nil {
		log.Printf("EditMessage: Error publishing message edit event: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish message edit event",
		})
	}

	log.Printf("EditMessage: Successfully completed for messageID=%s", messageID)
	return c.Status(fiber.StatusOK).JSON(updatedMessage)
}
