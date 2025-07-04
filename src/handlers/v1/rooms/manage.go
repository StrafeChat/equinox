package handlers_v1

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type AddMemberInput struct {
	UserID  string   `json:"user_id,omitempty"`
	UserIDs []string `json:"user_ids,omitempty"`
}

type RemoveMemberInput struct {
	UserID string `json:"user_id" validate:"required"`
}

type TransferOwnershipInput struct {
	NewOwnerID string `json:"new_owner_id" validate:"required"`
}

type UpdateRoomInput struct {
	Name  *string `json:"name,omitempty"`
	Topic *string `json:"topic,omitempty"`
	Icon  *string `json:"icon,omitempty"`
}

// DeleteRoom deletes a PM group (only by creator, unless they left)
func DeleteRoom(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")

	log.Printf("DeleteRoom: Started for roomID=%s, userID=%s", roomID, user.ID)

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	// Get the room details
	log.Printf("RemoveMember: Fetching room details for roomID=%s", roomID)
	var room types.Room
	roomQ := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQ.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("RemoveMember: Failed to execute room query: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch room",
			"error":   err.Error(),
		})
	}

	if err := roomQ.Get(&room); err != nil {
		log.Printf("RemoveMember: Room not found: %v", err)
		roomQ.Release()
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "Room not found",
			"error":   err.Error(),
		})
	}
	roomQ.Release()
	log.Printf("RemoveMember: Room found - type=%d, creator=%v, recipients=%v", room.Type, room.Creator, room.Recipients)

	// Only allow deletion of group PMs
	if room.Type != types.RoomTypeGroupPM {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Only group PMs can be deleted",
		})
	}

	// Check if user has permission to delete
	// Only the creator can delete, unless they left the group
	if room.Creator != nil && *room.Creator == user.ID {
		// Creator can always delete
	} else {
		// Check if the creator left the group
		if room.Creator != nil {
			// Check if creator is still in recipients
			creatorInGroup := false
			for _, recipientID := range room.Recipients {
				if recipientID == *room.Creator {
					creatorInGroup = true
					break
				}
			}
			if creatorInGroup {
				return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
					"message": "Only the group creator can delete this room",
				})
			}
			// Creator left, any remaining member can delete
		}
	}

	// Check if user is in the room
	userInRoom := false
	for _, recipientID := range room.Recipients {
		if recipientID == user.ID {
			userInRoom = true
			break
		}
	}
	if !userInRoom {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You are not a member of this room",
		})
	}

	// Delete all room recipients
	deleteRecipientsQ := models.RoomRecipientByUserTable.DeleteQuery(*database.Session)
	if err := deleteRecipientsQ.BindMap(map[string]interface{}{
		"room_id": roomID,
	}).ExecRelease(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to delete room recipients",
			"error":   err.Error(),
		})
	}

	// Delete the room
	deleteRoomQ := models.RoomTable.DeleteQuery(*database.Session)
	if err := deleteRoomQ.BindMap(map[string]interface{}{
		"id": roomID,
	}).ExecRelease(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to delete room",
			"error":   err.Error(),
		})
	}

	// Publish room deletion event to Redis
	eventData := map[string]interface{}{
		"type": "ROOM_DELETE",
		"data": map[string]interface{}{
			"room_id":    roomID,
			"deleted_by": user.ID,
		},
	}

	eventBytes, err := json.Marshal(eventData)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to create room deletion event",
			"error":   err.Error(),
		})
	}

	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventBytes)).Err(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish room deletion event",
			"error":   err.Error(),
		})
	}

	log.Printf("DeleteRoom: Successfully deleted room %s by user %s", roomID, user.ID)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Room deleted successfully",
	})
}

