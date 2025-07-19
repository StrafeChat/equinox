package repository

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v2"
	"github.com/scylladb/gocqlx/v2/qb"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/utils"
)

type RoomPermissionsRepository struct {
	session *gocqlx.Session
}

func NewRoomPermissionsRepository(session *gocqlx.Session) *RoomPermissionsRepository {
	return &RoomPermissionsRepository{
		session: session,
	}
}

// SetRolePermissionOverride sets permission overrides for a role in a specific room
func (r *RoomPermissionsRepository) SetRolePermissionOverride(roomID, roleID string, grantedPermissions, deniedPermissions []string) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	now := time.Now()
	grantedBitmap := int64(utils.PermissionsToBitfield(grantedPermissions))
	deniedBitmap := int64(utils.PermissionsToBitfield(deniedPermissions))

	// If both bitmaps are 0, it means all permissions are set to default
	// In this case, we should delete the override entry entirely
	if grantedBitmap == 0 && deniedBitmap == 0 {
		return r.DeleteRolePermissionOverride(roomID, roleID)
	}

	// Check if override already exists
	var existing models.RoomRolePermissions
	query := models.RoomRolePermissionsTable.SelectBuilder().Where(qb.Eq("room_id"), qb.Eq("role_id")).Query(*r.session)
	defer query.Release()

	err := query.BindMap(qb.M{
		"room_id": roomID,
		"role_id": roleID,
	}).Get(&existing)

	if err != nil && err != gocql.ErrNotFound {
		return fmt.Errorf("failed to check existing override: %v", err)
	}

	if err == gocql.ErrNotFound {
		// Create new override
		override := models.RoomRolePermissions{
			RoomID:      roomID,
			RoleID:      roleID,
			Permissions: grantedBitmap,
			Denied:      deniedBitmap,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		insertQuery := models.RoomRolePermissionsTable.InsertBuilder().Query(*r.session)
		defer insertQuery.Release()

		return insertQuery.BindStruct(&override).Exec()
	} else {
		// Update existing override
		updateQuery := models.RoomRolePermissionsTable.UpdateBuilder().
			Set("permissions", "denied", "updated_at").
			Where(qb.Eq("room_id"), qb.Eq("role_id")).
			Query(*r.session)
		defer updateQuery.Release()

		return updateQuery.BindMap(qb.M{
			"room_id":     roomID,
			"role_id":     roleID,
			"permissions": grantedBitmap,
			"denied":      deniedBitmap,
			"updated_at":  now,
		}).Exec()
	}
}

// UpdateRolePermissionOverride updates individual permission overrides for a role
func (r *RoomPermissionsRepository) UpdateRolePermissionOverride(roomID, roleID, permissionID string, override string) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	now := time.Now()
	permissionBit := int64(utils.PermissionsToBitfield([]string{permissionID}))

	// Check if override already exists
	var existing models.RoomRolePermissions
	query := models.RoomRolePermissionsTable.SelectBuilder().Where(qb.Eq("room_id"), qb.Eq("role_id")).Query(*r.session)
	defer query.Release()

	err := query.BindMap(qb.M{
		"room_id": roomID,
		"role_id": roleID,
	}).Get(&existing)

	if err != nil && err != gocql.ErrNotFound {
		return fmt.Errorf("failed to check existing override: %v", err)
	}

	var newGrantedBitmap, newDeniedBitmap int64

	if err == gocql.ErrNotFound {
		// No existing override, start with empty bitmaps
		newGrantedBitmap = 0
		newDeniedBitmap = 0
	} else {
		// Start with existing bitmaps
		newGrantedBitmap = existing.Permissions
		newDeniedBitmap = existing.Denied
	}

	// Update bitmaps based on override type
	switch override {
	case "grant":
		// Add to granted, remove from denied
		newGrantedBitmap |= permissionBit
		newDeniedBitmap &= ^permissionBit
	case "deny":
		// Add to denied, remove from granted
		newDeniedBitmap |= permissionBit
		newGrantedBitmap &= ^permissionBit
	case "default":
		// Remove from both bitmaps
		newGrantedBitmap &= ^permissionBit
		newDeniedBitmap &= ^permissionBit
	}

	// If both bitmaps are now 0, delete the override entry
	if newGrantedBitmap == 0 && newDeniedBitmap == 0 {
		return r.DeleteRolePermissionOverride(roomID, roleID)
	}

	if err == gocql.ErrNotFound {
		// Create new override
		override := models.RoomRolePermissions{
			RoomID:      roomID,
			RoleID:      roleID,
			Permissions: newGrantedBitmap,
			Denied:      newDeniedBitmap,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		insertQuery := models.RoomRolePermissionsTable.InsertBuilder().Query(*r.session)
		defer insertQuery.Release()

		return insertQuery.BindStruct(&override).Exec()
	} else {
		// Update existing override
		updateQuery := models.RoomRolePermissionsTable.UpdateBuilder().
			Set("permissions", "denied", "updated_at").
			Where(qb.Eq("room_id"), qb.Eq("role_id")).
			Query(*r.session)
		defer updateQuery.Release()

		return updateQuery.BindMap(qb.M{
			"room_id":     roomID,
			"role_id":     roleID,
			"permissions": newGrantedBitmap,
			"denied":      newDeniedBitmap,
			"updated_at":  now,
		}).Exec()
	}
}

