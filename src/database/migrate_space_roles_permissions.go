package database

import (
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/scylladb/gocqlx/v2/qb"
)

// MigrateSpaceRolesPermissionsSchema performs a complete migration of space roles permissions
// from string arrays to bitmaps, preserving all existing data
func MigrateSpaceRolesPermissionsSchema() error {
	log.Println("Starting complete space roles permissions migration...")
	log.Println("This will preserve all existing data while converting permissions to bitmap format.")

	// Step 1: Read all existing data before any schema changes
	log.Println("Step 1: Reading existing space roles data...")
	var existingRoles []OldSpaceRole
	query := qb.Select("space_roles").Columns(
		"space_id", "role_id", "name", "color", "permissions",
		"position", "mentionable", "hoist", "created_at", "updated_at",
	).Query(*Session)

	if err := query.SelectRelease(&existingRoles); err != nil {
		log.Printf("Failed to read existing space roles: %v", err)
		// If table doesn't exist or has no data, continue with schema creation
		log.Println("No existing data found, proceeding with fresh schema creation...")
		existingRoles = []OldSpaceRole{}
	}

	log.Printf("Found %d existing space roles to preserve", len(existingRoles))

	// Step 2: Drop and recreate the table with new schema
	log.Println("Step 2: Updating table schema...")
	if err := Session.ExecStmt(`DROP TABLE IF EXISTS space_roles`); err != nil {
		log.Printf("Failed to drop space_roles table: %v", err)
		return err
	}
	log.Println("Dropped existing space_roles table")

	// Recreate the space_roles table with the correct schema
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
	log.Println("Recreated space_roles table with bigint permissions column")

	// Step 3: Convert and insert the preserved data
	if len(existingRoles) > 0 {
		log.Println("Step 3: Converting and restoring existing data...")
		for _, oldRole := range existingRoles {
			// Convert string permissions to bitmap
			permissionsBitmap := int64(utils.PermissionsToBitfield(oldRole.Permissions))

			// Create new role with bitmap permissions
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

			// Insert the converted role
			insertQuery := qb.Insert("space_roles").Columns(
				"space_id", "role_id", "name", "color", "permissions",
				"position", "mentionable", "hoist", "created_at", "updated_at",
			).Query(*Session)

			if err := insertQuery.BindStruct(&newRole).ExecRelease(); err != nil {
				log.Printf("Failed to insert converted role %s in space %d: %v", oldRole.RoleID, oldRole.SpaceID, err)
				return err
			}

			log.Printf("Migrated role %s in space %d: %v -> %d", oldRole.RoleID, oldRole.SpaceID, oldRole.Permissions, permissionsBitmap)
		}
		log.Printf("Successfully migrated %d space roles with preserved data", len(existingRoles))
	} else {
		log.Println("No existing data to migrate")
	}

	log.Println("Space roles permissions migration completed successfully")
	return nil
}

// MigrateSpaceRolesPermissionsSafe attempts to alter the existing table instead of dropping it
// This preserves data but may fail if Cassandra doesn't support the column type change
func MigrateSpaceRolesPermissionsSafe() error {
	log.Println("Attempting safe migration of space_roles permissions column...")

	// Try to add a new permissions_new column with bigint type
	if err := Session.ExecStmt(`ALTER TABLE space_roles ADD permissions_new bigint`); err != nil {
		log.Printf("Failed to add permissions_new column: %v", err)
		return err
	}
	log.Println("Added permissions_new bigint column")

	// Note: Data migration would need to be done manually here
	// You would need to:
	// 1. Read all existing roles
	// 2. Convert their permissions from []string to int64 bitmap
	// 3. Update the permissions_new column
	// 4. Drop the old permissions column
	// 5. Rename permissions_new to permissions

	log.Println("Safe migration setup completed. Manual data migration required.")
	log.Println("Please run data migration script to convert permissions from string array to bitmap.")
	return nil
}

// OldSpaceRole represents the space role structure before migration
// This struct is used to read data from the old schema with string array permissions
type OldSpaceRole struct {
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

// MigrateSpaceRolesPermissionsData is now deprecated - data migration is handled in MigrateSpaceRolesPermissionsSchema
// This function is kept for backward compatibility but does nothing
func MigrateSpaceRolesPermissionsData() error {
	log.Println("Data migration is now handled automatically in schema migration - skipping separate data migration")
	return nil
}