// AddMember adds a user to a PM group (friends only)
func AddMember(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")
	body := new(AddMemberInput)

	log.Printf("AddMember: Started for roomID=%s, userID=%s", roomID, user.ID)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
		})
	}

	// Determine which user IDs to process
	var userIDsToAdd []string
	if body.UserID != "" {
		userIDsToAdd = []string{body.UserID}
	} else if len(body.UserIDs) > 0 {
		userIDsToAdd = body.UserIDs
	} else {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID and User ID(s) are required",
		})
	}

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	// Get the room details
	log.Printf("RemoveMember: Fetching room details for roomID=%s", roomID)
	var room types.Room
	roomQ := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQ.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("RemoveMember: Failed to execute room query: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch room",
			"error":   err.Error(),
		})
	}

	if err := roomQ.Get(&room); err != nil {
		log.Printf("RemoveMember: Room not found: %v", err)
		roomQ.Release()
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "Room not found",
			"error":   err.Error(),
		})
	}
	roomQ.Release()
	log.Printf("RemoveMember: Room found - type=%d, creator=%v, recipients=%v", room.Type, room.Creator, room.Recipients)

	// Only allow adding to group PMs
	if room.Type != types.RoomTypeGroupPM {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Can only add members to group PMs",
		})
	}

	// Check if user is in the room
	userInRoom := false
	for _, recipientID := range room.Recipients {
		if recipientID == user.ID {
			userInRoom = true
			break
		}
	}
	if !userInRoom {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You are not a member of this room",
		})
	}

	var addedUsers []string

	// Process each user ID
	for _, targetUserID := range userIDsToAdd {
		// Check if target user is already in the room
		alreadyInRoom := false
		for _, recipientID := range room.Recipients {
			if recipientID == targetUserID {
				alreadyInRoom = true
				break
			}
		}
		if alreadyInRoom {
			log.Printf("AddMember: User %s is already in room %s, skipping", targetUserID, roomID)
			continue
		}

		// Check if the current user and target user are friends
		areFriends, err := helpers.CheckExistingRelationship(user.ID, targetUserID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to check friendship status",
				"error":   err.Error(),
			})
		}

		if !areFriends {
			log.Printf("AddMember: User %s is not a friend of %s, skipping", targetUserID, user.ID)
			continue
		}

		// Add user to recipients array
		room.Recipients = append(room.Recipients, targetUserID)
		addedUsers = append(addedUsers, targetUserID)

		// Create RoomRecipientByUser entry for the new member
		recipientEntry := models.RoomRecipientByUser{
			UserId:    targetUserID,
			RoomId:    roomID,
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
		}

		recipientQ := models.RoomRecipientByUserTable.InsertQuery(*database.Session)
		if err := recipientQ.BindStruct(recipientEntry).ExecRelease(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to create room recipient entry",
				"error":   err.Error(),
			})
		}

		log.Printf("AddMember: Successfully added user %s to room %s", targetUserID, roomID)
	}

	if len(addedUsers) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "No users were added (already members or not friends)",
		})
	}

	// Update the room with new recipients
	room.UpdatedAt = time.Now()
	if err := models.RoomTable.UpdateBuilder().
		Set("recipients", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"recipients": room.Recipients,
			"updated_at": room.UpdatedAt,
			"id":         room.ID,
		}).ExecRelease(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to update room",
			"error":   err.Error(),
		})
	}

	// Publish events for each added user
	for _, addedUserID := range addedUsers {
		// Send ROOM_CREATE event to the added user (so they get the full room data)
		roomCreateEventData := map[string]interface{}{
			"type": "ROOM_CREATE",
			"data": map[string]interface{}{
				"id":              room.ID,
				"name":            room.Name,
				"type":            room.Type,
				"recipients":      room.Recipients,
				"creator":         room.Creator,
				"last_message_id": room.LastMessageId,
				"icon":            room.Icon,
				"topic":           room.Topic,
				"created_at":      room.CreatedAt,
				"updated_at":      room.UpdatedAt,
			},
			"target_user": addedUserID,
		}

		roomCreateEventBytes, err := json.Marshal(roomCreateEventData)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to create room creation event",
				"error":   err.Error(),
			})
		}

		if err := database.Rdb.Publish("ROOM_EVENTS", string(roomCreateEventBytes)).Err(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to publish room creation event",
				"error":   err.Error(),
			})
		}

		// Send ROOM_MEMBER_ADD event to existing members (excluding the added user)
		memberAddEventData := map[string]interface{}{
			"type": "ROOM_MEMBER_ADD",
			"data": map[string]interface{}{
				"room_id":    roomID,
				"user_id":    addedUserID,
				"added_by":   user.ID,
				"recipients": room.Recipients,
			},
		}

		memberAddEventBytes, err := json.Marshal(memberAddEventData)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to create member addition event",
				"error":   err.Error(),
			})
		}

		if err := database.Rdb.Publish("ROOM_EVENTS", string(memberAddEventBytes)).Err(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"message": "Failed to publish member addition event",
				"error":   err.Error(),
			})
		}

		// Create system message for member addition
		systemData := helpers.SystemMessageData{
			Type:    helpers.MemberAdded,
			UserID:  &addedUserID,
			ActorID: &user.ID,
		}
		if err := helpers.CreateSystemMessage(roomID, helpers.MemberAdded, systemData); err != nil {
			log.Printf("AddMember: Failed to create system message for user %s: %v", addedUserID, err)
			// Don't return error, just log it as system messages are not critical
		}
	}

	log.Printf("AddMember: Successfully added %d users to room %s by user %s", len(addedUsers), roomID, user.ID)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message":     fmt.Sprintf("%d member(s) added successfully", len(addedUsers)),
		"added_users": addedUsers,
		"recipients":  room.Recipients,
	})
}