// UpdateMemberPermissionOverride updates individual permission overrides for a member
func (r *RoomPermissionsRepository) UpdateMemberPermissionOverride(roomID, userID, permissionID string, override string) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	now := time.Now()
	permissionBit := int64(utils.PermissionsToBitfield([]string{permissionID}))

	// Check if override already exists
	var existing models.RoomMemberPermissions
	query := models.RoomMemberPermissionsTable.SelectBuilder().Where(qb.Eq("room_id"), qb.Eq("user_id")).Query(*r.session)
	defer query.Release()

	err := query.BindMap(qb.M{
		"room_id": roomID,
		"user_id": userID,
	}).Get(&existing)

	if err != nil && err != gocql.ErrNotFound {
		return fmt.Errorf("failed to check existing override: %v", err)
	}

	var newGrantedBitmap, newDeniedBitmap int64

	if err == gocql.ErrNotFound {
		// No existing override, start with empty bitmaps
		newGrantedBitmap = 0
		newDeniedBitmap = 0
	} else {
		// Start with existing bitmaps
		newGrantedBitmap = existing.Permissions
		newDeniedBitmap = existing.Denied
	}

	// Update bitmaps based on override type
	switch override {
	case "grant":
		// Add to granted, remove from denied
		newGrantedBitmap |= permissionBit
		newDeniedBitmap &= ^permissionBit
	case "deny":
		// Add to denied, remove from granted
		newDeniedBitmap |= permissionBit
		newGrantedBitmap &= ^permissionBit
	case "default":
		// Remove from both bitmaps
		newGrantedBitmap &= ^permissionBit
		newDeniedBitmap &= ^permissionBit
	}

	// If both bitmaps are now 0, delete the override entry
	if newGrantedBitmap == 0 && newDeniedBitmap == 0 {
		return r.DeleteMemberPermissionOverride(roomID, userID)
	}

	if err == gocql.ErrNotFound {
		// Create new override
		override := models.RoomMemberPermissions{
			RoomID:      roomID,
			UserID:      userID,
			Permissions: newGrantedBitmap,
			Denied:      newDeniedBitmap,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		insertQuery := models.RoomMemberPermissionsTable.InsertBuilder().Query(*r.session)
		defer insertQuery.Release()

		return insertQuery.BindStruct(&override).Exec()
	} else {
		// Update existing override
		updateQuery := models.RoomMemberPermissionsTable.UpdateBuilder().
			Set("permissions", "denied", "updated_at").
			Where(qb.Eq("room_id"), qb.Eq("user_id")).
			Query(*r.session)
		defer updateQuery.Release()

		return updateQuery.BindMap(qb.M{
			"room_id":     roomID,
			"user_id":     userID,
			"permissions": newGrantedBitmap,
			"denied":      newDeniedBitmap,
			"updated_at":  now,
		}).Exec()
	}
}

