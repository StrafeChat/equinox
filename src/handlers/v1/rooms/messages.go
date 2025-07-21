package handlers_v1

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/e2ee"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/repository"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

var e2eeService *e2ee.E2EEService

// getE2EEService returns the E2EE service, initializing it if necessary
func getE2EEService() *e2ee.E2EEService {
	if e2eeService == nil {
		e2eeService = e2ee.NewE2EEService()
	}
	return e2eeService
}

type CreateMessageInput struct {
	Content           string   `json:"content"`
	Nonce             string   `json:"nonce"`
	MessageReferences []string `json:"message_references"`
	Attachments       []string `json:"attachments"`
}

type GetMessagesQuery struct {
	Limit  int    `query:"limit"`
	Before string `query:"before"`
	After  string `query:"after"`
	Around string `query:"around"`
}

// Note: checkSpaceMemberPermission function removed - now using global utils.CheckPermission

// getUserPermissionsForRoom gets all permissions for a user in a room
func getUserPermissionsForRoom(roomID, userID string) ([]string, error) {
	// First get the room details to check its type and space_id
	var room types.Room
	roomQuery := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQuery.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("getUserPermissionsForRoom: Failed to fetch room: %v", err)
		return nil, err
	}

	if err := roomQuery.Get(&room); err != nil {
		roomQuery.Release()
		log.Printf("getUserPermissionsForRoom: Failed to get room details: %v", err)
		return nil, err
	}
	roomQuery.Release()

	// For space rooms (text, voice, sections), use the new room permission override system
	if (room.Type == types.RoomTypeTextRoom || room.Type == types.RoomTypeVoiceRoom || room.Type == types.RoomTypeSpaceSection) && room.SpaceID != nil {
		roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
		permissions, err := roomPermRepo.CalculateUserPermissionsInRoom(roomID, userID, *room.SpaceID)
		if err != nil {
			log.Printf("getUserPermissionsForRoom: Failed to calculate room permissions: %v", err)
			return []string{}, nil
		}
		return permissions, nil
	}

	// For PM/Group PM types, return basic permissions
	if room.Type == types.RoomTypePM || room.Type == types.RoomTypeGroupPM {
		return []string{utils.SEND_MESSAGES, utils.READ_MESSAGE_HISTORY}, nil
	}

	// For other room types, return empty permissions
	return []string{}, nil
}

// checkRoomAccess checks if a user has access to a room based on room type
func checkRoomAccess(roomID, userID string, permission string) (bool, error) {
	// First get the room details to check its type and space_id
	var room types.Room
	roomQuery := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQuery.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("checkRoomAccess: Failed to fetch room: %v", err)
		return false, err
	}

	if err := roomQuery.Get(&room); err != nil {
		roomQuery.Release()
		log.Printf("checkRoomAccess: Failed to get room details: %v", err)
		return false, err
	}
	roomQuery.Release()

	// For space rooms (text, voice, sections), check permissions using room override system
	if (room.Type == types.RoomTypeTextRoom || room.Type == types.RoomTypeVoiceRoom || room.Type == types.RoomTypeSpaceSection) && room.SpaceID != nil {
		log.Printf("checkRoomAccess: Checking room permission %s for room %s in space %d", permission, roomID, *room.SpaceID)
		roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
		return roomPermRepo.HasPermissionInRoom(roomID, userID, permission, *room.SpaceID)
	}

	// For PM/Group PM types (type 0, 1), check recipient permissions
	if room.Type == types.RoomTypePM || room.Type == types.RoomTypeGroupPM {
		log.Printf("checkRoomAccess: Checking recipient access for PM/Group PM room %s", roomID)
		var roomRecipients []models.RoomRecipientByUser
		if err := models.RoomRecipientByUserTable.SelectBuilder().
			Columns("user_id", "room_id").
			Where(qb.Eq("user_id")).
			Query(*database.Session).
			BindMap(qb.M{
				"user_id": userID,
			}).SelectRelease(&roomRecipients); err != nil {
			log.Printf("checkRoomAccess: Failed to check room recipients: %v", err)
			return false, err
		}

		// Check if the user is a recipient of the specified room
		for _, recipient := range roomRecipients {
			if recipient.RoomId == roomID {
				return true, nil
			}
		}
		return false, nil
	}

	// For other room types, deny access by default
	log.Printf("checkRoomAccess: Unsupported room type %d for room %s", room.Type, roomID)
	return false, nil
}