// RemoveMember removes a user from a PM group
func RemoveMember(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")
	body := new(RemoveMemberInput)

	log.Printf("RemoveMember: Started for roomID=%s, userID=%s", roomID, user.ID)

	if err := c.Bind().Body(body); err != nil {
		log.Printf("RemoveMember: Failed to bind request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	log.Printf("RemoveMember: Request body parsed - targetUserID=%s", body.UserID)

	if roomID == "" || body.UserID == "" {
		log.Printf("RemoveMember: Missing required parameters - roomID=%s, targetUserID=%s", roomID, body.UserID)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID and User ID are required",
		})
	}

	// Get the room details
	log.Printf("RemoveMember: Fetching room details for roomID=%s", roomID)
	var room types.Room
	roomQ := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQ.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("RemoveMember: Failed to execute room query: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch room",
			"error":   err.Error(),
		})
	}

	if err := roomQ.Get(&room); err != nil {
		log.Printf("RemoveMember: Room not found: %v", err)
		roomQ.Release()
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "Room not found",
			"error":   err.Error(),
		})
	}
	roomQ.Release()
	log.Printf("RemoveMember: Room found - type=%d, creator=%v, recipients=%v", room.Type, room.Creator, room.Recipients)

	// Only allow removing from group PMs
	if room.Type != types.RoomTypeGroupPM {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Can only remove members from group PMs",
		})
	}

	// Check if user has permission to remove members
	// Users can remove themselves, or the creator can remove others
	log.Printf("RemoveMember: Checking permissions - requestingUser=%s, targetUser=%s, roomCreator=%v", user.ID, body.UserID, room.Creator)
	canRemove := false
	if body.UserID == user.ID {
		// User can always remove themselves
		canRemove = true
		log.Printf("RemoveMember: User removing themselves - allowed")
	} else if room.Creator != nil && *room.Creator == user.ID {
		// Creator can remove others
		canRemove = true
		log.Printf("RemoveMember: Creator removing other member - allowed")
	}

	if !canRemove {
		log.Printf("RemoveMember: Permission denied - user %s cannot remove user %s", user.ID, body.UserID)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You can only remove yourself or, if you're the creator, remove other members",
		})
	}

	// Check if target user is in the room
	log.Printf("RemoveMember: Checking if user %s is in room recipients: %v", body.UserID, room.Recipients)
	targetUserIndex := -1
	for i, recipientID := range room.Recipients {
		if recipientID == body.UserID {
			targetUserIndex = i
			break
		}
	}
	if targetUserIndex == -1 {
		log.Printf("RemoveMember: User %s is not a member of room %s", body.UserID, roomID)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "User is not a member of this room",
		})
	}

	log.Printf("RemoveMember: Found user at index %d, removing from recipients", targetUserIndex)
	// Remove user from recipients array
	room.Recipients = append(room.Recipients[:targetUserIndex], room.Recipients[targetUserIndex+1:]...)
	room.UpdatedAt = time.Now()
	log.Printf("RemoveMember: Updated recipients list: %v", room.Recipients)

	// If removing the creator, transfer ownership to the first remaining member
	if room.Creator != nil && *room.Creator == body.UserID && len(room.Recipients) > 0 {
		newCreator := room.Recipients[0]
		room.Creator = &newCreator
		log.Printf("RemoveMember: Transferred ownership to new creator: %s", newCreator)
	}

	// Update the room
	log.Printf("RemoveMember: Updating room in database")
	updateFields := []string{"recipients", "updated_at"}
	updateData := qb.M{
		"recipients": room.Recipients,
		"updated_at": room.UpdatedAt,
		"id":         room.ID,
	}
	
	// If creator was changed, include it in the update
	if room.Creator != nil {
		updateFields = append(updateFields, "creator")
		updateData["creator"] = *room.Creator
	}
	
	if err := models.RoomTable.UpdateBuilder().
		Set(updateFields...).
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(updateData).ExecRelease(); err != nil {
		log.Printf("RemoveMember: Failed to update room: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to update room",
			"error":   err.Error(),
		})
	}
	log.Printf("RemoveMember: Room updated successfully")

	// Remove RoomRecipientByUser entries for this user and room
	log.Printf("RemoveMember: Removing RoomRecipientByUser entries for user %s in room %s", body.UserID, roomID)
	
	// First, get all records for this user to find the one for this room
	var roomRecipients []models.RoomRecipientByUser
	if err := models.RoomRecipientByUserTable.SelectBuilder().
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"user_id": body.UserID,
		}).SelectRelease(&roomRecipients); err != nil {
		log.Printf("RemoveMember: Failed to find RoomRecipientByUser entries: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to find room recipient entries",
			"error":   err.Error(),
		})
	}
	
	// Find and delete the specific record for this room
	var deleted bool
	for _, recipient := range roomRecipients {
		if recipient.RoomId == roomID {
			if err := models.RoomRecipientByUserTable.DeleteBuilder().
				Where(qb.Eq("user_id"), qb.Eq("created_at")).
				Query(*database.Session).
				BindMap(qb.M{
					"user_id":    body.UserID,
					"created_at": recipient.CreatedAt,
				}).ExecRelease(); err != nil {
				log.Printf("RemoveMember: Failed to remove RoomRecipientByUser entry: %v", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"message": "Failed to remove room recipient entry",
					"error":   err.Error(),
				})
			}
			deleted = true
			break
		}
	}
	
	if !deleted {
		log.Printf("RemoveMember: No RoomRecipientByUser entry found for user %s in room %s", body.UserID, roomID)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "Room recipient entry not found",
		})
	}
	
	log.Printf("RemoveMember: Successfully removed RoomRecipientByUser entry for user %s in room %s", body.UserID, roomID)

	// Publish member removal event to Redis
	log.Printf("RemoveMember: Publishing member removal event to Redis")
	eventData := map[string]interface{}{
		"type": "ROOM_MEMBER_REMOVE",
		"data": map[string]interface{}{
			"room_id":     roomID,
			"user_id":     body.UserID,
			"removed_by":  user.ID,
			"recipients":  room.Recipients,
			"new_creator": room.Creator,
		},
	}

	eventBytes, err := json.Marshal(eventData)
	if err != nil {
		log.Printf("RemoveMember: Failed to marshal event data: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to create member removal event",
			"error":   err.Error(),
		})
	}

	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("RemoveMember: Failed to publish event to Redis: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish member removal event",
			"error":   err.Error(),
		})
	}
	log.Printf("RemoveMember: Event published to Redis successfully")

	// Create system message for member removal
	systemData := helpers.SystemMessageData{
		Type:    helpers.MemberRemoved,
		UserID:  &body.UserID,
		ActorID: &user.ID,
	}
	if err := helpers.CreateSystemMessage(roomID, helpers.MemberRemoved, systemData); err != nil {
		log.Printf("RemoveMember: Failed to create system message for user %s: %v", body.UserID, err)
		// Don't return error, just log it as system messages are not critical
	}

	log.Printf("RemoveMember: Successfully removed user %s from room %s by user %s", body.UserID, roomID, user.ID)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message":     "Member removed successfully",
		"recipients":  room.Recipients,
		"new_creator": room.Creator,
	})
}