// SetMemberPermissionOverride sets permission overrides for a member in a specific room
func (r *RoomPermissionsRepository) SetMemberPermissionOverride(roomID, userID string, grantedPermissions, deniedPermissions []string) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	now := time.Now()
	grantedBitmap := int64(utils.PermissionsToBitfield(grantedPermissions))
	deniedBitmap := int64(utils.PermissionsToBitfield(deniedPermissions))

	// If both bitmaps are 0, it means all permissions are set to default
	// In this case, we should delete the override entry entirely
	if grantedBitmap == 0 && deniedBitmap == 0 {
		return r.DeleteMemberPermissionOverride(roomID, userID)
	}

	// Check if override already exists
	var existing models.RoomMemberPermissions
	query := models.RoomMemberPermissionsTable.SelectBuilder().Where(qb.Eq("room_id"), qb.Eq("user_id")).Query(*r.session)
	defer query.Release()

	err := query.BindMap(qb.M{
		"room_id": roomID,
		"user_id": userID,
	}).Get(&existing)

	if err != nil && err != gocql.ErrNotFound {
		return fmt.Errorf("failed to check existing override: %v", err)
	}

	if err == gocql.ErrNotFound {
		// Create new override
		override := models.RoomMemberPermissions{
			RoomID:      roomID,
			UserID:      userID,
			Permissions: grantedBitmap,
			Denied:      deniedBitmap,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		insertQuery := models.RoomMemberPermissionsTable.InsertBuilder().Query(*r.session)
		defer insertQuery.Release()

		return insertQuery.BindStruct(&override).Exec()
	} else {
		// Update existing override
		updateQuery := models.RoomMemberPermissionsTable.UpdateBuilder().
			Set("permissions", "denied", "updated_at").
			Where(qb.Eq("room_id"), qb.Eq("user_id")).
			Query(*r.session)
		defer updateQuery.Release()

		return updateQuery.BindMap(qb.M{
			"room_id":     roomID,
			"user_id":     userID,
			"permissions": grantedBitmap,
			"denied":      deniedBitmap,
			"updated_at":  now,
		}).Exec()
	}
}

// GetRolePermissionOverrides gets all role permission overrides for a room
func (r *RoomPermissionsRepository) GetRolePermissionOverrides(roomID string) ([]models.RoomRolePermissions, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	var overrides []models.RoomRolePermissions
	query := models.RoomRolePermissionsTable.SelectBuilder().Where(qb.Eq("room_id")).Query(*r.session)
	defer query.Release()

	err := query.BindMap(qb.M{"room_id": roomID}).Select(&overrides)
	if err != nil {
		return nil, fmt.Errorf("failed to get role overrides: %v", err)
	}

	return overrides, nil
}

// GetMemberPermissionOverrides gets all member permission overrides for a room
func (r *RoomPermissionsRepository) GetMemberPermissionOverrides(roomID string) ([]models.RoomMemberPermissions, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	var overrides []models.RoomMemberPermissions
	query := models.RoomMemberPermissionsTable.SelectBuilder().Where(qb.Eq("room_id")).Query(*r.session)
	defer query.Release()

	err := query.BindMap(qb.M{"room_id": roomID}).Select(&overrides)
	if err != nil {
		return nil, fmt.Errorf("failed to get member overrides: %v", err)
	}

	return overrides, nil
}

// DeleteRolePermissionOverride removes a role permission override
func (r *RoomPermissionsRepository) DeleteRolePermissionOverride(roomID, roleID string) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	query := models.RoomRolePermissionsTable.DeleteBuilder().Where(qb.Eq("room_id"), qb.Eq("role_id")).Query(*r.session)
	defer query.Release()

	return query.BindMap(qb.M{
		"room_id": roomID,
		"role_id": roleID,
	}).Exec()
}

