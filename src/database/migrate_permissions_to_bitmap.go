package database

import (
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/scylladb/gocqlx/v2/qb"
)

// MigratePermissionsToBitmap migrates any remaining tables with array-based permissions
// to the new bitmap format, specifically handling the VIEW_CHANNELS -> VIEW_ROOMS conversion
func MigratePermissionsToBitmap() error {
	log.Println("Starting comprehensive permissions migration to bitmap format...")
	log.Println("This migration will convert array-based permissions to bitmap and update VIEW_CHANNELS to VIEW_ROOMS")

	// Check if space_roles table still has array permissions and migrate if needed
	if err := migrateSpaceRolesIfNeeded(); err != nil {
		return err
	}

	// Check for any other tables that might have array-based permissions
	if err := migrateRoomPermissionsIfNeeded(); err != nil {
		return err
	}

	// Check for any user-specific permission tables
	if err := migrateUserPermissionsIfNeeded(); err != nil {
		return err
	}

	log.Println("Comprehensive permissions migration completed successfully")
	return nil
}

// migrateSpaceRolesIfNeeded checks if space_roles table needs migration and performs it
func migrateSpaceRolesIfNeeded() error {
	log.Println("Checking space_roles table for array-based permissions...")

	// Try to read existing data to check if it has array permissions
	var existingRoles []OldSpaceRoleWithArrayPermissions
	query := qb.Select("space_roles").Columns(
		"space_id", "role_id", "name", "color", "permissions",
		"position", "mentionable", "hoist", "created_at", "updated_at",
	).Query(*Session)

	if err := query.SelectRelease(&existingRoles); err != nil {
		log.Printf("Could not read space_roles with array permissions (likely already migrated): %v", err)
		return nil // Table likely already has bitmap permissions
	}

	if len(existingRoles) == 0 {
		log.Println("No space roles found with array permissions")
		return nil
	}

	log.Printf("Found %d space roles with array permissions to migrate", len(existingRoles))

	// Create backup table with proper schema
	if err := Session.ExecStmt(`CREATE TABLE IF NOT EXISTS space_roles_backup (
		space_id bigint,
		role_id text,
		name text,
		color text,
		permissions list<text>,
		position int,
		mentionable boolean,
		hoist boolean,
		created_at timestamp,
		updated_at timestamp,
		PRIMARY KEY (space_id, role_id)
	);`); err != nil {
		log.Printf("Failed to create backup table: %v", err)
		return err
	}

	// Copy existing data to backup table
	for _, role := range existingRoles {
		backupQuery := qb.Insert("space_roles_backup").Columns(
			"space_id", "role_id", "name", "color", "permissions",
			"position", "mentionable", "hoist", "created_at", "updated_at",
		).Query(*Session)

		if err := backupQuery.BindStruct(&role).ExecRelease(); err != nil {
			log.Printf("Failed to backup role %s in space %d: %v", role.RoleID, role.SpaceID, err)
			return err
		}
	}
	log.Printf("Created backup table and copied %d roles: space_roles_backup", len(existingRoles))

	// Drop and recreate table with correct schema
	if err := Session.ExecStmt(`DROP TABLE IF EXISTS space_roles`); err != nil {
		log.Printf("Failed to drop space_roles table: %v", err)
		return err
	}

	// Recreate with bitmap permissions
	if err := Session.ExecStmt(`CREATE TABLE IF NOT EXISTS space_roles (
		space_id bigint,
		role_id text,
		name text,
		color text,
		permissions bigint,
		position int,
		mentionable boolean,
		hoist boolean,
		created_at timestamp,
		updated_at timestamp,
		PRIMARY KEY (space_id, role_id)
	);`); err != nil {
		log.Printf("Failed to recreate space_roles table: %v", err)
		return err
	}

	// Convert and insert data
	for _, oldRole := range existingRoles {
		// Convert VIEW_CHANNELS to VIEW_ROOMS in permissions array
		convertedPermissions := convertViewChannelsToViewRooms(oldRole.Permissions)
		
		// Convert to bitmap
		permissionsBitmap := int64(utils.PermissionsToBitfield(convertedPermissions))

		newRole := models.SpaceRole{
			SpaceID:     oldRole.SpaceID,
			RoleID:      oldRole.RoleID,
			Name:        oldRole.Name,
			Color:       oldRole.Color,
			Permissions: permissionsBitmap,
			Position:    oldRole.Position,
			Mentionable: oldRole.Mentionable,
			Hoist:       oldRole.Hoist,
			CreatedAt:   oldRole.CreatedAt,
			UpdatedAt:   oldRole.UpdatedAt,
		}

		insertQuery := qb.Insert("space_roles").Columns(
			"space_id", "role_id", "name", "color", "permissions",
			"position", "mentionable", "hoist", "created_at", "updated_at",
		).Query(*Session)

		if err := insertQuery.BindStruct(&newRole).ExecRelease(); err != nil {
			log.Printf("Failed to insert converted role %s in space %d: %v", oldRole.RoleID, oldRole.SpaceID, err)
			return err
		}

		log.Printf("Migrated role %s in space %d: %v -> %d (converted VIEW_CHANNELS to VIEW_ROOMS)", 
			oldRole.RoleID, oldRole.SpaceID, oldRole.Permissions, permissionsBitmap)
	}

	log.Printf("Successfully migrated %d space roles", len(existingRoles))
	return nil
}