// TransferOwnership transfers ownership of a group PM to another member
func TransferOwnership(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")

	log.Printf("TransferOwnership: Started for roomID=%s, userID=%s", roomID, user.ID)

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	// Parse request body
	var input TransferOwnershipInput
	if err := c.Bind().Body(&input); err != nil {
		log.Printf("TransferOwnership: Failed to parse request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	if input.NewOwnerID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "New owner ID is required",
		})
	}

	// Get the room details
	log.Printf("TransferOwnership: Fetching room details for roomID=%s", roomID)
	var room types.Room
	roomQ := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQ.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("TransferOwnership: Failed to execute room query: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch room",
			"error":   err.Error(),
		})
	}

	if err := roomQ.Get(&room); err != nil {
		log.Printf("TransferOwnership: Room not found: %v", err)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "Room not found",
		})
	}

	log.Printf("TransferOwnership: Room found - type=%d, creator=%v, recipients=%v", room.Type, room.Creator, room.Recipients)

	// Check if room is a group PM
	if room.Type != types.RoomTypeGroupPM {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Can only transfer ownership of group PMs",
		})
	}

	// Check if user is the current owner
	if room.Creator == nil || *room.Creator != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "Only the current owner can transfer ownership",
		})
	}

	// Check if new owner is a member of the room
	found := false
	for _, recipient := range room.Recipients {
		if recipient == input.NewOwnerID {
			found = true
			break
		}
	}
	if !found {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "New owner must be a member of the room",
		})
	}

	// Update room creator
	updateQuery := qb.Update("rooms").Set("creator").Where(qb.Eq("id"))
	updateQueryExec := updateQuery.Query(*database.Session).BindMap(qb.M{
		"creator": input.NewOwnerID,
		"id":      roomID,
	})
	if err := updateQueryExec.ExecRelease(); err != nil {
		log.Printf("TransferOwnership: Failed to update room creator: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to transfer ownership",
			"error":   err.Error(),
		})
	}

	log.Printf("TransferOwnership: Successfully transferred ownership from %s to %s for room %s", user.ID, input.NewOwnerID, roomID)

	// Create a system message for the ownership transfer
	log.Printf("TransferOwnership: Creating system message for ownership transfer")
	oldOwnerID := user.ID
	newOwnerID := input.NewOwnerID
	systemMessageData := helpers.SystemMessageData{
		Type:    helpers.OwnershipTransferred,
		ActorID: &oldOwnerID,
		UserID:  &newOwnerID,
		ExtraData: map[string]interface{}{
			"old_owner": oldOwnerID,
			"new_owner": newOwnerID,
		},
	}

	if err := helpers.CreateSystemMessage(roomID, helpers.OwnershipTransferred, systemMessageData); err != nil {
		log.Printf("TransferOwnership: Failed to create system message: %v", err)
		// Continue with the process even if system message creation fails
	} else {
		log.Printf("TransferOwnership: System message created successfully")
	}

	// Publish ownership transfer event to Redis
	log.Printf("TransferOwnership: Publishing ownership transfer event to Redis")
	eventData := map[string]interface{}{
		"type": "ROOM_OWNERSHIP_TRANSFER",
		"data": map[string]interface{}{
			"room_id":   roomID,
			"old_owner": user.ID,
			"new_owner": input.NewOwnerID,
			"timestamp": time.Now().Unix(),
		},
	}

	eventBytes, err := json.Marshal(eventData)
	if err != nil {
		log.Printf("TransferOwnership: Failed to marshal event data: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to create ownership transfer event",
			"error":   err.Error(),
		})
	}

	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("TransferOwnership: Failed to publish event to Redis: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish ownership transfer event",
			"error":   err.Error(),
		})
	}
	log.Printf("TransferOwnership: Event published to Redis successfully")

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message":   "Ownership transferred successfully",
		"new_owner": input.NewOwnerID,
	})
}