// fetchAttachmentMetadata fetches file metadata directly from database
func fetchAttachmentMetadata(attachmentID string) (map[string]interface{}, error) {
	log.Printf("fetchAttachmentMetadata: Fetching metadata for attachment ID: %s", attachmentID)

	// Query file metadata directly from database
	var file models.File
	if err := models.FileTable.SelectBuilder().
		Columns("id", "filename", "mime_type", "size", "width", "height", "user_id", "stored_filename").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{"id": attachmentID}).
		GetRelease(&file); err != nil {
		log.Printf("fetchAttachmentMetadata: Database query failed for ID %s: %v", attachmentID, err)
		return nil, fmt.Errorf("failed to get file metadata from database: %v", err)
	}

	log.Printf("fetchAttachmentMetadata: Found file data - ID: %s, Filename: %s, MimeType: %s, Size: %d", file.ID, file.Filename, file.MimeType, file.Size)

	// Transform the data to match the expected attachment format
	attachmentData := map[string]interface{}{
		"id":      file.ID,
		"name":    file.Filename,
		"url":     fmt.Sprintf("/attachments/%s/%s", file.UserID, file.StoredFilename),
		"type":    file.MimeType,
		"height":  0, // Default values for media dimensions
		"width":   0,
		"size":    file.Size,
		"user_id": file.UserID,
	}

	// Set actual dimensions if available
	if file.Width != nil {
		attachmentData["width"] = *file.Width
	}
	if file.Height != nil {
		attachmentData["height"] = *file.Height
	}

	log.Printf("fetchAttachmentMetadata: Returning attachment data: %+v", attachmentData)
	return attachmentData, nil
}

// fetchMessagesFromIDs fetches full message details concurrently
func fetchMessagesFromIDs(messageIDs []string) ([]models.Message, error) {
	if len(messageIDs) == 0 {
		return []models.Message{}, nil
	}

	// Convert string IDs to int64
	messageIDsInt := make([]int64, 0, len(messageIDs))
	for _, id := range messageIDs {
		messageIDInt, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			log.Printf("fetchMessagesFromIDs: Error converting message ID to int64: %v", err)
			continue
		}
		messageIDsInt = append(messageIDsInt, messageIDInt)
	}

	if len(messageIDsInt) == 0 {
		return nil, fmt.Errorf("no valid message IDs")
	}

	// Use concurrent fetching with worker pool
	const maxWorkers = 10
	workers := len(messageIDsInt)
	if workers > maxWorkers {
		workers = maxWorkers
	}

	messagesChan := make(chan models.Message, len(messageIDsInt))
	errorsChan := make(chan error, len(messageIDsInt))
	jobsChan := make(chan int64, len(messageIDsInt))

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for msgID := range jobsChan {
				var msg models.Message
				if err := models.MessageTable.SelectBuilder().
					Columns("id", "content", "author_id", "room_id", "created_at", "nonce", "space_id", "system", "tts", "attachments", "embeds", "flags", "mention_everyone", "mention_roles", "mention_rooms", "mentions", "message_references", "pinned", "edited_at", "type", "system_type", "system_data").
					Where(qb.Eq("id")).
					Query(*database.Session).
					BindMap(qb.M{"id": msgID}).
					GetRelease(&msg); err != nil {
					errorsChan <- fmt.Errorf("failed to fetch message ID %d: %v", msgID, err)
					continue
				}
				messagesChan <- msg
			}
		}()
	}

	// Send jobs
	go func() {
		defer close(jobsChan)
		for _, msgID := range messageIDsInt {
			jobsChan <- msgID
		}
	}()

	// Wait for workers to complete
	go func() {
		wg.Wait()
		close(messagesChan)
		close(errorsChan)
	}()

	// Collect results
	var fullMessages []models.Message
	var errors []error

	done := false
	for !done {
		select {
		case msg, ok := <-messagesChan:
			if !ok {
				messagesChan = nil
			} else {
				fullMessages = append(fullMessages, msg)
			}
		case err, ok := <-errorsChan:
			if !ok {
				errorsChan = nil
			} else {
				errors = append(errors, err)
			}
		}
		if messagesChan == nil && errorsChan == nil {
			done = true
		}
	}

	if len(errors) > 0 {
		log.Printf("fetchMessagesFromIDs: %d errors occurred while fetching messages", len(errors))
		for _, err := range errors {
			log.Printf("fetchMessagesFromIDs: %v", err)
		}
	}

	if len(fullMessages) == 0 {
		return nil, fmt.Errorf("failed to fetch any messages")
	}

	return fullMessages, nil
}

