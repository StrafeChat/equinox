package repository

import (
	"fmt"
	"log"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v2"
	"github.com/scylladb/gocqlx/v2/qb"

	"github.com/StrafeChat/equinox/src/database/models"
)

type SpaceRolesRepository struct {
	session *gocqlx.Session
}

func NewSpaceRolesRepository(session *gocqlx.Session) *SpaceRolesRepository {
	return &SpaceRolesRepository{
		session: session,
	}
}

// CreateRole creates a new role in a space
func (r *SpaceRolesRepository) CreateRole(role models.SpaceRole) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	query := models.SpaceRoleTable.InsertBuilder().Query(*r.session).BindStruct(role)
	if err := query.ExecRelease(); err != nil {
		log.Printf("[CreateRole] Error creating role: %v", err)
		return err
	}

	log.Printf("[CreateRole] Created role %s in space %d", role.RoleID, role.SpaceID)
	return nil
}

// GetMaxPosition gets the highest position for roles in a space
func (r *SpaceRolesRepository) GetMaxPosition(spaceID int64) (int, error) {
	if r.session == nil {
		return 0, fmt.Errorf("database session not initialized")
	}

	var positions []int
	posQuery := qb.Select("space_roles").Columns("position").Where(qb.Eq("space_id")).Query(*r.session)
	if err := posQuery.Bind(spaceID).SelectRelease(&positions); err != nil {
		log.Printf("[GetMaxPosition] Error getting positions for space %d: %v", spaceID, err)
		return 0, err
	}

	if len(positions) == 0 {
		return 0, nil
	}

	maxPosition := positions[0]
	for _, pos := range positions {
		if pos > maxPosition {
			maxPosition = pos
		}
	}

	return maxPosition, nil
}

// GetSpaceRoles retrieves all roles for a space
func (r *SpaceRolesRepository) GetSpaceRoles(spaceID int64) ([]models.SpaceRole, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	var roles []models.SpaceRole
	query := qb.Select("space_roles").Where(qb.Eq("space_id")).Query(*r.session)
	if err := query.Bind(spaceID).SelectRelease(&roles); err != nil {
		log.Printf("[GetSpaceRoles] Error getting roles for space %d: %v", spaceID, err)
		return nil, err
	}

	return roles, nil
}

// GetRole retrieves a specific role
func (r *SpaceRolesRepository) GetRole(spaceID int64, roleID string) (*models.SpaceRole, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	var role models.SpaceRole
	query := qb.Select("space_roles").Where(qb.Eq("space_id"), qb.Eq("role_id")).Query(*r.session)
	if err := query.Bind(spaceID, roleID).GetRelease(&role); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		log.Printf("[GetRole] Error getting role %s in space %d: %v", roleID, spaceID, err)
		return nil, err
	}

	return &role, nil
}

// UpdateRole updates an existing role
func (r *SpaceRolesRepository) UpdateRole(spaceID int64, roleID string, updates map[string]interface{}) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	// Add updated_at timestamp
	updates["updated_at"] = time.Now()

	// Build update query with consistent field and value ordering
	updateBuilder := qb.Update("space_roles")
	var fields []string
	var values []interface{}

	// Collect fields and values in the same order
	for field, value := range updates {
		fields = append(fields, field)
		values = append(values, value)
		updateBuilder = updateBuilder.Set(field)
	}
	updateBuilder = updateBuilder.Where(qb.Eq("space_id"), qb.Eq("role_id"))

	// Add WHERE clause values
	values = append(values, spaceID, roleID)

	query := updateBuilder.Query(*r.session)
	if err := query.Bind(values...).ExecRelease(); err != nil {
		log.Printf("[UpdateRole] Error updating role %s in space %d: %v", roleID, spaceID, err)
		return err
	}

	log.Printf("[UpdateRole] Updated role %s in space %d", roleID, spaceID)
	return nil
}

// DeleteRole deletes a role from a space and removes it from all members
func (r *SpaceRolesRepository) DeleteRole(spaceID int64, roleID string, memberRolesRepo *SpaceMemberRolesRepository) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	// Don't allow deletion of @everyone role
	if roleID == "@everyone" {
		return fmt.Errorf("cannot delete @everyone role")
	}

	// First remove the role from all members using the new junction table
	if err := memberRolesRepo.RemoveRoleFromAllMembers(nil, spaceID, roleID); err != nil {
		log.Printf("[DeleteRole] Error removing role %s from all members in space %d: %v", roleID, spaceID, err)
		return err
	}

	// Then delete the role itself
	query := qb.Delete("space_roles").Where(qb.Eq("space_id"), qb.Eq("role_id")).Query(*r.session)
	if err := query.Bind(spaceID, roleID).ExecRelease(); err != nil {
		log.Printf("[DeleteRole] Error deleting role %s in space %d: %v", roleID, spaceID, err)
		return err
	}

	log.Printf("[DeleteRole] Deleted role %s from space %d and removed from all members", roleID, spaceID)
	return nil
}

