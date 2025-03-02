package handlers_v1

import (
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type CreateMessageInput struct {
	Content string `json:"content" validate:"required"`
	Nonce   string `json:"nonce"`
}

type GetMessagesQuery struct {
	Limit  int    `query:"limit"`
	Before string `query:"before"`
	After  string `query:"after"`
	Around string `query:"around"`
}

func GetRoomMessages(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")
	query := new(GetMessagesQuery)

	log.Printf("GetRoomMessages: Started for roomID=%s, userID=%s", roomID, user.ID)

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	if err := c.Bind().Query(query); err != nil {
		log.Printf("GetRoomMessages: Invalid query parameters: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid query parameters",
		})
	}

	log.Printf("GetRoomMessages: Query parameters - Limit: %d, Before: %s, After: %s, Around: %s",
		query.Limit, query.Before, query.After, query.Around)

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

	// Set default limit if not provided or if it exceeds maximum
	if query.Limit <= 0 || query.Limit > 100 {
		query.Limit = 50
		log.Printf("GetRoomMessages: Adjusted limit to default: %d", query.Limit)
	}

	// Build the query based on the parameters
	selectBuilder := models.MessagesByRoomTable.SelectBuilder().
		Columns("room_id", "id", "created_at").
		Where(qb.Eq("room_id")).
		OrderBy("id", qb.DESC).
		Limit(uint(query.Limit))

	if query.Before != "" {
		selectBuilder = selectBuilder.Where(qb.Lt("id"))
	} else if query.After != "" {
		selectBuilder = selectBuilder.Where(qb.Gt("id")).
			OrderBy("id", qb.ASC)
	} else if query.Around != "" {
		// For 'around', we'll fetch messages before and after the specified ID
		halfLimit := query.Limit / 2
		log.Printf("GetRoomMessages: Using 'around' mode with ID %s, half limit: %d", query.Around, halfLimit)

		// Get messages before the specified ID
		beforeBuilder := models.MessagesByRoomTable.SelectBuilder().
			Columns("room_id", "id", "created_at").
			Where(qb.Eq("room_id"), qb.Lt("id")).
			Limit(uint(halfLimit))

		// Get messages after the specified ID
		afterBuilder := models.MessagesByRoomTable.SelectBuilder().
			Columns("room_id", "id", "created_at").
			Where(qb.Eq("room_id"), qb.Gt("id")).
			Limit(uint(halfLimit))

		var beforeMessages, afterMessages []models.MessagesByRoom

		// Execute both queries concurrently
		var wg sync.WaitGroup
		wg.Add(2)

		var beforeErr, afterErr error
		go func() {
			defer wg.Done()
			log.Printf("GetRoomMessages: Executing 'before' query for room %s, message ID %s", roomID, query.Around)
			if err := beforeBuilder.Query(*database.Session).
				BindMap(qb.M{
					"room_id": roomID,
					"id":      query.Around,
				}).
				SelectRelease(&beforeMessages); err != nil {
				log.Printf("GetRoomMessages: Error in 'before' query: %v", err)
				beforeErr = err
			} else {
				log.Printf("GetRoomMessages: 'Before' query returned %d messages", len(beforeMessages))
			}
		}()

		go func() {
			defer wg.Done()
			log.Printf("GetRoomMessages: Executing 'after' query for room %s, message ID %s", roomID, query.Around)
			if err := afterBuilder.Query(*database.Session).
				BindMap(qb.M{
					"room_id": roomID,
					"id":      query.Around,
				}).
				SelectRelease(&afterMessages); err != nil {
				log.Printf("GetRoomMessages: Error in 'after' query: %v", err)
				afterErr = err
			} else {
				log.Printf("GetRoomMessages: 'After' query returned %d messages", len(afterMessages))
			}
		}()

		wg.Wait()

		// Check for errors in concurrent queries
		if beforeErr != nil || afterErr != nil {
			log.Printf("Error fetching messages: before=%v, after=%v", beforeErr, afterErr)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to fetch messages",
			})
		}

		// Combine the message IDs
		messageIDs := make([]string, 0, len(beforeMessages)+len(afterMessages))
		for _, msg := range beforeMessages {
			messageIDs = append(messageIDs, msg.ID)
		}
		for _, msg := range afterMessages {
			messageIDs = append(messageIDs, msg.ID)
		}

		log.Printf("GetRoomMessages: Combined %d message IDs for full retrieval", len(messageIDs))

		// If no messages found, return empty array
		if len(messageIDs) == 0 {
			log.Printf("GetRoomMessages: No messages found for 'around' query")
			return c.Status(fiber.StatusOK).JSON(fiber.Map{
				"messages": []models.Message{},
			})
		}

		// Fetch full message details from messages table
		var fullMessages []models.Message
		log.Printf("GetRoomMessages: Fetching full message details for %d message IDs", len(messageIDs))

		// Convert string IDs to int64 for database query
		var messageIDsInt []int64
		for _, id := range messageIDs {
			messageIDInt, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				log.Printf("GetRoomMessages: Error converting message ID to int64: %v", err)
				continue
			}
			messageIDsInt = append(messageIDsInt, messageIDInt)
		}

		// Build the query for fetching messages
		queryBuilder := models.MessageTable.SelectBuilder().Columns("id", "content", "author_id", "room_id", "created_at", "nonce", "space_id", "system", "tts", "attachments", "embeds", "flags", "mention_everyone", "mention_roles", "mention_rooms", "mentions", "message_references", "pinned")

		// Fetch each message individually
		for _, msgID := range messageIDsInt {
			var msg models.Message
			if err := queryBuilder.Where(qb.Eq("id")).
				Query(*database.Session).
				BindMap(qb.M{"id": msgID}).
				GetRelease(&msg); err != nil {
				log.Printf("GetRoomMessages: Error fetching message ID %d: %v", msgID, err)
				continue
			}
			fullMessages = append(fullMessages, msg)
		}

		// Check if we failed to fetch any messages
		if len(fullMessages) == 0 {
			log.Printf("GetRoomMessages: Error fetching full message details")
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to fetch full message details",
			})
		}

		log.Printf("GetRoomMessages: Successfully retrieved %d full messages", len(fullMessages))
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"messages": fullMessages,
		})
	}

	// Execute the query for before/after/default cases
	var messagesByRoom []models.MessagesByRoom
	queryParams := qb.M{"room_id": roomID}

	if query.Before != "" {
		queryParams["id"] = query.Before
		log.Printf("GetRoomMessages: Using 'before' mode with ID %s", query.Before)
	} else if query.After != "" {
		queryParams["id"] = query.After
		log.Printf("GetRoomMessages: Using 'after' mode with ID %s", query.After)
	} else {
		log.Printf("GetRoomMessages: Using default mode (no before/after/around)")
	}

	log.Printf("GetRoomMessages: Executing main query for room %s", roomID)
	if err := selectBuilder.Query(*database.Session).
		BindMap(queryParams).
		SelectRelease(&messagesByRoom); err != nil {
		log.Printf("GetRoomMessages: Error in main query: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch messages",
		})
	}

	log.Printf("GetRoomMessages: Main query returned %d messages", len(messagesByRoom))

	// If no messages found, return empty array
	if len(messagesByRoom) == 0 {
		log.Printf("GetRoomMessages: No messages found for main query")
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"messages": []models.Message{},
		})
	}

	// Extract message IDs
	messageIDs := make([]string, len(messagesByRoom))
	for i, msg := range messagesByRoom {
		messageIDs[i] = msg.ID
	}

	log.Printf("GetRoomMessages: Extracted %d message IDs for full retrieval", len(messageIDs))

	// Convert string IDs to int64 for database query
	var messageIDsInt []int64
	for _, id := range messageIDs {
		messageIDInt, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			log.Printf("GetRoomMessages: Error converting message ID to int64: %v", err)
			continue
		}
		messageIDsInt = append(messageIDsInt, messageIDInt)
	}

	// Fetch full message details from messages table
	var fullMessages []models.Message
	log.Printf("GetRoomMessages: Fetching full message details for %d message IDs", len(messageIDs))

	// Create a query builder for the messages table
	queryBuilder := models.MessageTable.SelectBuilder().
		Columns("id", "content", "author_id", "room_id", "created_at", "nonce", "space_id", "system", "tts", "attachments", "embeds", "flags", "mention_everyone", "mention_roles", "mention_rooms", "mentions", "message_references", "pinned")

	// Build individual queries for each message ID
	if len(messageIDsInt) > 0 {
		for _, msgID := range messageIDsInt {
			var msg models.Message
			if err := queryBuilder.Where(qb.Eq("id")).
				Query(*database.Session).
				BindMap(qb.M{"id": msgID}).
				GetRelease(&msg); err != nil {
				continue
			}
			fullMessages = append(fullMessages, msg)
		}
	}

	// Check if we failed to fetch any messages
	if len(fullMessages) == 0 {
		log.Printf("GetRoomMessages: Error fetching full message details")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch full message details",
		})
	}

	log.Printf("GetRoomMessages: Successfully retrieved %d full messages", len(fullMessages))

	// Reverse the order for 'after' queries to maintain chronological order
	if query.After != "" {
		log.Printf("GetRoomMessages: Reversing message order for 'after' query")
		for i, j := 0, len(fullMessages)-1; i < j; i, j = i+1, j-1 {
			fullMessages[i], fullMessages[j] = fullMessages[j], fullMessages[i]
		}
	}

	log.Printf("GetRoomMessages: Completed successfully, returning %d messages", len(fullMessages))
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"messages": fullMessages,
	})
}