// fetchAuthorDetails fetches user details for message authors concurrently
func fetchAuthorDetails(authorIDs []string) (map[string]interface{}, error) {
	if len(authorIDs) == 0 {
		return make(map[string]interface{}), nil
	}

	// Remove duplicates
	uniqueIDs := make(map[string]bool)
	for _, id := range authorIDs {
		if id != "" {
			uniqueIDs[id] = true
		}
	}

	// Convert to slice for concurrent processing
	uniqueIDSlice := make([]string, 0, len(uniqueIDs))
	for id := range uniqueIDs {
		uniqueIDSlice = append(uniqueIDSlice, id)
	}

	// Use concurrent fetching with worker pool
	const maxWorkers = 10
	workers := len(uniqueIDSlice)
	if workers > maxWorkers {
		workers = maxWorkers
	}

	type authorResult struct {
		id   string
		user models.User
		err  error
	}

	resultsChan := make(chan authorResult, len(uniqueIDSlice))
	jobsChan := make(chan string, len(uniqueIDSlice))

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for authorID := range jobsChan {
				var user models.User
				err := models.UserTable.SelectBuilder().
					Columns("id", "username", "discriminator", "display_name", "avatar", "banner", "presence", "flags", "about_me", "bio", "bot").
					Where(qb.Eq("id")).
					Limit(1).
					Query(*database.Session).
					BindMap(qb.M{"id": authorID}).
					GetRelease(&user)
				resultsChan <- authorResult{id: authorID, user: user, err: err}
			}
		}()
	}

	// Send jobs
	go func() {
		defer close(jobsChan)
		for _, authorID := range uniqueIDSlice {
			jobsChan <- authorID
		}
	}()

	// Wait for workers to complete
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results
	authors := make(map[string]interface{})
	for result := range resultsChan {
		if result.err == nil {
			authors[result.id] = fiber.Map{
				"id":            result.user.ID,
				"username":      result.user.Username,
				"discriminator": result.user.Discriminator,
				"display_name":  result.user.DisplayName,
				"avatar":        result.user.Avatar,
				"banner":        result.user.Banner,
				"presence":      result.user.Presence,
				"flags":         result.user.Flags,
				"about_me":      result.user.AboutMe,
				"bio":           result.user.Bio,
				"bot":           result.user.Bot,
			}
		} else {
			log.Printf("fetchAuthorDetails: Failed to fetch user %s: %v", result.id, result.err)
		}
	}

	return authors, nil
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

	// Check if user has access to the room and permission to read message history
	log.Printf("GetRoomMessages: Checking room access and read message history permission for user %s", user.ID)
	hasAccess, err := checkRoomAccess(roomID, user.ID, utils.READ_MESSAGE_HISTORY)
	if err != nil {
		log.Printf("GetRoomMessages: Failed to check room access: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to check room access",
		})
	}

	log.Printf("GetRoomMessages: User %s has access to room %s: %v", user.ID, roomID, hasAccess)
	if !hasAccess {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have permission to read message history in this room",
		})
	}

	// Set default limit if not provided or if it exceeds maximum
	if query.Limit <= 0 || query.Limit > 100 {
		query.Limit = 50
		log.Printf("GetRoomMessages: Adjusted limit to default: %d", query.Limit)
	}

	var messageIDs []string

	// Handle different query modes
	if query.Around != "" {
		// For 'around', fetch messages before and after the specified ID concurrently
		halfLimit := query.Limit / 2
		log.Printf("GetRoomMessages: Using 'around' mode with ID %s, half limit: %d", query.Around, halfLimit)

		// Create builders for before and after queries
		beforeBuilder := models.MessagesByRoomTable.SelectBuilder().
			Columns("room_id", "id", "created_at").
			Where(qb.Eq("room_id"), qb.Lt("id")).
			Limit(uint(halfLimit))

		afterBuilder := models.MessagesByRoomTable.SelectBuilder().
			Columns("room_id", "id", "created_at").
			Where(qb.Eq("room_id"), qb.Gt("id")).
			Limit(uint(halfLimit))

		// Execute both queries concurrently
		type queryResult struct {
			messages  []models.MessagesByRoom
			err       error
			queryType string
		}

		resultsChan := make(chan queryResult, 2)

		// Before query
		go func() {
			var beforeMessages []models.MessagesByRoom
			err := beforeBuilder.Query(*database.Session).
				BindMap(qb.M{
					"room_id": roomID,
					"id":      query.Around,
				}).
				SelectRelease(&beforeMessages)
			resultsChan <- queryResult{beforeMessages, err, "before"}
		}()

		// After query
		go func() {
			var afterMessages []models.MessagesByRoom
			err := afterBuilder.Query(*database.Session).
				BindMap(qb.M{
					"room_id": roomID,
					"id":      query.Around,
				}).
				SelectRelease(&afterMessages)
			resultsChan <- queryResult{afterMessages, err, "after"}
		}()

		// Collect results
		var beforeMessages, afterMessages []models.MessagesByRoom
		for i := 0; i < 2; i++ {
			result := <-resultsChan
			if result.err != nil {
				log.Printf("GetRoomMessages: Error in '%s' query: %v", result.queryType, result.err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"message": "Failed to fetch messages",
				})
			}
			if result.queryType == "before" {
				beforeMessages = result.messages
			} else {
				afterMessages = result.messages
			}
		}

		log.Printf("GetRoomMessages: 'Before' query returned %d messages, 'After' query returned %d messages", len(beforeMessages), len(afterMessages))

		// Combine message IDs
		messageIDs = make([]string, 0, len(beforeMessages)+len(afterMessages))
		for _, msg := range beforeMessages {
			messageIDs = append(messageIDs, msg.ID)
		}
		for _, msg := range afterMessages {
			messageIDs = append(messageIDs, msg.ID)
		}
	} else {
		// Handle before/after/default cases
		selectBuilder := models.MessagesByRoomTable.SelectBuilder().
			Columns("room_id", "id", "created_at").
			Where(qb.Eq("room_id")).
			Limit(uint(query.Limit))

		queryParams := qb.M{"room_id": roomID}

		if query.Before != "" {
			selectBuilder = selectBuilder.Where(qb.Lt("id"))
			queryParams["id"] = query.Before
			log.Printf("GetRoomMessages: Using 'before' mode with ID %s", query.Before)
		} else if query.After != "" {
			selectBuilder = selectBuilder.Where(qb.Gt("id"))
			queryParams["id"] = query.After
			log.Printf("GetRoomMessages: Using 'after' mode with ID %s", query.After)
		} else {
			log.Printf("GetRoomMessages: Using default mode (no before/after/around)")
		}

		log.Printf("GetRoomMessages: Executing main query for room %s", roomID)
		var messagesByRoom []models.MessagesByRoom
		if err := selectBuilder.Query(*database.Session).
			BindMap(queryParams).
			SelectRelease(&messagesByRoom); err != nil {
			log.Printf("GetRoomMessages: Error in main query: %v", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to fetch messages",
			})
		}

		log.Printf("GetRoomMessages: Main query returned %d messages", len(messagesByRoom))

		// Extract message IDs
		messageIDs = make([]string, len(messagesByRoom))
		for i, msg := range messagesByRoom {
			messageIDs[i] = msg.ID
		}
	}

	log.Printf("GetRoomMessages: Total %d message IDs for full retrieval", len(messageIDs))

	// If no messages found, return empty array
	if len(messageIDs) == 0 {
		log.Printf("GetRoomMessages: No messages found")
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"messages": []models.Message{},
		})
	}

	// Fetch full message details concurrently
	fullMessages, err := fetchMessagesFromIDs(messageIDs)
	if err != nil {
		log.Printf("GetRoomMessages: Error fetching full message details: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch full message details",
		})
	}

	log.Printf("GetRoomMessages: Successfully retrieved %d full messages", len(fullMessages))

	// Debug: Check if any messages have attachments
	for i, msg := range fullMessages {
		if len(msg.Attachments) > 0 {
			log.Printf("GetRoomMessages: Message %d has %d attachments: %+v", i, len(msg.Attachments), msg.Attachments)
		} else {
			log.Printf("GetRoomMessages: Message %d has no attachments", i)
		}
	}

	// Reverse the order for 'after' queries to maintain chronological order
	if query.After != "" {
		log.Printf("GetRoomMessages: Reversing message order for 'after' query")
		for i, j := 0, len(fullMessages)-1; i < j; i, j = i+1, j-1 {
			fullMessages[i], fullMessages[j] = fullMessages[j], fullMessages[i]
		}
	}

	// Collect unique author IDs from messages
	authorIDs := make([]string, 0)
	for _, msg := range fullMessages {
		if msg.AuthorID != nil && *msg.AuthorID != "" {
			authorIDs = append(authorIDs, *msg.AuthorID)
		}
	}

	// Fetch author details
	authors, err := fetchAuthorDetails(authorIDs)
	if err != nil {
		log.Printf("GetRoomMessages: Error fetching author details: %v", err)
		// Continue without author details rather than failing the entire request
		authors = make(map[string]interface{})
	}

	// Get room details for E2EE decryption check
	var room types.Room
	roomQuery := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQuery.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("GetRoomMessages: Failed to fetch room: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch room details",
		})
	}

	if err := roomQuery.Get(&room); err != nil {
		roomQuery.Release()
		log.Printf("GetRoomMessages: Failed to get room details: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to get room details",
		})
	}
	roomQuery.Release()

	// Initialize E2EE service for decryption if needed
	var e2eeService *e2ee.E2EEService
	if room.Type == types.RoomTypePM || room.Type == types.RoomTypeGroupPM {
		e2eeService = e2ee.NewE2EEService()
		log.Printf("GetRoomMessages: E2EE service initialized for room type %d", room.Type)
	}

	// Convert messages to interface{} slice and add author details + process attachments + handle E2EE decryption
	messagesWithAuthors := make([]interface{}, len(fullMessages))
	for i, msg := range fullMessages {
		// Create a new map with the message data
		msgWithAuthor := make(map[string]interface{})

		// Copy all existing message fields
		msgBytes, _ := json.Marshal(msg)
		json.Unmarshal(msgBytes, &msgWithAuthor)

		// Handle E2EE decryption for PM and GROUP_PM rooms
		if e2eeService != nil && msg.Content != nil {
			var decryptedContent string
			var decryptionErr error

			if room.Type == types.RoomTypePM {
				// Direct message decryption - get sender ID
				senderID := ""
				if msg.AuthorID != nil {
					senderID = *msg.AuthorID
				}

				if senderID != "" {
					decryptedContent, decryptionErr = getE2EEService().DecryptMessage(user.ID, senderID, *msg.Content)
					if decryptionErr != nil {
						log.Printf("GetRoomMessages: E2EE decryption failed for direct message %s: %v", msg.ID, decryptionErr)
						// Keep original content if decryption fails (might be plaintext)
						decryptedContent = *msg.Content
					} else {
						log.Printf("GetRoomMessages: Successfully decrypted direct message %s", msg.ID)
					}
				} else {
					decryptedContent = *msg.Content
				}
			} else {
				// Group message decryption
				decryptedContent, decryptionErr = getE2EEService().DecryptGroupMessage(roomID, *msg.Content)
				if decryptionErr != nil {
					log.Printf("GetRoomMessages: E2EE decryption failed for group message %s: %v", msg.ID, decryptionErr)
					// Keep original content if decryption fails (might be plaintext)
					decryptedContent = *msg.Content
				} else {
					log.Printf("GetRoomMessages: Successfully decrypted group message %s", msg.ID)
				}
			}

			// Update content with decrypted version
			msgWithAuthor["content"] = decryptedContent
		}

		// Add author details if available
		if msg.AuthorID != nil && *msg.AuthorID != "" {
			if authorData, exists := authors[*msg.AuthorID]; exists {
				msgWithAuthor["author"] = authorData
			}
		}

		// Process attachments - they should already contain full data from database
		if len(msg.Attachments) > 0 {
			log.Printf("GetRoomMessages: Found %d attachments for message %s", len(msg.Attachments), msg.ID)
			processedAttachments := make([]interface{}, 0, len(msg.Attachments))

			for _, attachment := range msg.Attachments {
				if attachment != nil {
					// Attachments should already contain full data from database
					processedAttachments = append(processedAttachments, *attachment)
					log.Printf("GetRoomMessages: Attachment data: %+v", *attachment)
				}
			}

			msgWithAuthor["attachments"] = processedAttachments
			log.Printf("GetRoomMessages: Successfully processed %d attachments for message %s", len(processedAttachments), msg.ID)
		} else {
			// Ensure attachments field is an empty array instead of null
			msgWithAuthor["attachments"] = []interface{}{}
		}

		messagesWithAuthors[i] = msgWithAuthor
	}

	log.Printf("GetRoomMessages: Completed successfully, returning %d messages with author details", len(messagesWithAuthors))
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"messages": messagesWithAuthors,
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

	// Validate that either content or attachments are provided
	if (body.Content == "" || strings.TrimSpace(body.Content) == "") && len(body.Attachments) == 0 {
		log.Printf("CreateMessage: No content or attachments provided")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Either content or attachments must be provided",
		})
	}
	// Check if user has access to the room and permission to send messages
	log.Printf("CreateMessage: Checking room access and send message permission for user %s", user.ID)
	hasAccess, err := checkRoomAccess(roomID, user.ID, utils.SEND_MESSAGES)
	if err != nil {
		log.Printf("CreateMessage: Failed to check room access: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to check room access",
		})
	}

	log.Printf("CreateMessage: User %s has access to room %s: %v", user.ID, roomID, hasAccess)
	if !hasAccess {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have permission to send messages in this room",
		})
	}
	// Get room details for E2EE check
	var room types.Room
	roomQuery := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQuery.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("CreateMessage: Failed to fetch room: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch room details",
		})
	}

	if err := roomQuery.Get(&room); err != nil {
		roomQuery.Release()
		log.Printf("CreateMessage: Failed to get room details: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to get room details",
		})
	}
	roomQuery.Release()

	// Create message
	messageID := helpers.GenerateMessageID().String()
	createdAt := time.Now()

	log.Printf("CreateMessage: Generated messageID=%s", messageID)

	// Parse mentions from content
	mentionResult := utils.ParseMentions(body.Content)
	log.Printf("CreateMessage: Parsed mentions - Users: %v, Roles: %v, Rooms: %v, Everyone: %v",
		mentionResult.UserMentions, mentionResult.RoleMentions, mentionResult.RoomMentions, mentionResult.MentionEveryone)

	// Get user's permissions to validate @everyone mention
	userPermissions, err := getUserPermissionsForRoom(roomID, user.ID)
	if err != nil {
		log.Printf("CreateMessage: Failed to get user permissions: %v", err)
		userPermissions = []string{} // Default to no permissions
	}

	// Validate @everyone mention
	canMentionEveryone := utils.ValidateEveryoneMention(userPermissions)
	if mentionResult.MentionEveryone && !canMentionEveryone {
		log.Printf("CreateMessage: User %s does not have permission to use @everyone", user.ID)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have permission to mention @everyone",
		})
	}

	// Handle E2EE encryption for PM and GROUP_PM rooms
	var finalContent string
	var isEncrypted bool
	if room.Type == types.RoomTypePM || room.Type == types.RoomTypeGroupPM {
		log.Printf("CreateMessage: Attempting E2EE encryption for room type %d", room.Type)

		if room.Type == types.RoomTypePM {
			// Direct message encryption - get recipient ID from room recipients
			var roomRecipients []models.RoomRecipientByUser
			if err := models.RoomRecipientByUserTable.SelectBuilder().
				Columns("user_id", "room_id").
				Where(qb.Eq("room_id")).
				Query(*database.Session).
				BindMap(qb.M{
					"room_id": roomID,
				}).SelectRelease(&roomRecipients); err != nil {
				log.Printf("CreateMessage: Failed to get room recipients: %v", err)
				finalContent = body.Content
				isEncrypted = false
			} else {
				recipientID := ""
				for _, recipient := range roomRecipients {
					if recipient.UserId != user.ID {
						recipientID = recipient.UserId
						break
					}
				}

				if recipientID != "" {
					encryptedContent, err := getE2EEService().EncryptMessage(user.ID, recipientID, body.Content)
					if err != nil {
						log.Printf("CreateMessage: E2EE encryption failed, sending as plaintext: %v", err)
						finalContent = body.Content
						isEncrypted = false
					} else {
						finalContent = encryptedContent
						isEncrypted = true
						log.Printf("CreateMessage: Successfully encrypted direct message")
					}
				} else {
					finalContent = body.Content
					isEncrypted = false
				}
			}
		} else {
			// Group message encryption
			encryptedContent, err := getE2EEService().EncryptGroupMessage(roomID, user.ID, body.Content)
			if err != nil {
				log.Printf("CreateMessage: Group E2EE encryption failed, sending as plaintext: %v", err)
				finalContent = body.Content
				isEncrypted = false
			} else {
				finalContent = encryptedContent
				isEncrypted = true
				log.Printf("CreateMessage: Successfully encrypted group message")
			}
		}
	} else {
		// No encryption for other room types
		finalContent = body.Content
		isEncrypted = false
	}

	message := models.Message{
		ID:              messageID,
		Content:         &finalContent,
		Nonce:           &body.Nonce,
		AuthorID:        &user.ID,
		RoomID:          roomID,
		CreatedAt:       createdAt,
		MentionRoles:    &mentionResult.RoleMentions,
		MentionRooms:    &mentionResult.RoomMentions,
		Mentions:        &mentionResult.UserMentions,
		MentionEveryone: mentionResult.MentionEveryone && canMentionEveryone,
		// UpdatedAt: createdAt,
	}

	// Add E2EE flag if encrypted
	if isEncrypted {
		log.Printf("CreateMessage: Message encrypted with E2EE")
	}

	// Add unread entry for all recipients except the sender and those who are online
	if err := AddUnreadMessage(roomID, messageID, user.ID); err != nil {
		log.Printf("CreateMessage: Failed to add unread entries: %v", err)
	}

	// Handle message references (replies) - limit to 5 max
	if len(body.MessageReferences) > 0 {
		log.Printf("CreateMessage: Processing message references (max 5)")
		// Truncate to max 5 references if needed
		if len(body.MessageReferences) > 5 {
			body.MessageReferences = body.MessageReferences[:5]
		}
		message.MessageReferences = &body.MessageReferences
		log.Printf("CreateMessage: Added %d message references", len(body.MessageReferences))
	}

	// Handle attachments
	if len(body.Attachments) > 0 {
		log.Printf("CreateMessage: Processing %d attachments", len(body.Attachments))
		attachments := make([]*map[string]interface{}, 0, len(body.Attachments))

		for _, attachmentID := range body.Attachments {
			log.Printf("CreateMessage: Processing attachment ID: %s", attachmentID)
			// Fetch attachment metadata from Nebula
			attachmentData, err := fetchAttachmentMetadata(attachmentID)
			if err != nil {
				log.Printf("CreateMessage: Failed to fetch attachment metadata for ID %s: %v", attachmentID, err)
				continue // Skip invalid attachments
			}

			log.Printf("CreateMessage: Fetched attachment data: %+v", attachmentData)
			attachments = append(attachments, &attachmentData)
		}

		if len(attachments) > 0 {
			message.Attachments = attachments
			log.Printf("CreateMessage: Final attachments to store: %+v", attachments)
		}
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

func DeleteMessage(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("roomID")
	messageID := c.Params("messageID")

	log.Printf("DeleteMessage: Started for roomID=%s, messageID=%s, userID=%s", roomID, messageID, user.ID)

	if roomID == "" || messageID == "" {
		log.Printf("DeleteMessage: Missing required parameters")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID and Message ID are required",
		})
	}

	// Check if user has access to the room
	log.Printf("DeleteMessage: Checking room access for user %s", user.ID)
	hasAccess, err := checkRoomAccess(roomID, user.ID, utils.READ_MESSAGE_HISTORY)
	if err != nil {
		log.Printf("DeleteMessage: Failed to check room access: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to check room access",
		})
	}

	log.Printf("DeleteMessage: User %s has access to room %s: %v", user.ID, roomID, hasAccess)
	if !hasAccess {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You don't have access to this room",
		})
	}

	// Get the message to check ownership
	var message models.Message
	if err := models.MessageTable.SelectBuilder().
		Columns("id", "author_id", "room_id").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{"id": messageID}).
		GetRelease(&message); err != nil {
		log.Printf("DeleteMessage: Failed to fetch message: %v", err)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "Message not found",
		})
	}

	// Get the room to check type and creator
	var room types.Room
	roomQ := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQ.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("DeleteMessage: Failed to fetch room: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch room details",
		})
	}

	if err := roomQ.Get(&room); err != nil {
		roomQ.Release()
		log.Printf("DeleteMessage: Failed to get room details: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to get room details",
		})
	}
	roomQ.Release()

	// Check if user has permission to delete the message
	isAuthor := message.AuthorID != nil && *message.AuthorID == user.ID
	isGroupCreator := room.Type == types.RoomTypeGroupPM && room.Creator != nil && *room.Creator == user.ID

	// In normal PMs (type 0), users can only delete their own messages
	// In group PMs (type 1), both the message author and group creator can delete messages
	if room.Type == types.RoomTypePM {
		if !isAuthor {
			log.Printf("DeleteMessage: User %s is not authorized to delete message %s in PM", user.ID, messageID)
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"message": "In direct messages, you can only delete your own messages.",
			})
		}
	} else if !isAuthor && !isGroupCreator {
		log.Printf("DeleteMessage: User %s is not authorized to delete message %s", user.ID, messageID)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You can only delete your own messages or any message in a group PM you created.",
		})
	}

	// Delete message from both tables concurrently
	var wg sync.WaitGroup
	errChan := make(chan error, 2)

	// Delete from messages table
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("DeleteMessage: Deleting from messages table for messageID=%s", messageID)

		// First, get the full message to retrieve the created_at field
		var fullMessage models.Message
		getMessageQuery := models.MessageTable.SelectBuilder().
			Where(qb.Eq("id")).
			Query(*database.Session).
			BindMap(qb.M{"id": messageID})

		if err := getMessageQuery.GetRelease(&fullMessage); err != nil {
			log.Printf("DeleteMessage: Error retrieving full message for deletion: %v", err)
			errChan <- err
			return
		}

		// Now delete with all required fields
		if err := models.MessageTable.DeleteBuilder().
			Where(qb.Eq("id")).
			Query(*database.Session).
			BindMap(qb.M{
				"id":         messageID,
				"created_at": fullMessage.CreatedAt,
			}).
			ExecRelease(); err != nil {
			log.Printf("DeleteMessage: Error deleting from messages table: %v", err)
			errChan <- err
			return
		}
		log.Printf("DeleteMessage: Successfully deleted from messages table")
		errChan <- nil
	}()

	// Delete from messages_by_room table
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("DeleteMessage: Deleting from messages_by_room table for roomID=%s, messageID=%s", roomID, messageID)

		// First, get the message to retrieve the created_at field
		var messageByRoom models.MessagesByRoom
		getQuery := models.MessagesByRoomTable.SelectBuilder().
			Where(qb.Eq("room_id"), qb.Eq("id")).
			Query(*database.Session).
			BindMap(qb.M{
				"room_id": roomID,
				"id":      messageID,
			})

		if err := getQuery.GetRelease(&messageByRoom); err != nil {
			log.Printf("DeleteMessage: Error retrieving message for deletion: %v", err)
			errChan <- err
			return
		}

		// Now delete with all required fields
		if err := models.MessagesByRoomTable.DeleteBuilder().
			Where(qb.Eq("room_id"), qb.Eq("id")).
			Query(*database.Session).
			BindMap(qb.M{
				"room_id":    roomID,
				"id":         messageID,
				"created_at": messageByRoom.CreatedAt,
			}).
			ExecRelease(); err != nil {
			log.Printf("DeleteMessage: Error deleting from messages_by_room table: %v", err)
			errChan <- err
			return
		}
		log.Printf("DeleteMessage: Successfully deleted from messages_by_room table")
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
			log.Printf("Error in message deletion: %v", err)
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to delete message",
		})
	}

	// Publish message deletion event to Redis for Stargate
	event := map[string]interface{}{
		"type": "MESSAGE_DELETE",
		"data": map[string]interface{}{
			"id":      messageID,
			"room_id": roomID,
		},
	}

	log.Printf("DeleteMessage: Preparing to publish message deletion event to Redis")
	eventJson, err := json.Marshal(event)
	if err != nil {
		log.Printf("DeleteMessage: Error marshaling message deletion event: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to marshal message deletion event",
		})
	}

	log.Printf("DeleteMessage: Publishing message deletion event to Redis ROOM_EVENTS channel")
	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventJson)).Err(); err != nil {
		log.Printf("DeleteMessage: Error publishing message deletion event: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish message deletion event",
		})
	}

	log.Printf("DeleteMessage: Successfully completed for messageID=%s", messageID)
	return c.Status(fiber.StatusNoContent).JSON(fiber.Map{
		"message": "Message deleted successfully",
	})
}
