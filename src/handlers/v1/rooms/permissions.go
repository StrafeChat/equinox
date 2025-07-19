package handlers_v1

import (
	"log"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/repository"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/StrafeChat/equinox/src/utils"
)

// Permission override types
type PermissionOverrideType string

const (
	OverrideGrant   PermissionOverrideType = "grant"
	OverrideDeny    PermissionOverrideType = "deny"
	OverrideDefault PermissionOverrideType = "default"
)

// Request/Response structures
type PermissionOverrideInput struct {
	PermissionID string                 `json:"permission_id" validate:"required"`
	Override     PermissionOverrideType `json:"override" validate:"required,oneof=grant deny default"`
}

type SetRolePermissionOverridesInput struct {
	RoleID    string                    `json:"role_id" validate:"required"`
	Overrides []PermissionOverrideInput `json:"overrides" validate:"required,min=1"`
}

type SetMemberPermissionOverridesInput struct {
	UserID    string                    `json:"user_id" validate:"required"`
	Overrides []PermissionOverrideInput `json:"overrides" validate:"required,min=1"`
}

type PermissionOverrideResponse struct {
	PermissionID string                 `json:"permission_id"`
	Override     PermissionOverrideType `json:"override"`
}

type RolePermissionOverridesResponse struct {
	RoleID    string                       `json:"role_id"`
	Overrides []PermissionOverrideResponse `json:"overrides"`
}

type MemberPermissionOverridesResponse struct {
	UserID    string                       `json:"user_id"`
	Overrides []PermissionOverrideResponse `json:"overrides"`
}

type RoomPermissionOverridesResponse struct {
	RoomID  string                              `json:"room_id"`
	Roles   []RolePermissionOverridesResponse   `json:"roles"`
	Members []MemberPermissionOverridesResponse `json:"members"`
}

// UpdateRolePermissionOverride updates a single permission override for a role in a room
func UpdateRolePermissionOverride(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("roomId")
	roleID := c.Params("roleId")
	permissionID := c.Params("permissionId")

	var input struct {
		Override string `json:"override"`
	}
	if err := c.Bind().Body(&input); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	// Check if user has permission to manage room permissions
	hasPermission, err := checkRoomManagePermission(roomID, user.ID)
	if err != nil {
		log.Printf("Error checking room manage permission: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Internal server error"})
	}

	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "You don't have permission to manage room permissions"})
	}

	// Update the permission override
	roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
	err = roomPermRepo.UpdateRolePermissionOverride(roomID, roleID, permissionID, input.Override)
	if err != nil {
		log.Printf("Error updating role permission override: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to update permission override"})
	}

	return c.JSON(fiber.Map{"message": "Permission override updated successfully"})
}

// UpdateMemberPermissionOverride updates a single permission override for a member in a room
func UpdateMemberPermissionOverride(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("roomId")
	userID := c.Params("userId")
	permissionID := c.Params("permissionId")

	var input struct {
		Override string `json:"override"`
	}
	if err := c.Bind().Body(&input); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	// Check if user has permission to manage room permissions
	hasPermission, err := checkRoomManagePermission(roomID, user.ID)
	if err != nil {
		log.Printf("Error checking room manage permission: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Internal server error"})
	}

	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "You don't have permission to manage room permissions"})
	}

	// Update the permission override
	roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
	err = roomPermRepo.UpdateMemberPermissionOverride(roomID, userID, permissionID, input.Override)
	if err != nil {
		log.Printf("Error updating member permission override: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to update permission override"})
	}

	return c.JSON(fiber.Map{"message": "Permission override updated successfully"})
}

// SetRolePermissionOverrides sets permission overrides for a role in a room
func SetRolePermissionOverrides(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("roomId")

	var input SetRolePermissionOverridesInput
	if err := c.Bind().Body(&input); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	// Check if user has permission to manage room permissions
	hasPermission, err := checkRoomManagePermission(roomID, user.ID)
	if err != nil {
		log.Printf("Error checking room manage permission: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Internal server error"})
	}

	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "You don't have permission to manage room permissions"})
	}

	// Convert overrides to granted and denied permission lists
	grantedPermissions := []string{}
	deniedPermissions := []string{}

	for _, override := range input.Overrides {
		switch override.Override {
		case OverrideGrant:
			grantedPermissions = append(grantedPermissions, override.PermissionID)
		case OverrideDeny:
			deniedPermissions = append(deniedPermissions, override.PermissionID)
			// OverrideDefault means no override, so we don't add it to either list
		}
	}

	// Set the permission overrides
	roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
	err = roomPermRepo.SetRolePermissionOverride(roomID, input.RoleID, grantedPermissions, deniedPermissions)
	if err != nil {
		log.Printf("Error setting role permission overrides: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to set permission overrides"})
	}

	return c.JSON(fiber.Map{"message": "Permission overrides set successfully"})
}

