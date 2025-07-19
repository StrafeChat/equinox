package handlers_v1

import (
	"log"
	"strconv"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

func GetUserRooms(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)

	// Query RoomRecipientByUser to get PM and Group PM rooms
	roomRecipientQuery := models.RoomRecipientByUserTable.SelectBuilder().
		Columns("room_id").
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"user_id": user.ID,
		})

	var roomRecipients []models.RoomRecipientByUser
	if err := roomRecipientQuery.SelectRelease(&roomRecipients); err != nil {
		log.Printf("Error fetching user's PM/Group rooms: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to fetch user's rooms",
		})
	}

	// Get the full room data for PM and Group PM rooms
	var rooms []types.Room
	for _, recipient := range roomRecipients {
		var room types.Room
		roomQuery := models.RoomTable.SelectBuilder().
			Columns("*").
			Where(qb.Eq("id")).
			Limit(1).
			Query(*database.Session).
			BindMap(qb.M{
				"id": recipient.RoomId,
			})

		if err := roomQuery.GetRelease(&room); err != nil {
			log.Printf("Error fetching room details for room ID %s: %v", recipient.RoomId, err)
			continue
		}

		rooms = append(rooms, room)
	}

	// Get user's spaces to find space rooms
	var spaceMemberships []models.SpaceMembersByUser
	spaceMemberQuery := models.SpaceMembersByUserTable.SelectBuilder().
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		Bind(user.ID)
	if err := spaceMemberQuery.SelectRelease(&spaceMemberships); err != nil {
		log.Printf("Error fetching user's spaces: %v", err)
		// Don't fail the request, just skip space rooms
	} else {
		// For each space, get all rooms and check VIEW_ROOMS permission
		for _, membership := range spaceMemberships {
			spaceIDStr := strconv.FormatInt(membership.SpaceID, 10)

			// Get all rooms for this space
			var spaceRooms []models.Room
			spaceRoomQuery := models.RoomTable.SelectBuilder().
				Columns("*").
				Where(qb.Eq("space_id")).
				Query(*database.Session).
				Bind(membership.SpaceID)
			if err := spaceRoomQuery.SelectRelease(&spaceRooms); err != nil {
				log.Printf("Error fetching rooms for space %d: %v", membership.SpaceID, err)
				continue
			}

			// Check VIEW_ROOMS permission for each room
			for _, spaceRoom := range spaceRooms {
				// Only check permission for text/voice rooms, not sections
				if spaceRoom.Type == types.RoomTypeTextRoom || spaceRoom.Type == types.RoomTypeVoiceRoom {
					hasPermission, err := utils.CheckPermissionFromContext(database.Session, user.ID, spaceIDStr, utils.VIEW_ROOMS)
					if err != nil {
						log.Printf("Error checking VIEW_ROOMS permission for room %d in space %s: %v", spaceRoom.ID, spaceIDStr, err)
						continue
					}
					if hasPermission {
						// Convert to response type
						room := types.Room{
							ID:         strconv.FormatInt(spaceRoom.ID, 10),
							Creator:    convertInt64PtrToStringPtr(spaceRoom.Creator),
							Recipients: convertInt64SliceToStringSlice(spaceRoom.Recipients),
							Type:       spaceRoom.Type,
							SpaceID:    &membership.SpaceID,
							ParentID:   spaceRoom.ParentID,
							Name:       spaceRoom.Name,
							Topic:      spaceRoom.Topic,
							Position:   spaceRoom.Position,
							CreatedAt:  spaceRoom.CreatedAt,
							UpdatedAt:  spaceRoom.UpdatedAt,
						}
						rooms = append(rooms, room)
					}
				} else {
					// Include sections without permission check
					room := types.Room{
						ID:         strconv.FormatInt(spaceRoom.ID, 10),
						Creator:    convertInt64PtrToStringPtr(spaceRoom.Creator),
						Recipients: convertInt64SliceToStringSlice(spaceRoom.Recipients),
						Type:       spaceRoom.Type,
						SpaceID:    &membership.SpaceID,
						ParentID:   spaceRoom.ParentID,
						Name:       spaceRoom.Name,
						Topic:      spaceRoom.Topic,
						Position:   spaceRoom.Position,
						CreatedAt:  spaceRoom.CreatedAt,
						UpdatedAt:  spaceRoom.UpdatedAt,
					}
					rooms = append(rooms, room)
				}
			}
		}
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"rooms": rooms,
	})
}

// Helper function to convert *int64 to *string
func convertInt64PtrToStringPtr(ptr *int64) *string {
	if ptr == nil {
		return nil
	}
	str := strconv.FormatInt(*ptr, 10)
	return &str
}

// Helper function to convert []int64 to []string
func convertInt64SliceToStringSlice(slice []int64) []string {
	result := make([]string, len(slice))
	for i, v := range slice {
		result[i] = strconv.FormatInt(v, 10)
	}
	return result
}
