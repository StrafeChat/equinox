package spaces

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/events"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
)

type CreateSpaceRoomInput struct {
	Name         string   `json:"name" validate:"required,min=1,max=100"`
	Type         int      `json:"type" validate:"required,oneof=2 3 4"` // 2 = Text Room, 3 = Voice Room, 4 = Space Section
	ParentID     *string  `json:"parent_id,omitempty"`                  // Section ID if room belongs to a section
	Topic        *string  `json:"topic,omitempty" validate:"omitempty,max=1024"`
	IsPrivate    bool     `json:"is_private,omitempty"`
	AllowedRoles []string `json:"allowed_roles,omitempty"` // Role IDs that can access this private room
}

func CreateSpaceRoom(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")
	body := new(CreateSpaceRoomInput)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Validate input
	if body.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Room name is required",
		})
	}

	if body.Type != types.RoomTypeTextRoom && body.Type != types.RoomTypeVoiceRoom && body.Type != types.RoomTypeSpaceSection {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid room type. Must be 2 (Text Room), 3 (Voice Room), or 4 (Space Section)",
		})
	}

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user has permission to manage channels
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, user.ID, strconv.FormatInt(spaceID, 10), utils.MANAGE_CHANNELS)
	if err != nil {
		log.Printf("[CreateSpaceRoom] Error checking permissions: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage channels in this space",
		})
	}

	// Generate room ID
	roomID := helpers.GenerateRoomID().Int64()
	userIDInt64, err := strconv.ParseInt(user.ID, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Invalid user ID",
		})
	}

	// Get the highest position for rooms in this space/section
	position, err := getNextRoomPosition(spaceID, body.ParentID)
	if err != nil {
		log.Printf("[CreateSpaceRoom] Error getting next position: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to determine room position",
		})
	}

	now := time.Now()

	// Create room
	room := models.Room{
		ID:         roomID,
		Creator:    &userIDInt64,
		Recipients: []int64{}, // Empty for space rooms
		Type:       body.Type,
		SpaceID:    &spaceID,
		ParentID:   body.ParentID,
		Name:       &body.Name,
		Topic:      body.Topic,
		Position:   &position,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Insert room into database
	query := models.RoomTable.InsertBuilder().Query(*database.Session).BindStruct(&room)
	if err := query.ExecRelease(); err != nil {
		log.Printf("[CreateSpaceRoom] Error creating room: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create room",
		})
	}

	// Handle private room permissions
	if body.IsPrivate && len(body.AllowedRoles) > 0 {
		if err := createRoomRolePermissions(roomID, body.AllowedRoles); err != nil {
			log.Printf("[CreateSpaceRoom] Error creating room role permissions: %v", err)
			// Don't fail the room creation, just log the error
		}
	}

	// Convert to response format
	roomResponse := types.Room{
		ID:         strconv.FormatInt(roomID, 10),
		Creator:    &user.ID,
		Recipients: []string{},
		Type:       body.Type,
		SpaceID:    &spaceID,
		ParentID:   body.ParentID,
		Name:       &body.Name,
		Topic:      body.Topic,
		Position:   &position,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Publish ROOM_CREATE event to Redis for real-time updates
	if err := publishRoomCreateEvent(roomResponse, spaceID); err != nil {
		log.Printf("[CreateSpaceRoom] Error publishing room create event: %v", err)
		// Don't fail the request, just log the error
	}

	log.Printf("[CreateSpaceRoom] Successfully created room %s in space %d", roomResponse.ID, spaceID)

	return c.Status(fiber.StatusCreated).JSON(roomResponse)
}

// Note: checkSpaceMemberPermission and checkSpaceOwnerOrAdmin functions removed
// Now using global utils.CheckPermissionFromContext function

// Helper function to validate parent section
func validateParentSection(parentID string, spaceID int64) (bool, error) {
	// Query the parent room directly using string ID
	query := "SELECT type, space_id FROM rooms WHERE id = ? ALLOW FILTERING"
	var roomType int
	var roomSpaceID *int64
	if err := database.Session.Session.Query(query, parentID).Scan(&roomType, &roomSpaceID); err != nil {
		if err == gocql.ErrNotFound {
			return false, nil // Parent section doesn't exist
		}
		return false, err
	}

	// Check if parent is a section and belongs to the same space
	return roomType == types.RoomTypeSpaceSection && roomSpaceID != nil && *roomSpaceID == spaceID, nil
}

// Helper function to get next room position
func getNextRoomPosition(spaceID int64, parentID *string) (int, error) {
	var maxPosition int

	if parentID != nil {
		// Query all positions for rooms with this parent
		query := "SELECT position FROM rooms WHERE space_id = ? AND parent_id = ? ALLOW FILTERING"
		iter := database.Session.Session.Query(query, spaceID, *parentID).Iter()
		var position *int
		for iter.Scan(&position) {
			if position != nil && *position > maxPosition {
				maxPosition = *position
			}
		}
		if err := iter.Close(); err != nil {
			return 0, fmt.Errorf("failed to query rooms: %v", err)
		}
	} else {
		// Get all rooms for this space and filter client-side for null parent_id
		query := "SELECT position, parent_id, type FROM rooms WHERE space_id = ? ALLOW FILTERING"
		iter := database.Session.Session.Query(query, spaceID).Iter()
		var position *int
		var parentIDResult *string
		var roomType int
		for iter.Scan(&position, &parentIDResult, &roomType) {
			// Filter for rooms with no parent and correct type (including sections)
			if parentIDResult == nil && (roomType == types.RoomTypeTextRoom || roomType == types.RoomTypeVoiceRoom || roomType == types.RoomTypeSpaceSection) {
				if position != nil && *position > maxPosition {
					maxPosition = *position
				}
			}
		}
		if err := iter.Close(); err != nil {
			return 0, fmt.Errorf("failed to query rooms: %v", err)
		}
	}

	return maxPosition + 1, nil
}

// Helper function to create room role permissions for private rooms
func createRoomRolePermissions(roomID int64, allowedRoles []string) error {
	// This would be implemented if you have a room_role_permissions table
	// For now, we'll just log that private room permissions would be set
	log.Printf("[CreateSpaceRoom] Private room %d would have permissions for roles: %v", roomID, allowedRoles)
	return nil
}

// Helper function to publish room create event
func publishRoomCreateEvent(room types.Room, spaceID int64) error {
	// Structure the event data to match Stargate's expectations
	eventData := map[string]interface{}{
		"type":       "ROOM_CREATE",
		"sender_id":  *room.Creator,
		"created_at": time.Now().Unix(),
		"space_id":   strconv.FormatInt(spaceID, 10),
		"data": map[string]interface{}{
			"id":         room.ID,
			"name":       room.Name,
			"type":       room.Type,
			"recipients": room.Recipients,
			"creator":    room.Creator,
			"parent_id":  room.ParentID,
			"created_at": room.CreatedAt,
			"updated_at": room.UpdatedAt,
			"space_id":   strconv.FormatInt(spaceID, 10),
		},
	}

	eventJSON, err := json.Marshal(eventData)
	if err != nil {
		return fmt.Errorf("failed to marshal event data: %v", err)
	}

	// Publish to Redis channel for real-time updates
	if err := events.PublishEvent("ROOM_CREATE", string(eventJSON)); err != nil {
		return fmt.Errorf("failed to publish room create event: %v", err)
	}

	return nil
}
