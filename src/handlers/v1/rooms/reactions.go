package handlers_v1

import (
	"encoding/json"
	"log"
	"net/url"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/events"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

type AddReactionInput struct {
	Emoji string `json:"emoji" validate:"required,min=1,max=50"`
}

type ReactionEventData struct {
	Type      string   `json:"type"`
	MessageID string   `json:"message_id"`
	RoomID    string   `json:"room_id"`
	UserID    string   `json:"user_id"`
	Emoji     string   `json:"emoji"`
	Count     int      `json:"count,omitempty"`
	Users     []string `json:"users,omitempty"`
}

// AddReaction adds a reaction to a message
func AddReaction(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	userID := user.ID
	roomID := c.Params("room_id")
	messageID := c.Params("message_id")

	// Check if user has access to the room
	hasAccess, err := checkRoomAccess(roomID, userID, utils.READ_MESSAGE_HISTORY)
	if err != nil {
		log.Printf("AddReaction: Error checking room access: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Internal server error"})
	}
	if !hasAccess {
		return c.Status(403).JSON(fiber.Map{"error": "Access denied"})
	}

	// Parse request body
	var input AddReactionInput
	if err := c.Bind().Body(&input); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid request body"})
	}

	// Validate emoji (basic validation)
	if len(input.Emoji) == 0 || len(input.Emoji) > 100 {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid emoji"})
	}

	// Check if message exists
	messageIDInt, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid message ID"})
	}

	var message models.Message
	if err := models.MessageTable.SelectBuilder().
		Columns("id", "room_id").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{"id": messageIDInt}).
		GetRelease(&message); err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Message not found"})
	}

	// Verify message is in the correct room
	if message.RoomID != roomID {
		return c.Status(400).JSON(fiber.Map{"error": "Message not in specified room"})
	}

	createdAt := time.Now().UTC().Format(time.RFC3339)

	// Check if reaction already exists
	var existingReaction models.MessageReaction
	exists := true
	if err := models.MessageReactionTable.SelectBuilder().
		Columns("message_id", "user_id", "emoji").
		Where(qb.Eq("message_id"), qb.Eq("user_id"), qb.Eq("emoji")).
		Query(*database.Session).
		BindMap(qb.M{
			"message_id": messageID,
			"user_id":    userID,
			"emoji":      input.Emoji,
		}).
		GetRelease(&existingReaction); err != nil {
		exists = false
	}

	if exists {
		return c.Status(409).JSON(fiber.Map{"error": "Reaction already exists"})
	}

	// Insert into message_reactions table
	if err := models.MessageReactionTable.InsertBuilder().
		Query(*database.Session).
		BindMap(qb.M{
			"message_id": messageID,
			"user_id":    userID,
			"emoji":      input.Emoji,
			"created_at": createdAt,
		}).
		ExecRelease(); err != nil {
		log.Printf("AddReaction: Failed to insert reaction: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to add reaction"})
	}

	// Insert into message_reactions_by_message table
	if err := models.MessageReactionByMessageTable.InsertBuilder().
		Query(*database.Session).
		BindMap(qb.M{
			"message_id": messageID,
			"emoji":      input.Emoji,
			"user_id":    userID,
			"created_at": createdAt,
		}).
		ExecRelease(); err != nil {
		log.Printf("AddReaction: Failed to insert reaction by message: %v", err)
		// Continue anyway, as the main reaction was inserted
	}

	// Update reaction count
	if err := updateReactionCount(messageID, input.Emoji); err != nil {
		log.Printf("AddReaction: Failed to update reaction count: %v", err)
	}

	// Get updated reaction count for event
	count, users, _ := getReactionCount(messageID, input.Emoji)

	// Publish reaction event with proper structure for Stargate
	eventData := map[string]interface{}{
		"type":       "ROOM_REACTION_ADD",
		"message_id": messageID,
		"room_id":    roomID,
		"user_id":    userID,
		"sender_id":  userID, // Add sender_id for Stargate compatibility
		"emoji":      input.Emoji,
		"count":      count,
		"users":      users,
		"created_at": time.Now().Unix(),
	}

	eventJSON, _ := json.Marshal(eventData)
	if err := events.PublishEvent("ROOM_REACTION_ADD", string(eventJSON)); err != nil {
		log.Printf("AddReaction: Failed to publish event: %v", err)
	}

	return c.Status(201).JSON(fiber.Map{
		"message": "Reaction added successfully",
		"emoji":   input.Emoji,
		"count":   count,
		"users":   users,
	})
}