// UpdateRoom updates room settings (name, topic) for group PMs
func UpdateRoom(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("id")

	log.Printf("UpdateRoom: Started for roomID=%s, userID=%s", roomID, user.ID)

	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Room ID is required",
		})
	}

	// Parse request body
	var input UpdateRoomInput
	if err := c.Bind().Body(&input); err != nil {
		log.Printf("UpdateRoom: Failed to parse request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	// Get the room details
	log.Printf("UpdateRoom: Fetching room details for roomID=%s", roomID)
	var room types.Room
	roomQ := models.RoomTable.SelectQuery(*database.Session)
	if err := roomQ.BindMap(map[string]interface{}{
		"id": roomID,
	}).Exec(); err != nil {
		log.Printf("UpdateRoom: Failed to execute room query: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to fetch room",
			"error":   err.Error(),
		})
	}

	if err := roomQ.Get(&room); err != nil {
		log.Printf("UpdateRoom: Room not found: %v", err)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"message": "Room not found",
			"error":   err.Error(),
		})
	}

	log.Printf("UpdateRoom: Room found - type=%d, creator=%v, recipients=%v", room.Type, room.Creator, room.Recipients)

	// Check if room is a group PM
	if room.Type != types.RoomTypeGroupPM {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Can only update group PM settings",
		})
	}

	// Check if user is the owner or a member
	userInRoom := false
	for _, recipient := range room.Recipients {
		if recipient == user.ID {
			userInRoom = true
			break
		}
	}
	if !userInRoom {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "You are not a member of this room",
		})
	}

	// Only the owner can update room settings
	if room.Creator == nil || *room.Creator != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"message": "Only the room owner can update settings",
		})
	}

	// Check if there are fields to update
	if input.Name == nil && input.Topic == nil && input.Icon == nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "No fields to update",
		})
	}

	// Get the existing room model for update
	var roomModel models.Room
	stmt := models.RoomTable.SelectBuilder().
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{"id": roomID})
	
	if err := stmt.GetRelease(&roomModel); err != nil {
		log.Printf("UpdateRoom: Failed to fetch room: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Room not found",
			"error":   err.Error(),
		})
	}
	
	// Update the room fields
	if input.Name != nil {
		roomModel.Name = input.Name
	}
	if input.Topic != nil {
		roomModel.Topic = input.Topic
	}
	if input.Icon != nil {
		roomModel.Icon = input.Icon
	}
	roomModel.UpdatedAt = time.Now()
	
	// Update in database using the same pattern as UpdateProfile
	// Always update all fields to keep it simple and consistent
	updateStmt := models.RoomTable.UpdateBuilder().
		Set("name", "topic", "icon", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindStruct(&roomModel)
	
	if err := updateStmt.ExecRelease(); err != nil {
		log.Printf("UpdateRoom: Failed to update room: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to update room",
			"error":   err.Error(),
		})
	}

	log.Printf("UpdateRoom: Successfully updated room %s by user %s", roomID, user.ID)

	// Publish room update event to Redis
	log.Printf("UpdateRoom: Publishing room update event to Redis")
	eventData := map[string]interface{}{
		"type": "ROOM_UPDATE",
		"data": map[string]interface{}{
			"room_id":    roomID,
			"updated_by": user.ID,
			"timestamp":  time.Now().Unix(),
		},
	}

	if input.Name != nil {
		eventData["data"].(map[string]interface{})["name"] = *input.Name
	}
	if input.Topic != nil {
		eventData["data"].(map[string]interface{})["topic"] = *input.Topic
	}
	if input.Icon != nil {
		eventData["data"].(map[string]interface{})["icon"] = *input.Icon
	}

	eventBytes, err := json.Marshal(eventData)
	if err != nil {
		log.Printf("UpdateRoom: Failed to marshal event data: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to create room update event",
			"error":   err.Error(),
		})
	}

	if err := database.Rdb.Publish("ROOM_EVENTS", string(eventBytes)).Err(); err != nil {
		log.Printf("UpdateRoom: Failed to publish event to Redis: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to publish room update event",
			"error":   err.Error(),
		})
	}
	log.Printf("UpdateRoom: Event published to Redis successfully")

	// Create system messages for room updates
	if input.Name != nil {
		systemData := helpers.SystemMessageData{
			Type:     helpers.RoomNameChanged,
			ActorID:  &user.ID,
			NewValue: input.Name,
		}
		if err := helpers.CreateSystemMessage(roomID, helpers.RoomNameChanged, systemData); err != nil {
			log.Printf("UpdateRoom: Failed to create system message for name change: %v", err)
		}
	}

	if input.Topic != nil {
		systemData := helpers.SystemMessageData{
			Type:     helpers.RoomTopicChanged,
			ActorID:  &user.ID,
			NewValue: input.Topic,
		}
		if err := helpers.CreateSystemMessage(roomID, helpers.RoomTopicChanged, systemData); err != nil {
			log.Printf("UpdateRoom: Failed to create system message for topic change: %v", err)
		}
	}

	if input.Icon != nil {
		systemData := helpers.SystemMessageData{
			Type:    helpers.RoomIconChanged,
			ActorID: &user.ID,
		}
		if err := helpers.CreateSystemMessage(roomID, helpers.RoomIconChanged, systemData); err != nil {
			log.Printf("UpdateRoom: Failed to create system message for icon change: %v", err)
		}
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Room updated successfully",
	})
}