func CreateMessage(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	body := new(CreateMessageInput)
	roomID := c.Params("id")

	log.Printf("CreateMessage: Started for roomID=%s, userID=%s", roomID, user.ID)

	if roomID == "" {
		log.Printf("CreateMessage: Missing room ID")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	if err := c.Bind().Body(body); err != nil {
		log.Printf("CreateMessage: Invalid request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
		})
	}
	// Check if user has access to the room
	var roomRecipients []models.RoomRecipientByUser
	log.Printf("CreateMessage: Checking room access for user %s", user.ID)
	if err := models.RoomRecipientByUserTable.SelectBuilder().
		Columns("user_id", "room_id", "created_at", "last_seen").
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"user_id": user.ID,
		}).
		SelectRelease(&roomRecipients); err != nil {
		log.Printf("CreateMessage: Failed to check room access: %v", err)
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

	log.Printf("CreateMessage: User %s has access to room %s: %v", user.ID, roomID, hasAccess)
	if !hasAccess {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have access to this room",
		})
	}
	// Create message
	messageID := helpers.GenerateMessageID().String()
	createdAt := time.Now()

	log.Printf("CreateMessage: Generated messageID=%s", messageID)

	message := models.Message{
		ID:        messageID,
		Content:   &body.Content,
		Nonce:     &body.Nonce,
		AuthorID:  user.ID,
		RoomID:    roomID,
		CreatedAt: createdAt,
		// UpdatedAt: createdAt,
	}

	var wg sync.WaitGroup
	errChan := make(chan error, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("CreateMessage: Inserting into messages table for messageID=%s", messageID)
		if err := models.MessageTable.InsertQuery(*database.Session).BindStruct(&message).ExecRelease(); err != nil {
			log.Printf("CreateMessage: Error inserting into messages table: %v", err)
			errChan <- err
			return
		}
		log.Printf("CreateMessage: Successfully inserted into messages table")
		errChan <- nil
	}()

	// Insert into messages_by_room table concurrently
	messageByRoom := models.MessagesByRoom{
		RoomID:    roomID,
		ID:        messageID,
		CreatedAt: createdAt,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("CreateMessage: Inserting into messages_by_room table for roomID=%s, messageID=%s", roomID, messageID)
		if err := models.MessagesByRoomTable.InsertQuery(*database.Session).BindStruct(&messageByRoom).ExecRelease(); err != nil {
			log.Printf("CreateMessage: Error inserting into messages_by_room table: %v", err)
			errChan <- err
			return
		}
		log.Printf("CreateMessage: Successfully inserted into messages_by_room table")
		errChan <- nil
	}()

	// Update room's last_message_id concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("CreateMessage: Updating room's last_message_id for roomID=%s", roomID)
		if err := models.RoomTable.UpdateBuilder().
			Set("last_message_id", "updated_at").
			Where(qb.Eq("id")).
			Query(*database.Session).
			BindMap(qb.M{
				"last_message_id": messageID,
				"updated_at":      createdAt,
				"id":              roomID,
			}).ExecRelease(); err != nil {
			log.Printf("CreateMessage: Error updating room's last_message_id: %v", err)
			errChan <- err
			return
		}
		log.Printf("CreateMessage: Successfully updated room's last_message_id")
		errChan <- nil
	}()

	// Wait for all goroutines to complete
	wg.Wait()
	// Close the error channel
	close(errChan)

	// Check if any errors occurred
	var errors []error
	for err := range errChan {
		if err != nil {
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		// Log the errors for debugging
		for _, err := range errors {
			log.Printf("Error in message operation: %v", err)
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to process message operation",
		})
	}

	// Publish message to Redis for Stargate
	event := map[string]interface{}{
		"type": "MESSAGE_CREATE",
		"data": message,
	}

	log.Printf("CreateMessage: Preparing to publish message event to Redis")
	eventJson, err := json.Marshal(event)
	if err != nil {
		log.Printf("CreateMessage: Error marshaling message event: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to marshal message event",
		})
	}

	log.Printf("CreateMessage: Publishing message event to Redis ROOM_EVENTS channel")
	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventJson)).Err(); err != nil {
		log.Printf("CreateMessage: Error publishing message event: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish message event",
		})
	}

	log.Printf("CreateMessage: Successfully completed for messageID=%s", messageID)
	return c.Status(fiber.StatusCreated).JSON(message)
}
