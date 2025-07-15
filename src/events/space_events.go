package events

import (
	"encoding/json"
	"log"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
)

// PublishSpaceCreateEvent publishes a space creation event to Redis
func PublishSpaceCreateEvent(spaceData map[string]interface{}) error {
	log.Printf("[PublishSpaceCreateEvent] Publishing space create event: SpaceID=%s", spaceData["id"])

	// Convert created_at to Unix timestamp if it's a time.Time
	var createdAtUnix int64
	if createdAt, ok := spaceData["created_at"].(time.Time); ok {
		createdAtUnix = createdAt.Unix()
	} else {
		// Fallback to current time if conversion fails
		createdAtUnix = time.Now().Unix()
	}

	// Create space create event
	spaceCreateEvent := map[string]interface{}{
		"type":       "SPACE_CREATE",
		"sender_id":  spaceData["owner_id"],
		"data":       spaceData,
		"created_at": createdAtUnix,
	}

	eventBytes, err := json.Marshal(spaceCreateEvent)
	if err != nil {
		log.Printf("[PublishSpaceCreateEvent] Failed to marshal space create event: %v", err)
		return err
	}

	if err := database.Rdb.Publish("SPACE_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("[PublishSpaceCreateEvent] Failed to publish space create event: %v", err)
		return err
	}

	log.Printf("[PublishSpaceCreateEvent] Successfully published space create event for space %s", spaceData["id"])
	return nil
}

// PublishSpaceRoleUpdateEvent publishes a space role update event to Redis
func PublishSpaceRoleUpdateEvent(spaceID int64, roleID string, updatedBy string, roleData map[string]interface{}) error {
	log.Printf("[PublishSpaceRoleUpdateEvent] Publishing role update event: SpaceID=%d, RoleID=%s", spaceID, roleID)

	// Create role update event
	roleUpdateEvent := map[string]interface{}{
		"type":       "SPACE_ROLE_UPDATE",
		"space_id":   strconv.FormatInt(spaceID, 10),
		"role_id":    roleID,
		"sender_id":  updatedBy,
		"data":       roleData,
		"created_at": time.Now().Unix(),
	}

	eventBytes, err := json.Marshal(roleUpdateEvent)
	if err != nil {
		log.Printf("[PublishSpaceRoleUpdateEvent] Failed to marshal role update event: %v", err)
		return err
	}

	if err := database.Rdb.Publish("SPACE_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("[PublishSpaceRoleUpdateEvent] Failed to publish role update event: %v", err)
		return err
	}

	log.Printf("[PublishSpaceRoleUpdateEvent] Successfully published role update event for space %d, role %s", spaceID, roleID)
	return nil
}

// PublishSpaceMemberRoleUpdateEvent publishes a space member role update event to Redis
func PublishSpaceMemberRoleUpdateEvent(spaceID int64, userID string, updatedBy string, roles []string) error {
	log.Printf("[PublishSpaceMemberRoleUpdateEvent] Publishing member role update event: SpaceID=%d, UserID=%s", spaceID, userID)

	// Create member role update event
	memberRoleUpdateEvent := map[string]interface{}{
		"type":       "SPACE_MEMBER_ROLE_UPDATE",
		"space_id":   strconv.FormatInt(spaceID, 10),
		"user_id":    userID,
		"sender_id":  updatedBy,
		"data": map[string]interface{}{
			"roles": roles,
		},
		"created_at": time.Now().Unix(),
	}

	eventBytes, err := json.Marshal(memberRoleUpdateEvent)
	if err != nil {
		log.Printf("[PublishSpaceMemberRoleUpdateEvent] Failed to marshal member role update event: %v", err)
		return err
	}

	if err := database.Rdb.Publish("SPACE_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("[PublishSpaceMemberRoleUpdateEvent] Failed to publish member role update event: %v", err)
		return err
	}

	log.Printf("[PublishSpaceMemberRoleUpdateEvent] Successfully published member role update event for space %d, user %s", spaceID, userID)
	return nil
}

// PublishSpaceRoleCreateEvent publishes a space role creation event to Redis
func PublishSpaceRoleCreateEvent(spaceID int64, roleData map[string]interface{}, createdBy string) error {
	log.Printf("[PublishSpaceRoleCreateEvent] Publishing role create event: SpaceID=%d", spaceID)

	// Create role create event
	roleCreateEvent := map[string]interface{}{
		"type":       "SPACE_ROLE_CREATE",
		"space_id":   strconv.FormatInt(spaceID, 10),
		"sender_id":  createdBy,
		"data":       roleData,
		"created_at": time.Now().Unix(),
	}

	eventBytes, err := json.Marshal(roleCreateEvent)
	if err != nil {
		log.Printf("[PublishSpaceRoleCreateEvent] Failed to marshal role create event: %v", err)
		return err
	}

	if err := database.Rdb.Publish("SPACE_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("[PublishSpaceRoleCreateEvent] Failed to publish role create event: %v", err)
		return err
	}

	log.Printf("[PublishSpaceRoleCreateEvent] Successfully published role create event for space %d", spaceID)
	return nil
}

// PublishSpaceRoleDeleteEvent publishes a space role deletion event to Redis
func PublishSpaceRoleDeleteEvent(spaceID int64, roleID string, deletedBy string) error {
	log.Printf("[PublishSpaceRoleDeleteEvent] Publishing role delete event: SpaceID=%d, RoleID=%s", spaceID, roleID)

	// Create role delete event
	roleDeleteEvent := map[string]interface{}{
		"type":       "SPACE_ROLE_DELETE",
		"space_id":   strconv.FormatInt(spaceID, 10),
		"role_id":    roleID,
		"sender_id":  deletedBy,
		"created_at": time.Now().Unix(),
	}

	eventBytes, err := json.Marshal(roleDeleteEvent)
	if err != nil {
		log.Printf("[PublishSpaceRoleDeleteEvent] Failed to marshal role delete event: %v", err)
		return err
	}

	if err := database.Rdb.Publish("SPACE_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("[PublishSpaceRoleDeleteEvent] Failed to publish role delete event: %v", err)
		return err
	}

	log.Printf("[PublishSpaceRoleDeleteEvent] Successfully published role delete event for space %d, role %s", spaceID, roleID)
	return nil
}
