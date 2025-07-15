package handlers_v1

import (
	"encoding/json"
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type UpdatePositionsInput struct {
	RoomPositions []RoomPosition `json:"room_positions" validate:"required,min=1"`
}

type RoomPosition struct {
	RoomID   string  `json:"room_id" validate:"required"`
	Position int     `json:"position" validate:"required,min=0"`
	ParentID *string `json:"parent_id,omitempty"`
}

func UpdateRoomPositions(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	var input UpdatePositionsInput

	log.Printf("UpdateRoomPositions: Started for userID=%s", user.ID)

	if err := c.Bind().Body(&input); err != nil {
		log.Printf("UpdateRoomPositions: Failed to parse request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	if len(input.RoomPositions) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "No room positions provided",
		})
	}

	// Validate that user has permission to update all rooms
	for _, roomPos := range input.RoomPositions {
		// Get the room details
		var room types.Room
		roomQ := models.RoomTable.SelectQuery(*database.Session)
		if err := roomQ.BindMap(map[string]interface{}{
			"id": roomPos.RoomID,
		}).Exec(); err != nil {
			log.Printf("UpdateRoomPositions: Failed to execute room query for %s: %v", roomPos.RoomID, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to fetch room",
				"error":   err.Error(),
			})
		}

		if err := roomQ.Get(&room); err != nil {
			log.Printf("UpdateRoomPositions: Room %s not found: %v", roomPos.RoomID, err)
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"message": "Room not found: " + roomPos.RoomID,
				"error":   err.Error(),
			})
		}

		// Check if room type supports position updates
		if room.Type != types.RoomTypeTextRoom && room.Type != types.RoomTypeVoiceRoom && room.Type != types.RoomTypeSpaceSection {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"message": "Position can only be updated for space rooms and sections: " + roomPos.RoomID,
			})
		}

		// Check if user has permission to manage channels in the space
		if room.SpaceID != nil {
			// Check if user has MANAGE_CHANNELS permission in the space
			hasPermission, err := checkSpaceMemberPermission(*room.SpaceID, user.ID, "MANAGE_CHANNELS")
			if err != nil {
				log.Printf("UpdateRoomPositions: Failed to check permissions for user %s in space %d: %v", user.ID, *room.SpaceID, err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"message": "Failed to verify permissions",
				})
			}

			if !hasPermission {
				return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
					"message": "You don't have permission to update this room: " + roomPos.RoomID,
				})
			}
		}
	}

	// Update all room positions
	for _, roomPos := range input.RoomPositions {
		// Get the existing room model for update
		var roomModel models.Room
		stmt := models.RoomTable.SelectBuilder().
			Where(qb.Eq("id")).
			Query(*database.Session).
			BindMap(qb.M{"id": roomPos.RoomID})

		if err := stmt.GetRelease(&roomModel); err != nil {
			log.Printf("UpdateRoomPositions: Failed to fetch room %s: %v", roomPos.RoomID, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Room not found: " + roomPos.RoomID,
				"error":   err.Error(),
			})
		}

		// Update position
		roomModel.Position = &roomPos.Position
		roomModel.UpdatedAt = time.Now()

		// Check if parent_id is explicitly provided in the request (including null)
		// We need to check if the field exists in the JSON, not just if it's nil
		var shouldUpdateParentID bool
		var requestBytes []byte
		if requestBytes = c.Body(); len(requestBytes) > 0 {
			var rawInput map[string]interface{}
			if err := json.Unmarshal(requestBytes, &rawInput); err == nil {
				if roomPositions, ok := rawInput["room_positions"].([]interface{}); ok {
					for _, rp := range roomPositions {
						if rpMap, ok := rp.(map[string]interface{}); ok {
							if rpMap["room_id"] == roomPos.RoomID {
								_, shouldUpdateParentID = rpMap["parent_id"]
								break
							}
						}
					}
				}
			}
		}

		// Update parent_id if it was explicitly provided (including null for orphaning)
		if shouldUpdateParentID {
			log.Printf("UpdateRoomPositions: Updating parent_id for room %s from %v to %v", roomPos.RoomID, roomModel.ParentID, roomPos.ParentID)
			roomModel.ParentID = roomPos.ParentID
		} else {
			log.Printf("UpdateRoomPositions: No parent_id provided for room %s, keeping current: %v", roomPos.RoomID, roomModel.ParentID)
		}

		// Update in database
		var updateStmt *gocqlx.Queryx
		if shouldUpdateParentID {
			updateStmt = models.RoomTable.UpdateBuilder().
				Set("position", "parent_id", "updated_at").
				Where(qb.Eq("id")).
				Query(*database.Session).
				BindStruct(&roomModel)
		} else {
			updateStmt = models.RoomTable.UpdateBuilder().
				Set("position", "updated_at").
				Where(qb.Eq("id")).
				Query(*database.Session).
				BindStruct(&roomModel)
		}

		if err := updateStmt.ExecRelease(); err != nil {
			log.Printf("UpdateRoomPositions: Failed to update room %s: %v", roomPos.RoomID, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to update room position: " + roomPos.RoomID,
				"error":   err.Error(),
			})
		}

		log.Printf("UpdateRoomPositions: Successfully updated room %s position to %d", roomPos.RoomID, roomPos.Position)
	}

	// Create updated room positions data for the event
	updatedRoomPositions := make([]map[string]interface{}, len(input.RoomPositions))
	for i, roomPos := range input.RoomPositions {
		// Get the updated room data from database to include all changes
		var updatedRoom models.Room
		stmt := models.RoomTable.SelectBuilder().
			Where(qb.Eq("id")).
			Query(*database.Session).
			BindMap(qb.M{"id": roomPos.RoomID})

		if err := stmt.GetRelease(&updatedRoom); err != nil {
			log.Printf("UpdateRoomPositions: Failed to fetch updated room %s for event: %v", roomPos.RoomID, err)
			// Fallback to original data if we can't fetch updated room
			updatedRoomPositions[i] = map[string]interface{}{
				"room_id":  roomPos.RoomID,
				"position": roomPos.Position,
			}
			if roomPos.ParentID != nil {
				updatedRoomPositions[i]["parent_id"] = roomPos.ParentID
			}
		} else {
			// Include the actual updated data
			updatedRoomPositions[i] = map[string]interface{}{
				"room_id":  updatedRoom.ID,
				"position": *updatedRoom.Position,
			}
			if updatedRoom.ParentID != nil {
				updatedRoomPositions[i]["parent_id"] = *updatedRoom.ParentID
			} else {
				updatedRoomPositions[i]["parent_id"] = nil
			}
		}
	}

	// Publish room positions update event to Redis
	log.Printf("UpdateRoomPositions: Publishing room positions update event to Redis")
	eventData := map[string]interface{}{
		"type": "ROOM_POSITIONS_UPDATE",
		"data": map[string]interface{}{
			"room_positions": updatedRoomPositions,
			"updated_by":     user.ID,
			"timestamp":      time.Now().Unix(),
		},
	}

	eventBytes, err := json.Marshal(eventData)
	if err != nil {
		log.Printf("UpdateRoomPositions: Failed to marshal event data: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to create room positions update event",
			"error":   err.Error(),
		})
	}

	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("UpdateRoomPositions: Failed to publish event to Redis: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish room positions update event",
			"error":   err.Error(),
		})
	}
	log.Printf("UpdateRoomPositions: Event published to Redis successfully")

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Room positions updated successfully",
	})
}
