package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3/qb"
)

// RoomEvent represents a room-related event from Redis
type RoomEvent struct {
	Type      string                 `json:"type"`
	RoomID    string                 `json:"room_id"`
	IconURL   string                 `json:"icon_url"`
	AuthToken string                 `json:"auth_token"`
	CreatedAt int64                  `json:"created_at"`
	Data      map[string]interface{} `json:"data"`
}

// StartRoomEventListener starts listening for room events from Redis
func StartRoomEventListener() {
	log.Printf("Starting Redis room event listener")

	// Subscribe to ROOM_EVENTS channel
	pubsub := database.Rdb.Subscribe("ROOM_EVENTS")
	defer pubsub.Close()

	ch := pubsub.Channel()
	log.Printf("Successfully subscribed to ROOM_EVENTS channel")

	for msg := range ch {
		log.Printf("[StartRoomEventListener] Received room event: %s", msg.Payload)

		payload := []byte(strings.TrimSpace(msg.Payload))

		if len(payload) == 0 {
			log.Printf("[StartRoomEventListener] Received empty payload in room event")
			continue
		}

		var event RoomEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			log.Printf("[StartRoomEventListener] Error unmarshaling room event (payload: %s): %v", string(payload), err)
			continue
		}

		if event.Type == "" {
			log.Printf("[StartRoomEventListener] Received room event with empty type: %+v", event)
			continue
		}

		log.Printf("[StartRoomEventListener] Processing room event: Type=%s, RoomID=%s", event.Type, event.RoomID)

		switch event.Type {
		case "ROOM_ICON_UPDATE":
			log.Printf("[StartRoomEventListener] Handling ROOM_ICON_UPDATE event")
			go handleRoomIconUpdate(event)
		default:
			log.Printf("[StartRoomEventListener] Unknown room event type: %s", event.Type)
		}
	}
}

// handleRoomIconUpdate handles room icon update events
func handleRoomIconUpdate(event RoomEvent) {
	log.Printf("[handleRoomIconUpdate] Starting to handle room icon update: RoomID=%s, IconURL=%s", event.RoomID, event.IconURL)

	// Validate the auth token
	if event.AuthToken == "" {
		log.Printf("Room icon update event missing auth token")
		return
	}

	// Verify the session token
	userID, err := validateSessionToken(event.AuthToken)
	if err != nil {
		log.Printf("Invalid session token in room icon update: %v", err)
		return
	}

	// Check if user has permission to update the room
	var room types.Room
	roomQ := models.RoomTable.SelectQuery(*database.Session)
	if roomErr := roomQ.BindMap(map[string]interface{}{
		"id": event.RoomID,
	}).GetRelease(&room); roomErr != nil {
		log.Printf("Failed to get room %s: %v", event.RoomID, err)
		return
	}

	// Check if user is the room creator or has permission
	if room.Creator == nil || *room.Creator != userID {
		log.Printf("User %s does not have permission to update room %s icon", userID, event.RoomID)
		return
	}

	// Update the room icon in the database
	updateStmt := models.RoomTable.UpdateBuilder().
		Set("icon", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"id":         event.RoomID,
			"icon":       event.IconURL,
			"updated_at": time.Now(),
		})

	if execErr := updateStmt.ExecRelease(); execErr != nil {
		log.Printf("Failed to update room icon in database: %v", err)
		return
	}

	log.Printf("Successfully updated room %s icon to %s", event.RoomID, event.IconURL)

	// Create system message for icon change
	systemData := helpers.SystemMessageData{
		Type:     helpers.RoomIconChanged,
		ActorID:  &userID,
		OldValue: room.Icon, // Previous icon URL (can be nil)
		NewValue: &event.IconURL,
	}

	if createErr := helpers.CreateSystemMessage(event.RoomID, helpers.RoomIconChanged, systemData); createErr != nil {
		log.Printf("Failed to create system message for room icon change: %v", err)
		// Don't return here - continue with room update event
	} else {
		log.Printf("Created system message for room %s icon change", event.RoomID)
	}

	// Publish room update event for Stargate to broadcast to clients
	roomUpdateEvent := map[string]interface{}{
		"type":    "ROOM_UPDATE",
		"room_id": event.RoomID,
		"data": map[string]interface{}{
			"id":   event.RoomID,
			"icon": event.IconURL,
		},
		"created_at": event.CreatedAt,
	}

	eventBytes, err := json.Marshal(roomUpdateEvent)
	if err != nil {
		log.Printf("Failed to marshal room update event: %v", err)
		return
	}

	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("Failed to publish room update event: %v", err)
		return
	}

	log.Printf("Published room update event for room %s", event.RoomID)
}

// validateSessionToken validates a session token and returns the user ID
func validateSessionToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("empty token")
	}

	sessionByToken := models.SessionTable.SelectBuilder().
		Columns("user_id", "expires_at").
		Where(qb.Eq("session_token")).
		Limit(1)

	sessionByTokenQuery := sessionByToken.Query(*database.Session).
		BindStruct(models.Session{
			Token: token,
		})

	var session models.Session
	if err := sessionByTokenQuery.GetRelease(&session); err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return "", errors.New("session not found")
		}
		return "", fmt.Errorf("error getting session: %v", err)
	}

	if session.ExpiresAt.Before(time.Now()) {
		return "", errors.New("session expired")
	}

	return fmt.Sprint(session.UserId), nil
}
