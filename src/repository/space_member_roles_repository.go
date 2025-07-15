package repository

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type SpaceMemberRolesRepository struct {
	session gocqlx.Session
}

func NewSpaceMemberRolesRepository(session gocqlx.Session) *SpaceMemberRolesRepository {
	return &SpaceMemberRolesRepository{
		session: session,
	}
}

// AddRoleToMember assigns a role to a space member
func (r *SpaceMemberRolesRepository) AddRoleToMember(ctx context.Context, spaceID int64, userID, roleID, assignedBy string) error {
	now := time.Now()

	// Insert into main table
	memberRole := &models.SpaceMemberRole{
		SpaceID:    spaceID,
		UserID:     userID,
		RoleID:     roleID,
		AssignedAt: now,
		AssignedBy: assignedBy,
	}

	if err := models.SpaceMemberRoleTable.InsertQuery(r.session).BindStruct(memberRole).ExecRelease(); err != nil {
		return err
	}

	// Insert into by_role table for efficient role-based queries
	memberRoleByRole := &models.SpaceMemberRolesByRole{
		SpaceID:    spaceID,
		RoleID:     roleID,
		UserID:     userID,
		AssignedAt: now,
		AssignedBy: assignedBy,
	}

	return models.SpaceMemberRolesByRoleTable.InsertQuery(r.session).BindStruct(memberRoleByRole).ExecRelease()
}

// RemoveRoleFromMember removes a specific role from a space member
func (r *SpaceMemberRolesRepository) RemoveRoleFromMember(ctx context.Context, spaceID int64, userID, roleID string) error {
	// Delete from main table
	query := qb.Delete("space_member_roles").
		Where(qb.Eq("space_id"), qb.Eq("user_id"), qb.Eq("role_id")).
		Query(r.session)
	if err := query.Bind(spaceID, userID, roleID).ExecRelease(); err != nil {
		return err
	}

	// Delete from by_role table
	query2 := qb.Delete("space_member_roles_by_role").
		Where(qb.Eq("space_id"), qb.Eq("role_id"), qb.Eq("user_id")).
		Query(r.session)
	return query2.Bind(spaceID, roleID, userID).ExecRelease()
}

// RemoveAllRolesFromMember removes all roles from a space member
func (r *SpaceMemberRolesRepository) RemoveAllRolesFromMember(ctx context.Context, spaceID int64, userID string) error {
	// First get all roles for this member
	roles, err := r.GetMemberRoles(ctx, spaceID, userID)
	if err != nil {
		return err
	}

	// Delete each role
	for _, role := range roles {
		if err := r.RemoveRoleFromMember(ctx, spaceID, userID, role.RoleID); err != nil {
			return err
		}
	}

	return nil
}

// RemoveRoleFromAllMembers removes a role from all members in a space (used when deleting a role)
func (r *SpaceMemberRolesRepository) RemoveRoleFromAllMembers(ctx context.Context, spaceID int64, roleID string) error {
	// Get all members with this role
	members, err := r.GetMembersWithRole(ctx, spaceID, roleID)
	if err != nil {
		return err
	}

	// Delete each assignment
	for _, member := range members {
		if err := r.RemoveRoleFromMember(ctx, spaceID, member.UserID, roleID); err != nil {
			return err
		}
	}

	return nil
}

// GetMemberRoles returns all roles assigned to a specific member
func (r *SpaceMemberRolesRepository) GetMemberRoles(ctx context.Context, spaceID int64, userID string) ([]models.SpaceMemberRole, error) {
	var roles []models.SpaceMemberRole

	query := qb.Select("space_member_roles").Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(r.session)
	if err := query.Bind(spaceID, userID).SelectRelease(&roles); err != nil {
		return nil, err
	}

	return roles, nil
}

// GetMembersWithRole returns all members that have a specific role
func (r *SpaceMemberRolesRepository) GetMembersWithRole(ctx context.Context, spaceID int64, roleID string) ([]models.SpaceMemberRolesByRole, error) {
	var members []models.SpaceMemberRolesByRole

	query := qb.Select("space_member_roles_by_role").Where(qb.Eq("space_id"), qb.Eq("role_id")).Query(r.session)
	if err := query.Bind(spaceID, roleID).SelectRelease(&members); err != nil {
		return nil, err
	}

	return members, nil
}

// SetMemberRoles replaces all roles for a member with the provided list
func (r *SpaceMemberRolesRepository) SetMemberRoles(ctx context.Context, spaceID int64, userID string, roleIDs []string, assignedBy string) error {
	// First remove all existing roles
	if err := r.RemoveAllRolesFromMember(ctx, spaceID, userID); err != nil {
		return err
	}

	// Then add the new roles
	for _, roleID := range roleIDs {
		if err := r.AddRoleToMember(ctx, spaceID, userID, roleID, assignedBy); err != nil {
			return err
		}
	}

	return nil
}

// GetAllSpaceMemberRoles returns all role assignments for a space
func (r *SpaceMemberRolesRepository) GetAllSpaceMemberRoles(ctx context.Context, spaceID int64) (map[string][]string, error) {
	var allRoles []models.SpaceMemberRole

	query := qb.Select("space_member_roles").Where(qb.Eq("space_id")).Query(r.session)
	if err := query.Bind(spaceID).SelectRelease(&allRoles); err != nil {
		return nil, err
	}

	// Group roles by user ID
	memberRoles := make(map[string][]string)
	for _, role := range allRoles {
		memberRoles[role.UserID] = append(memberRoles[role.UserID], role.RoleID)
	}

	return memberRoles, nil
}