// RemoveReaction removes a reaction from a message
func RemoveReaction(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	userID := user.ID
	roomID := c.Params("room_id")
	messageID := c.Params("message_id")
	emoji := c.Params("emoji")

	// URL decode the emoji parameter to handle Unicode emojis properly
	decodedEmoji, err := url.QueryUnescape(emoji)
	if err != nil {
		log.Printf("RemoveReaction: Failed to decode emoji parameter: %v", err)
		// Use original emoji if decoding fails
		decodedEmoji = emoji
	}
	emoji = decodedEmoji

	log.Printf("RemoveReaction: Processing emoji removal - original: %s, decoded: %s", c.Params("emoji"), emoji)

	// Check if user has access to the room
	hasAccess, err := checkRoomAccess(roomID, userID, utils.READ_MESSAGE_HISTORY)
	if err != nil {
		log.Printf("RemoveReaction: Error checking room access: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Internal server error"})
	}
	if !hasAccess {
		return c.Status(403).JSON(fiber.Map{"error": "Access denied"})
	}

	// Check if reaction exists
	var existingReaction models.MessageReaction
	if err := models.MessageReactionTable.SelectBuilder().
		Columns("message_id", "user_id", "emoji").
		Where(qb.Eq("message_id"), qb.Eq("user_id"), qb.Eq("emoji")).
		Query(*database.Session).
		BindMap(qb.M{
			"message_id": messageID,
			"user_id":    userID,
			"emoji":      emoji,
		}).
		GetRelease(&existingReaction); err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Reaction not found"})
	}

	// Delete from message_reactions table
	if err := models.MessageReactionTable.DeleteBuilder().
		Where(qb.Eq("message_id"), qb.Eq("user_id"), qb.Eq("emoji")).
		Query(*database.Session).
		BindMap(qb.M{
			"message_id": messageID,
			"user_id":    userID,
			"emoji":      emoji,
		}).
		ExecRelease(); err != nil {
		log.Printf("RemoveReaction: Failed to delete reaction: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to remove reaction"})
	}

	// Delete from message_reactions_by_message table
	if err := models.MessageReactionByMessageTable.DeleteBuilder().
		Where(qb.Eq("message_id"), qb.Eq("emoji"), qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"message_id": messageID,
			"emoji":      emoji,
			"user_id":    userID,
		}).
		ExecRelease(); err != nil {
		log.Printf("RemoveReaction: Failed to delete reaction by message: %v", err)
	}

	// Update reaction count
	if err := updateReactionCount(messageID, emoji); err != nil {
		log.Printf("RemoveReaction: Failed to update reaction count: %v", err)
	}

	// Get updated reaction count for event
	count, users, _ := getReactionCount(messageID, emoji)

	// Publish reaction event with proper structure for Stargate
	eventData := map[string]interface{}{
		"type":       "ROOM_REACTION_REMOVE",
		"message_id": messageID,
		"room_id":    roomID,
		"user_id":    userID,
		"sender_id":  userID, // Add sender_id for Stargate compatibility
		"emoji":      emoji,
		"count":      count,
		"users":      users,
		"created_at": time.Now().Unix(),
	}

	eventJSON, _ := json.Marshal(eventData)
	if err := events.PublishEvent("ROOM_REACTION_REMOVE", string(eventJSON)); err != nil {
		log.Printf("RemoveReaction: Failed to publish event: %v", err)
	}

	return c.Status(200).JSON(fiber.Map{
		"message": "Reaction removed successfully",
		"emoji":   emoji,
		"count":   count,
		"users":   users,
	})
}

