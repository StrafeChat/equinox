package events

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/repository"
)

// SpaceEvent represents a space-related event
type SpaceEvent struct {
	Type      string                 `json:"type"`
	SpaceID   string                 `json:"space_id"`
	IconURL   string                 `json:"icon_url"`
	BannerURL string                 `json:"banner_url"`
	FileID    string                 `json:"file_id"`
	AuthToken string                 `json:"auth_token"`
	CreatedAt int64                  `json:"created_at"`
	Data      map[string]interface{} `json:"data"`
}

// StartSpaceEventListener starts listening for space events from Redis
func StartSpaceEventListener() {
	log.Printf("Starting Redis space event listener")

	// Subscribe to SPACE_EVENTS channel
	pubsub := database.Rdb.Subscribe("SPACE_EVENTS")
	defer pubsub.Close()

	ch := pubsub.Channel()
	log.Printf("Successfully subscribed to SPACE_EVENTS channel")

	for msg := range ch {
		log.Printf("[StartSpaceEventListener] Received space event: %s", msg.Payload)

		payload := []byte(strings.TrimSpace(msg.Payload))

		if len(payload) == 0 {
			log.Printf("[StartSpaceEventListener] Received empty payload in space event")
			continue
		}

		var event SpaceEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			log.Printf("[StartSpaceEventListener] Failed to unmarshal space event: %v", err)
			continue
		}

		log.Printf("[StartSpaceEventListener] Processing space event type: %s", event.Type)

		// Handle different event types
		switch event.Type {
		case "SPACE_ICON_UPDATE":
			go handleSpaceIconUpdate(event)
		case "SPACE_BANNER_UPDATE":
			go handleSpaceBannerUpdate(event)
		default:
			log.Printf("[StartSpaceEventListener] Unknown space event type: %s", event.Type)
		}
	}
}

// handleSpaceIconUpdate handles space icon update events
func handleSpaceIconUpdate(event SpaceEvent) {
	log.Printf("[handleSpaceIconUpdate] Starting to handle space icon update: SpaceID=%s, IconURL=%s", event.SpaceID, event.IconURL)

	// Validate the auth token
	if event.AuthToken == "" {
		log.Printf("Space icon update event missing auth token")
		return
	}

	userID, err := validateSessionToken(event.AuthToken)
	if err != nil {
		log.Printf("[handleSpaceIconUpdate] Invalid session token: %v", err)
		return
	}

	log.Printf("[handleSpaceIconUpdate] Validated user: %s", userID)

	// Convert space ID to int64
	spaceID, err := strconv.ParseInt(event.SpaceID, 10, 64)
	if err != nil {
		log.Printf("[handleSpaceIconUpdate] Invalid space ID: %v", err)
		return
	}

	// Check if user is the space owner
	spaceRepo := repository.NewSpaceRepository(database.Session)
	space, err := spaceRepo.GetSpace(spaceID)
	if err != nil {
		log.Printf("[handleSpaceIconUpdate] Failed to get space: %v", err)
		return
	}

	if space.OwnerID != userID {
		log.Printf("[handleSpaceIconUpdate] User %s is not the owner of space %d", userID, spaceID)
		return
	}

	// Update space icon in database
	updates := map[string]interface{}{
		"icon":       event.IconURL,
		"updated_at": time.Now(),
	}

	if err := spaceRepo.UpdateSpace(spaceID, updates); err != nil {
		log.Printf("[handleSpaceIconUpdate] Failed to update space icon: %v", err)
		return
	}

	log.Printf("[handleSpaceIconUpdate] Successfully updated space %d icon to %s", spaceID, event.IconURL)

	// Publish space update event for real-time updates to clients
	spaceUpdateEvent := SpaceUpdateEvent{
		Type:      "SPACE_UPDATED",
		SpaceID:   spaceID,
		Updates:   updates,
		UpdatedBy: userID,
		Timestamp: time.Now().Unix(),
	}

	if err := PublishSpaceUpdate(spaceUpdateEvent); err != nil {
		log.Printf("[handleSpaceIconUpdate] Failed to publish space update event: %v", err)
		// Don't fail the operation if event publishing fails
	}

	log.Printf("[handleSpaceIconUpdate] Space icon update completed successfully")
}

// handleSpaceBannerUpdate handles space banner update events
func handleSpaceBannerUpdate(event SpaceEvent) {
	log.Printf("[handleSpaceBannerUpdate] Starting to handle space banner update: SpaceID=%s, BannerURL=%s", event.SpaceID, event.BannerURL)

	// Validate the auth token
	if event.AuthToken == "" {
		log.Printf("Space banner update event missing auth token")
		return
	}

	userID, err := validateSessionToken(event.AuthToken)
	if err != nil {
		log.Printf("[handleSpaceBannerUpdate] Invalid session token: %v", err)
		return
	}

	log.Printf("[handleSpaceBannerUpdate] Validated user: %s", userID)

	// Convert space ID to int64
	spaceID, err := strconv.ParseInt(event.SpaceID, 10, 64)
	if err != nil {
		log.Printf("[handleSpaceBannerUpdate] Invalid space ID: %v", err)
		return
	}

	// Check if user is the space owner
	spaceRepo := repository.NewSpaceRepository(database.Session)
	space, err := spaceRepo.GetSpace(spaceID)
	if err != nil {
		log.Printf("[handleSpaceBannerUpdate] Failed to get space: %v", err)
		return
	}

	if space.OwnerID != userID {
		log.Printf("[handleSpaceBannerUpdate] User %s is not the owner of space %d", userID, spaceID)
		return
	}

	// Update space banner in database
	updates := map[string]interface{}{
		"banner":     event.BannerURL,
		"updated_at": time.Now(),
	}

	if err := spaceRepo.UpdateSpace(spaceID, updates); err != nil {
		log.Printf("[handleSpaceBannerUpdate] Failed to update space banner: %v", err)
		return
	}

	log.Printf("[handleSpaceBannerUpdate] Successfully updated space %d banner to %s", spaceID, event.BannerURL)

	// Publish space update event for real-time updates to clients
	spaceUpdateEvent := SpaceUpdateEvent{
		Type:      "SPACE_UPDATED",
		SpaceID:   spaceID,
		Updates:   updates,
		UpdatedBy: userID,
		Timestamp: time.Now().Unix(),
	}

	if err := PublishSpaceUpdate(spaceUpdateEvent); err != nil {
		log.Printf("[handleSpaceBannerUpdate] Failed to publish space update event: %v", err)
		// Don't fail the operation if event publishing fails
	}

	log.Printf("[handleSpaceBannerUpdate] Space banner update completed successfully")
}