// DeleteMemberPermissionOverride removes a member permission override
func (r *RoomPermissionsRepository) DeleteMemberPermissionOverride(roomID, userID string) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	query := models.RoomMemberPermissionsTable.DeleteBuilder().Where(qb.Eq("room_id"), qb.Eq("user_id")).Query(*r.session)
	defer query.Release()

	return query.BindMap(qb.M{
		"room_id": roomID,
		"user_id": userID,
	}).Exec()
}

// CalculateUserPermissionsInRoom calculates the final permissions for a user in a room
// This takes into account space permissions, role overrides, and member overrides
// Hierarchy: space owner > room overrides > space permissions
func (r *RoomPermissionsRepository) CalculateUserPermissionsInRoom(roomID, userID string, spaceID int64) ([]string, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	// First check if user is the space owner - space owners have all permissions
	userIDInt, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid user ID: %v", err)
	}

	isOwner, err := utils.CheckPermission(r.session, userIDInt, spaceID, utils.ADMINISTRATOR)
	if err == nil && isOwner {
		// Space owners have all permissions
		return []string{
			utils.MANAGE_SPACE,
			utils.MANAGE_CHANNELS,
			utils.MANAGE_ROLES,
			utils.KICK_MEMBERS,
			utils.BAN_MEMBERS,
			utils.VIEW_ROOMS,
			utils.SEND_MESSAGES,
			utils.MANAGE_MESSAGES,
			utils.READ_MESSAGE_HISTORY,
			utils.CONNECT,
			utils.SPEAK,
			utils.MUTE_MEMBERS,
			utils.DEAFEN_MEMBERS,
			utils.MOVE_MEMBERS,
			utils.ADMINISTRATOR,
		}, nil
	}

	// Start with space permissions
	spaceRolesRepo := NewSpaceRolesRepository(r.session)
	memberRolesRepo := NewSpaceMemberRolesRepository(*r.session)
	spacePermissions, err := spaceRolesRepo.CalculateMemberPermissions(spaceID, userID, memberRolesRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to get space permissions: %v", err)
	}

	// Convert to bitmap for easier manipulation
	permissionsBitmap := utils.PermissionsToBitfield(spacePermissions)

	// Get user's roles in the space (including @everyone)
	userRoles, err := spaceRolesRepo.GetMemberRoles(spaceID, userID, memberRolesRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to get user roles: %v", err)
	}

	// Apply role overrides
	roleOverrides, err := r.GetRolePermissionOverrides(roomID)
	if err != nil {
		return nil, fmt.Errorf("failed to get role overrides: %v", err)
	}

	for _, override := range roleOverrides {
		// Check if user has this role
		for _, userRole := range userRoles {
			if userRole.RoleID == override.RoleID {
				// Apply granted permissions
				permissionsBitmap |= utils.PermissionValue(override.Permissions)
				// Apply denied permissions (remove them)
				permissionsBitmap &^= utils.PermissionValue(override.Denied)
				break
			}
		}
	}

	// Apply member-specific overrides (highest priority)
	memberOverrides, err := r.GetMemberPermissionOverrides(roomID)
	if err != nil {
		return nil, fmt.Errorf("failed to get member overrides: %v", err)
	}

	for _, override := range memberOverrides {
		if override.UserID == userID {
			// Apply granted permissions
			permissionsBitmap |= utils.PermissionValue(override.Permissions)
			// Apply denied permissions (remove them)
			permissionsBitmap &^= utils.PermissionValue(override.Denied)
			break
		}
	}

	// Convert back to string array
	return utils.BitfieldToPermissions(permissionsBitmap), nil
}

// HasPermissionInRoom checks if a user has a specific permission in a room
func (r *RoomPermissionsRepository) HasPermissionInRoom(roomID, userID, permission string, spaceID int64) (bool, error) {
	permissions, err := r.CalculateUserPermissionsInRoom(roomID, userID, spaceID)
	if err != nil {
		return false, err
	}

	for _, perm := range permissions {
		if perm == permission {
			return true, nil
		}
	}

	return false, nil
}
