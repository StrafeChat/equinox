package handlers_v1

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

// GetUnreadMessages returns all unread messages for a user in a specific room
func GetUnreadMessages(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	var unreads []models.MessageUnread
	if err := models.MessageUnreadTable.SelectBuilder().
		Where(qb.Eq("user_id"), qb.Eq("room_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"user_id": user.ID,
			"room_id": roomID,
		}).
		SelectRelease(&unreads); err != nil {
		log.Printf("GetUnreadMessages: Failed to fetch unread messages: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch unread messages",
		})
	}

	return c.JSON(unreads)
}

// MarkMessagesAsRead marks all messages in a room as read for the user
func AcknowledgeMessages(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	// Get all unread messages for this user in this room
	var unreads []models.MessageUnread
	if err := models.MessageUnreadTable.SelectBuilder().
		Where(qb.Eq("user_id"), qb.Eq("room_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"user_id": user.ID,
			"room_id": roomID,
		}).
		SelectRelease(&unreads); err != nil {
		log.Printf("AcknowledgeMessages: Failed to fetch unread messages: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch unread messages",
		})
	}

	// Delete each unread message individually with the correct primary key
	for _, unread := range unreads {
		if err := models.MessageUnreadTable.DeleteBuilder().
			Where(qb.Eq("user_id"), qb.Eq("room_id"), qb.Eq("message_id")).
			Query(*database.Session).
			BindMap(qb.M{
				"user_id":    user.ID,
				"room_id":    roomID,
				"message_id": unread.MessageID,
			}).
			ExecRelease(); err != nil {
			log.Printf("AcknowledgeMessages: Failed to mark message %s as read: %v", unread.MessageID, err)
			// Continue with other messages even if one fails
			continue
		}
	}

	return c.SendStatus(fiber.StatusOK)
}

// AddUnreadMessage adds a message to the unread list for all recipients except the sender
func AddUnreadMessage(roomID string, messageID string, senderID string) error {
	// Get all recipients of the room
	var room models.Room
	if err := models.RoomTable.GetQuery(*database.Session).
		BindMap(qb.M{"id": roomID}).
		Get(&room); err != nil {
		return err
	}

	// Add unread entry for each recipient except the sender and online users
	for _, recipientID := range room.Recipients {
		recipientIDStr := strconv.FormatInt(recipientID, 10)
		if recipientIDStr == senderID {
			continue
		}

		// Check if user is online via Redis
		onlineStatus, err := database.Rdb.Get(fmt.Sprintf("user:%s:online", recipientIDStr)).Result()
		if err == nil && onlineStatus == "true" {
			// Skip creating unread entry for online users
			continue
		}

		unread := models.MessageUnread{
			UserID:    recipientIDStr,
			RoomID:    roomID,
			MessageID: messageID,
			CreatedAt: time.Now(),
		}

		if err := models.MessageUnreadTable.InsertQuery(*database.Session).
			BindStruct(unread).
			Exec(); err != nil {
			log.Printf("AddUnreadMessage: Failed to add unread message for user %s: %v", recipientIDStr, err)
		}
	}

	return nil
}
