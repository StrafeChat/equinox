package database

import (
	"log"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/repository"
	"github.com/scylladb/gocqlx/v2/qb"
)

// SpaceMemberWithRoles represents the old SpaceMember structure with roles field
// This is used only for migration purposes
type SpaceMemberWithRoles struct {
	SpaceID int64    `db:"space_id"`
	UserID  string   `db:"user_id"`
	Roles   []string `db:"roles"`
}

// MigrateMemberRolesToJunctionTable migrates existing member roles from the space_members table
// to the new space_member_roles junction table system
func MigrateMemberRolesToJunctionTable() error {
	log.Println("Starting migration of member roles to junction table...")

	// First, create the new tables if they don't exist
	spaceMemberRole := &models.SpaceMemberRole{}
	spaceMemberRolesByRole := &models.SpaceMemberRolesByRole{}

	for _, stmt := range spaceMemberRole.SchemaDefinition() {
		if err := Session.ExecStmt(stmt); err != nil {
			log.Printf("Error creating space_member_roles table: %v", err)
			return err
		}
	}

	for _, stmt := range spaceMemberRolesByRole.SchemaDefinition() {
		if err := Session.ExecStmt(stmt); err != nil {
			log.Printf("Error creating space_member_roles_by_role table: %v", err)
			return err
		}
	}

	log.Println("Created new junction tables")

	// Get all space members with roles using the old structure
	var members []SpaceMemberWithRoles
	query := qb.Select("space_members").Columns("space_id", "user_id", "roles").Query(*Session)
	if err := query.SelectRelease(&members); err != nil {
		log.Printf("Error fetching space members: %v", err)
		return err
	}

	log.Printf("Found %d space members to migrate", len(members))

	// Initialize the repository for role management
	memberRolesRepo := repository.NewSpaceMemberRolesRepository(*Session)

	migrationCount := 0
	for _, member := range members {
		// Skip members with no roles (this field might not exist in old schema)
		if len(member.Roles) == 0 {
			continue
		}

		// Migrate each role for this member
		for _, roleID := range member.Roles {
			if err := memberRolesRepo.AddRoleToMember(nil, member.SpaceID, member.UserID, roleID, "system_migration"); err != nil {
				log.Printf("Error migrating role %s for member %s in space %d: %v", roleID, member.UserID, member.SpaceID, err)
				// Continue with other roles instead of failing completely
				continue
			}
			migrationCount++
		}

		log.Printf("Migrated %d roles for member %s in space %d", len(member.Roles), member.UserID, member.SpaceID)
	}

	log.Printf("Successfully migrated %d role assignments to junction table", migrationCount)

	// Note: We don't automatically drop the roles column from space_members
	// This should be done manually after verifying the migration was successful
	log.Println("IMPORTANT: After verifying the migration was successful, you should manually run:")
	log.Println("ALTER TABLE space_members DROP roles;")
	log.Println("This will remove the old roles column from the space_members table.")

	return nil
}

// VerifyMemberRolesMigration checks that the migration was successful by comparing
// role counts between the old and new systems
func VerifyMemberRolesMigration() error {
	log.Println("Verifying member roles migration...")

	// Count roles in old system (space_members table)
	var members []SpaceMemberWithRoles
	query := qb.Select("space_members").Columns("space_id", "user_id", "roles").Query(*Session)
	if err := query.SelectRelease(&members); err != nil {
		log.Printf("Error fetching space members for verification: %v", err)
		return err
	}

	oldRoleCount := 0
	for _, member := range members {
		oldRoleCount += len(member.Roles)
	}

	// Count roles in new system (space_member_roles table)
	var newRoles []models.SpaceMemberRole
	newQuery := qb.Select("space_member_roles").Query(*Session)
	if err := newQuery.SelectRelease(&newRoles); err != nil {
		log.Printf("Error fetching new role assignments for verification: %v", err)
		return err
	}

	newRoleCount := len(newRoles)

	log.Printf("Old system role count: %d", oldRoleCount)
	log.Printf("New system role count: %d", newRoleCount)

	if oldRoleCount == newRoleCount {
		log.Println("✅ Migration verification successful! Role counts match.")
	} else {
		log.Printf("⚠️  Migration verification warning: Role counts don't match. Old: %d, New: %d", oldRoleCount, newRoleCount)
		log.Println("Please review the migration logs and check for any errors.")
	}

	return nil
}