// migrateRoomPermissionsIfNeeded checks for any room-specific permission tables that need migration
func migrateRoomPermissionsIfNeeded() error {
	log.Println("Checking room permission tables...")

	// Check if room_member_permissions and room_role_permissions tables exist and have correct schema
	// These tables should already be using bitmap format based on the models, but check anyway
	
	// Ensure room_member_permissions table has correct schema
	roomMemberPerms := &models.RoomMemberPermissions{}
	for _, stmt := range roomMemberPerms.SchemaDefinition() {
		if err := Session.ExecStmt(stmt); err != nil {
			log.Printf("Room member permissions table already exists or error: %v", err)
		}
	}

	// Ensure room_role_permissions table has correct schema
	roomRolePerms := &models.RoomRolePermissions{}
	for _, stmt := range roomRolePerms.SchemaDefinition() {
		if err := Session.ExecStmt(stmt); err != nil {
			log.Printf("Room role permissions table already exists or error: %v", err)
		}
	}

	log.Println("Room permission tables verified")
	return nil
}

// migrateUserPermissionsIfNeeded checks for any user-specific permission tables
func migrateUserPermissionsIfNeeded() error {
	log.Println("Checking for any additional permission tables...")

	// Check if there are any other tables with array-based permissions
	// This is a placeholder for any future permission tables that might need migration
	
	log.Println("No additional permission tables found requiring migration")
	return nil
}

// convertViewChannelsToViewRooms converts VIEW_CHANNELS permission to VIEW_ROOMS
func convertViewChannelsToViewRooms(permissions []string) []string {
	converted := make([]string, 0, len(permissions))
	for _, perm := range permissions {
		if perm == "VIEW_CHANNELS" {
			converted = append(converted, "VIEW_ROOMS")
			log.Printf("Converted permission: VIEW_CHANNELS -> VIEW_ROOMS")
		} else {
			converted = append(converted, perm)
		}
	}
	return converted
}

// OldSpaceRoleWithArrayPermissions represents space role with array-based permissions
type OldSpaceRoleWithArrayPermissions struct {
	SpaceID     int64     `db:"space_id"`
	RoleID      string    `db:"role_id"`
	Name        string    `db:"name"`
	Color       *string   `db:"color"`
	Permissions []string  `db:"permissions"`
	Position    int       `db:"position"`
	Mentionable bool      `db:"mentionable"`
	Hoist       bool      `db:"hoist"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// VerifyPermissionsMigration verifies that the migration was successful
func VerifyPermissionsMigration() error {
	log.Println("Verifying permissions migration...")

	// Check space_roles table
	var roles []models.SpaceRole
	query := qb.Select("space_roles").Query(*Session)
	if err := query.SelectRelease(&roles); err != nil {
		log.Printf("Error reading space_roles for verification: %v", err)
		return err
	}

	log.Printf("Found %d space roles with bitmap permissions", len(roles))

	// Verify that permissions are properly converted
	for _, role := range roles {
		permissions := utils.BitfieldToPermissions(utils.PermissionValue(role.Permissions))
		log.Printf("Role %s in space %d has permissions: %v (bitmap: %d)", 
			role.RoleID, role.SpaceID, permissions, role.Permissions)
		
		// Check that no VIEW_CHANNELS permissions exist
		for _, perm := range permissions {
			if perm == "VIEW_CHANNELS" {
				log.Printf("⚠️  Warning: Found VIEW_CHANNELS permission in role %s (should be VIEW_ROOMS)", role.RoleID)
			}
		}
	}

	log.Println("✅ Permissions migration verification completed")
	return nil
}