// getDefaultEveryonePermissions returns the default permissions for @everyone role
func getDefaultEveryonePermissions() []string {
	return []string{
		"VIEW_CHANNELS",
		"SEND_MESSAGES",
		"READ_MESSAGE_HISTORY",
		"ADD_REACTIONS",
		"ATTACH_FILES",
		"EMBED_LINKS",
		"CONNECT",
		"SPEAK",
		"USE_VOICE_ACTIVATION",
	}
}

// hasPermission checks if a user has a specific permission
func hasPermission(userPermissions []string, permission string) bool {
	// Administrator has all permissions
	for _, perm := range userPermissions {
		if perm == "ADMINISTRATOR" {
			return true
		}
	}

	// Check for specific permission
	for _, perm := range userPermissions {
		if perm == permission {
			return true
		}
	}

	return false
}

// calculatePermissions calculates the final permissions for a user based on their roles
func calculatePermissions(rolePermissions [][]string) []string {
	permissionSet := make(map[string]bool)

	// Combine all permissions from all roles
	for _, rolePerms := range rolePermissions {
		for _, perm := range rolePerms {
			permissionSet[perm] = true
		}
	}

	// Convert map to slice
	permissions := make([]string, 0, len(permissionSet))
	for perm := range permissionSet {
		permissions = append(permissions, perm)
	}

	return permissions
}

// CreateDefaultEveryoneRole creates the default @everyone role for a space
func (r *SpaceRolesRepository) CreateDefaultEveryoneRole(spaceID int64) error {
	defaultPermissions := getDefaultEveryonePermissions()
	color := "#99aab5"
	now := time.Now()
	role := models.SpaceRole{
		SpaceID:     spaceID,
		RoleID:      "@everyone",
		Name:        "@everyone",
		Color:       &color,
		Permissions: defaultPermissions,
		Position:    0,     // Lowest position
		Mentionable: false, // Not mentionable
		Hoist:       false, // Not hoisted
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return r.CreateRole(role)
}

// GetMemberRoles retrieves all roles for a specific member using the new role system
// This method now works with SpaceMemberRolesRepository
func (r *SpaceRolesRepository) GetMemberRoles(spaceID int64, userID string, memberRolesRepo *SpaceMemberRolesRepository) ([]models.SpaceRole, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	// Get member's role assignments from the new junction table
	memberRoleAssignments, err := memberRolesRepo.GetMemberRoles(nil, spaceID, userID)
	if err != nil {
		return nil, err
	}

	// Always include @everyone role
	roleIDs := []string{"@everyone"}
	for _, assignment := range memberRoleAssignments {
		roleIDs = append(roleIDs, assignment.RoleID)
	}

	// Get all roles for the space
	allRoles, err := r.GetSpaceRoles(spaceID)
	if err != nil {
		return nil, err
	}

	// Filter roles that the member has
	var memberRoles []models.SpaceRole
	for _, role := range allRoles {
		for _, roleID := range roleIDs {
			if role.RoleID == roleID {
				memberRoles = append(memberRoles, role)
				break
			}
		}
	}

	return memberRoles, nil
}

// CalculateMemberPermissions calculates the final permissions for a member using the new role system
func (r *SpaceRolesRepository) CalculateMemberPermissions(spaceID int64, userID string, memberRolesRepo *SpaceMemberRolesRepository) ([]string, error) {
	memberRoles, err := r.GetMemberRoles(spaceID, userID, memberRolesRepo)
	if err != nil {
		return nil, err
	}

	// Extract permissions from all roles
	var rolePermissions [][]string
	for _, role := range memberRoles {
		rolePermissions = append(rolePermissions, role.Permissions)
	}

	// Calculate combined permissions
	return calculatePermissions(rolePermissions), nil
}

// HasPermission checks if a member has a specific permission using the new role system
func (r *SpaceRolesRepository) HasPermission(spaceID int64, userID, permission string, memberRolesRepo *SpaceMemberRolesRepository) (bool, error) {
	permissions, err := r.CalculateMemberPermissions(spaceID, userID, memberRolesRepo)
	if err != nil {
		return false, err
	}

	return hasPermission(permissions, permission), nil
}

// Note: Role assignment methods have been moved to SpaceMemberRolesRepository
// Use SpaceMemberRolesRepository.AddRoleToMember() and SpaceMemberRolesRepository.RemoveRoleFromMember()
