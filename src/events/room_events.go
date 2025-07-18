package events

import (
	"fmt"
	"log"
	"strings"

	"github.com/StrafeChat/equinox/src/database"
)

// PublishEvent publishes an event to the specified Redis channel
func PublishEvent(eventType string, eventData string) error {
	var channel string
	switch {
	case strings.Contains(eventType, "ROOM"):
		channel = "ROOM_EVENTS"
	case strings.Contains(eventType, "SPACE"):
		channel = "SPACE_EVENTS"
	case strings.Contains(eventType, "USER"):
		channel = "USER_EVENTS"
	case strings.Contains(eventType, "RELATIONSHIP"):
		channel = "RELATIONSHIP_EVENTS"
	case strings.Contains(eventType, "VOICE"):
		channel = "VOICE_EVENTS"
	case strings.Contains(eventType, "FILE"):
		channel = "FILE_EVENTS"
	default:
		channel = "ROOM_EVENTS" // Default to ROOM_EVENTS for backward compatibility
	}

	if err := database.Rdb.Publish(channel, eventData).Err(); err != nil {
		return fmt.Errorf("failed to publish %s event to %s: %v", eventType, channel, err)
	}

	log.Printf("Published %s event to %s channel", eventType, channel)
	return nil
}