// SetMemberPermissionOverrides sets permission overrides for a member in a room
func SetMemberPermissionOverrides(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("roomId")

	var input SetMemberPermissionOverridesInput
	if err := c.Bind().Body(&input); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	// Check if user has permission to manage room permissions
	hasPermission, err := checkRoomManagePermission(roomID, user.ID)
	if err != nil {
		log.Printf("Error checking room manage permission: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Internal server error"})
	}

	if !hasPermission {
		return c.Status(403).JSON(fiber.Map{"error": "You don't have permission to manage room permissions"})
	}

	// Convert overrides to granted and denied permission lists
	grantedPermissions := []string{}
	deniedPermissions := []string{}

	for _, override := range input.Overrides {
		switch override.Override {
		case OverrideGrant:
			grantedPermissions = append(grantedPermissions, override.PermissionID)
		case OverrideDeny:
			deniedPermissions = append(deniedPermissions, override.PermissionID)
			// OverrideDefault means no override, so we don't add it to either list
		}
	}

	// Set the permission overrides
	roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
	err = roomPermRepo.SetMemberPermissionOverride(roomID, input.UserID, grantedPermissions, deniedPermissions)
	if err != nil {
		log.Printf("Error setting member permission overrides: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to set permission overrides"})
	}

	return c.JSON(fiber.Map{"message": "Permission overrides set successfully"})
}

// GetRoomPermissionOverrides gets all permission overrides for a room
func GetRoomPermissionOverrides(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("roomId")

	// Check if user has permission to view room permissions
	hasPermission, err := checkRoomViewPermission(roomID, user.ID)
	if err != nil {
		log.Printf("Error checking room view permission: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Internal server error"})
	}

	if !hasPermission {
		return c.Status(403).JSON(fiber.Map{"error": "You don't have permission to view this room"})
	}

	roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)

	// Get role overrides
	roleOverrides, err := roomPermRepo.GetRolePermissionOverrides(roomID)
	if err != nil {
		log.Printf("Error getting role overrides: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to get permission overrides"})
	}

	// Get member overrides
	memberOverrides, err := roomPermRepo.GetMemberPermissionOverrides(roomID)
	if err != nil {
		log.Printf("Error getting member overrides: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to get permission overrides"})
	}

	// Convert to response format
	roleResponses := []RolePermissionOverridesResponse{}
	for _, override := range roleOverrides {
		overrides := convertBitmapsToOverrides(override.Permissions, override.Denied)
		roleResponses = append(roleResponses, RolePermissionOverridesResponse{
			RoleID:    override.RoleID,
			Overrides: overrides,
		})
	}

	memberResponses := []MemberPermissionOverridesResponse{}
	for _, override := range memberOverrides {
		overrides := convertBitmapsToOverrides(override.Permissions, override.Denied)
		memberResponses = append(memberResponses, MemberPermissionOverridesResponse{
			UserID:    override.UserID,
			Overrides: overrides,
		})
	}

	response := RoomPermissionOverridesResponse{
		RoomID:  roomID,
		Roles:   roleResponses,
		Members: memberResponses,
	}

	return c.JSON(response)
}

// DeleteRolePermissionOverrides deletes all permission overrides for a role in a room
func DeleteRolePermissionOverrides(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("roomId")
	roleID := c.Params("roleId")

	// Check if user has permission to manage room permissions
	hasPermission, err := checkRoomManagePermission(roomID, user.ID)
	if err != nil {
		log.Printf("Error checking room manage permission: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Internal server error"})
	}

	if !hasPermission {
		return c.Status(403).JSON(fiber.Map{"error": "You don't have permission to manage room permissions"})
	}

	roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
	err = roomPermRepo.DeleteRolePermissionOverride(roomID, roleID)
	if err != nil {
		log.Printf("Error deleting role permission overrides: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to delete permission overrides"})
	}

	return c.JSON(fiber.Map{"message": "Permission overrides deleted successfully"})
}

