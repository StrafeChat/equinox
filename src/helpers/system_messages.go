package helpers

import (
	"encoding/json"
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/scylladb/gocqlx/v2/qb"
)

// SystemMessageType represents different types of system messages
type SystemMessageType string

const (
	MemberAdded          SystemMessageType = "MEMBER_ADDED"
	MemberRemoved        SystemMessageType = "MEMBER_REMOVED"
	RoomNameChanged      SystemMessageType = "ROOM_NAME_CHANGED"
	RoomTopicChanged     SystemMessageType = "ROOM_TOPIC_CHANGED"
	RoomIconChanged      SystemMessageType = "ROOM_ICON_CHANGED"
	OwnershipTransferred SystemMessageType = "OWNERSHIP_TRANSFERRED"
)

// SystemMessageData contains the data for system messages
type SystemMessageData struct {
	Type      SystemMessageType      `json:"type"`
	UserID    *string                `json:"user_id,omitempty"`    // For member events
	ActorID   *string                `json:"actor_id,omitempty"`   // Who performed the action
	OldValue  *string                `json:"old_value,omitempty"`  // For property changes
	NewValue  *string                `json:"new_value,omitempty"`  // For property changes
	ExtraData map[string]interface{} `json:"extra_data,omitempty"` // Additional data
}

// CreateSystemMessage creates a system message in the database and publishes it
func CreateSystemMessage(roomID string, messageType SystemMessageType, data SystemMessageData) error {
	log.Printf("[CreateSystemMessage] Creating system message: roomID=%s, type=%s", roomID, messageType)

	// Generate message ID and timestamp
	messageID := GenerateMessageID().String()
	createdAt := time.Now()

	// Create system message content based on type
	content := generateSystemMessageContent(messageType, data)

	// Create the message with system author (empty author_id indicates system message)
	msgType := 1 // System message type
	systemTypeStr := string(messageType)

	// Convert SystemMessageData to proper struct for database storage
	extraDataJson := ""
	if data.ExtraData != nil {
		if jsonBytes, err := json.Marshal(data.ExtraData); err == nil {
			extraDataJson = string(jsonBytes)
		}
	}

	systemDataStruct := models.MessageSystemData{
		Type:      string(data.Type),
		UserID:    data.UserID,
		ActorID:   data.ActorID,
		OldValue:  data.OldValue,
		NewValue:  data.NewValue,
		ExtraData: extraDataJson,
	}

	systemDataMap := systemDataStruct.ToMap()

	message := models.Message{
		ID:         messageID,
		Content:    &content,
		AuthorID:   nil, // nil author_id indicates system message
		RoomID:     roomID,
		Type:       &msgType,
		SystemType: &systemTypeStr,
		SystemData: &systemDataMap,
		System:     true,
		CreatedAt:  createdAt,
	}

	// Insert into messages table
	log.Printf("[CreateSystemMessage] Inserting system message into database: messageID=%s", messageID)
	if err := models.MessageTable.InsertQuery(*database.Session).BindStruct(&message).ExecRelease(); err != nil {
		log.Printf("[CreateSystemMessage] Failed to insert system message: %v", err)
		return err
	}

	// Insert into messages_by_room table
	messageByRoom := models.MessagesByRoom{
		RoomID:    roomID,
		ID:        messageID,
		CreatedAt: createdAt,
	}

	if err := models.MessagesByRoomTable.InsertQuery(*database.Session).BindStruct(&messageByRoom).ExecRelease(); err != nil {
		log.Printf("[CreateSystemMessage] Failed to insert into messages_by_room: %v", err)
		return err
	}

	// Update room's last_message_id
	if err := models.RoomTable.UpdateBuilder().
		Set("last_message_id", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"last_message_id": messageID,
			"updated_at":      createdAt,
			"id":              roomID,
		}).ExecRelease(); err != nil {
		log.Printf("[CreateSystemMessage] Failed to update room last_message_id: %v", err)
		return err
	}

	// Publish message to Redis for Stargate
	event := map[string]interface{}{
		"type": "MESSAGE_CREATE",
		"data": map[string]interface{}{
			"id":          messageID,
			"content":     content,
			"author_id":   "", // System message
			"room_id":     roomID,
			"created_at":  createdAt.Format(time.RFC3339),
			"type":        msgType,
			"system":      true,
			"system_type": systemTypeStr,
			"system_data": data,
		},
	}

	eventJson, err := json.Marshal(event)
	if err != nil {
		log.Printf("[CreateSystemMessage] Failed to marshal event: %v", err)
		return err
	}

	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventJson)).Err(); err != nil {
		log.Printf("[CreateSystemMessage] Failed to publish event: %v", err)
		return err
	}

	log.Printf("[CreateSystemMessage] Successfully created and published system message: messageID=%s", messageID)
	return nil
}

// generateSystemMessageContent creates human-readable content for system messages
func generateSystemMessageContent(messageType SystemMessageType, data SystemMessageData) string {
	switch messageType {
	case MemberAdded:
		if data.ActorID != nil && data.UserID != nil {
			if *data.ActorID == *data.UserID {
				return "joined the group"
			} else {
				return "was added to the group"
			}
		}
		return "was added to the group"

	case MemberRemoved:
		if data.ActorID != nil && data.UserID != nil {
			if *data.ActorID == *data.UserID {
				return "left the group"
			} else {
				return "was removed from the group"
			}
		}
		return "was removed from the group"

	case RoomNameChanged:
		if data.NewValue != nil {
			if data.OldValue != nil && *data.OldValue != "" {
				return "changed the group name"
			} else {
				return "set the group name"
			}
		}
		return "changed the group name"

	case RoomTopicChanged:
		if data.NewValue != nil {
			if data.OldValue != nil && *data.OldValue != "" {
				return "changed the group topic"
			} else {
				return "set the group topic"
			}
		}
		return "removed the group topic"

	case RoomIconChanged:
		if data.NewValue != nil && *data.NewValue != "" {
			return "changed the group icon"
		}
		return "removed the group icon"

	case OwnershipTransferred:
		return "transferred group ownership"

	default:
		return "performed an action"
	}
}