// GetMessageReactions gets all reactions for a message
func GetMessageReactions(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	userID := user.ID
	roomID := c.Params("room_id")
	messageID := c.Params("message_id")

	log.Printf("GetMessageReactions: Getting reactions for message %s in room %s", messageID, roomID)

	// Check if user has access to the room
	hasAccess, err := checkRoomAccess(roomID, userID, utils.READ_MESSAGE_HISTORY)
	if err != nil {
		log.Printf("GetMessageReactions: Error checking room access: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Internal server error"})
	}
	if !hasAccess {
		return c.Status(403).JSON(fiber.Map{"error": "Access denied"})
	}

	// Get all reactions for this message from message_reactions_by_message table
	var reactionsList []models.MessageReactionByMessage
	if err := models.MessageReactionByMessageTable.SelectBuilder().
		Columns("emoji", "user_id").
		Where(qb.Eq("message_id")).
		Query(*database.Session).
		BindMap(qb.M{"message_id": messageID}).
		SelectRelease(&reactionsList); err != nil {
		log.Printf("GetMessageReactions: Failed to get reactions: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to get reactions"})
	}

	log.Printf("GetMessageReactions: Found %d individual reactions for message %s", len(reactionsList), messageID)

	// Aggregate reactions by emoji
	emojiCounts := make(map[string]map[string]interface{})
	for _, reaction := range reactionsList {
		if _, exists := emojiCounts[reaction.Emoji]; !exists {
			emojiCounts[reaction.Emoji] = map[string]interface{}{
				"count": 0,
				"users": []string{},
			}
		}

		// Increment count
		emojiCounts[reaction.Emoji]["count"] = emojiCounts[reaction.Emoji]["count"].(int) + 1

		// Add user to users list
		users := emojiCounts[reaction.Emoji]["users"].([]string)
		users = append(users, reaction.UserID)
		emojiCounts[reaction.Emoji]["users"] = users
	}

	// Format response
	reactions := make(map[string]interface{})
	for emoji, data := range emojiCounts {
		count := data["count"].(int)
		users := data["users"].([]string)
		log.Printf("GetMessageReactions: Reaction %s has count %d and %d users", emoji, count, len(users))
		if count > 0 {
			reactions[emoji] = fiber.Map{
				"count": count,
				"users": users,
			}
		}
	}

	log.Printf("GetMessageReactions: Returning %d reactions for message %s", len(reactions), messageID)
	return c.Status(200).JSON(reactions)
}

// updateReactionCount updates the reaction count for a specific emoji on a message
func updateReactionCount(messageID, emoji string) error {
	// Get all users who reacted with this emoji
	var reactions []models.MessageReactionByMessage
	if err := models.MessageReactionByMessageTable.SelectBuilder().
		Columns("user_id").
		Where(qb.Eq("message_id"), qb.Eq("emoji")).
		Query(*database.Session).
		BindMap(qb.M{
			"message_id": messageID,
			"emoji":      emoji,
		}).
		SelectRelease(&reactions); err != nil {
		return err
	}

	// Extract user IDs
	users := make([]string, len(reactions))
	for i, reaction := range reactions {
		users[i] = reaction.UserID
	}

	count := len(users)

	if count == 0 {
		// Delete the count record if no reactions
		if err := models.MessageReactionCountTable.DeleteBuilder().
			Where(qb.Eq("message_id"), qb.Eq("emoji")).
			Query(*database.Session).
			BindMap(qb.M{
				"message_id": messageID,
				"emoji":      emoji,
			}).
			ExecRelease(); err != nil {
			return err
		}
	} else {
		// Update or insert the count record
		if err := models.MessageReactionCountTable.InsertBuilder().
			Query(*database.Session).
			BindMap(qb.M{
				"message_id": messageID,
				"emoji":      emoji,
				"count":      count,
				"users":      users,
			}).
			ExecRelease(); err != nil {
			return err
		}
	}

	return nil
}

// getReactionCount gets the current count and users for a reaction
func getReactionCount(messageID, emoji string) (int, []string, error) {
	var reactionCount models.MessageReactionCount
	if err := models.MessageReactionCountTable.SelectBuilder().
		Columns("count", "users").
		Where(qb.Eq("message_id"), qb.Eq("emoji")).
		Query(*database.Session).
		BindMap(qb.M{
			"message_id": messageID,
			"emoji":      emoji,
		}).
		GetRelease(&reactionCount); err != nil {
		return 0, []string{}, err
	}

	return reactionCount.Count, reactionCount.Users, nil
}