// DeleteMemberPermissionOverrides deletes all permission overrides for a member in a room
func DeleteMemberPermissionOverrides(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	roomID := c.Params("roomId")
	userID := c.Params("userId")

	// Check if user has permission to manage room permissions
	hasPermission, err := checkRoomManagePermission(roomID, user.ID)
	if err != nil {
		log.Printf("Error checking room manage permission: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Internal server error"})
	}

	if !hasPermission {
		return c.Status(403).JSON(fiber.Map{"error": "You don't have permission to manage room permissions"})
	}

	roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
	err = roomPermRepo.DeleteMemberPermissionOverride(roomID, userID)
	if err != nil {
		log.Printf("Error deleting member permission overrides: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to delete permission overrides"})
	}

	return c.JSON(fiber.Map{"message": "Permission overrides deleted successfully"})
}

// Helper functions

// checkRoomManagePermission checks if a user can manage permissions in a room
func checkRoomManagePermission(roomID, userID string) (bool, error) {
	// Get room details
	var room types.Room
	roomQuery := models.RoomTable.SelectQuery(*database.Session)
	defer roomQuery.Release()

	if err := roomQuery.BindMap(map[string]interface{}{"id": roomID}).Get(&room); err != nil {
		return false, err
	}

	// For space rooms, check MANAGE_CHANNELS permission using room permission system
	if room.Type == types.RoomTypeTextRoom || room.Type == types.RoomTypeVoiceRoom || room.Type == types.RoomTypeSpaceSection {
		if room.SpaceID == nil {
			return false, nil
		}

		// Use room permission repository to respect hierarchy: space owner > room overrides > space permissions
		roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
		return roomPermRepo.HasPermissionInRoom(roomID, userID, utils.MANAGE_CHANNELS, *room.SpaceID)
	}

	// For DMs and Group DMs, only participants can manage (though this doesn't make much sense)
	for _, recipient := range room.Recipients {
		if recipient == userID {
			return true, nil
		}
	}

	return false, nil
}

// checkRoomViewPermission checks if a user can view a room
func checkRoomViewPermission(roomID, userID string) (bool, error) {
	// Get room details
	var room types.Room
	roomQuery := models.RoomTable.SelectQuery(*database.Session)
	defer roomQuery.Release()

	if err := roomQuery.BindMap(map[string]interface{}{"id": roomID}).Get(&room); err != nil {
		return false, err
	}

	// For space rooms, check VIEW_ROOMS permission using room permission system
	if room.Type == types.RoomTypeTextRoom || room.Type == types.RoomTypeVoiceRoom || room.Type == types.RoomTypeSpaceSection {
		if room.SpaceID == nil {
			return false, nil
		}

		// Use room permission repository to respect hierarchy: space owner > room overrides > space permissions
		roomPermRepo := repository.NewRoomPermissionsRepository(database.Session)
		return roomPermRepo.HasPermissionInRoom(roomID, userID, utils.VIEW_ROOMS, *room.SpaceID)
	}

	// For DMs and Group DMs, only participants can view
	for _, recipient := range room.Recipients {
		if recipient == userID {
			return true, nil
		}
	}

	return false, nil
}

// convertBitmapsToOverrides converts granted and denied bitmaps to override responses
func convertBitmapsToOverrides(grantedBitmap, deniedBitmap int64) []PermissionOverrideResponse {
	overrides := []PermissionOverrideResponse{}

	// Get all available permissions for room overrides
	allPermissions := utils.GetRoomOverridePermissions()

	for _, perm := range allPermissions {
		permValue := utils.PermissionsToBitfield([]string{perm.ID})

		if grantedBitmap&int64(permValue) != 0 {
			overrides = append(overrides, PermissionOverrideResponse{
				PermissionID: perm.ID,
				Override:     OverrideGrant,
			})
		} else if deniedBitmap&int64(permValue) != 0 {
			overrides = append(overrides, PermissionOverrideResponse{
				PermissionID: perm.ID,
				Override:     OverrideDeny,
			})
		} else {
			// If neither granted nor denied, it's default
			overrides = append(overrides, PermissionOverrideResponse{
				PermissionID: perm.ID,
				Override:     OverrideDefault,
			})
		}
	}

	return overrides
}
