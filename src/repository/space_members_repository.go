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

type SpaceMembersRepository struct {
	session *gocqlx.Session
}

func NewSpaceMembersRepository(session *gocqlx.Session) *SpaceMembersRepository {
	return &SpaceMembersRepository{
		session: session,
	}
}

func (r *SpaceMembersRepository) GetSpaceMembers(spaceID int64) ([]models.SpaceMember, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	var members []models.SpaceMember
	query := qb.Select("space_members").Where(qb.Eq("space_id")).Query(*r.session)
	if err := query.Bind(spaceID).SelectRelease(&members); err != nil {
		log.Printf("[GetSpaceMembers] Error getting members for space %d: %v", spaceID, err)
		return nil, err
	}

	return members, nil
}

func (r *SpaceMembersRepository) GetSpaceMember(spaceID int64, userID string) (*models.SpaceMember, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	var member models.SpaceMember
	query := qb.Select("space_members").Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(*r.session)
	if err := query.Bind(spaceID, userID).GetRelease(&member); err != nil {
		if err == gocql.ErrNotFound {
			return nil, fmt.Errorf("member not found")
		}
		log.Printf("[GetSpaceMember] Error getting member %s in space %d: %v", userID, spaceID, err)
		return nil, err
	}

	return &member, nil
}

func (r *SpaceMembersRepository) IsMember(spaceID int64, userID string) (bool, error) {
	if r.session == nil {
		return false, fmt.Errorf("database session not initialized")
	}

	// Try to get the member directly - more efficient than COUNT
	var member models.SpaceMember
	query := models.SpaceMemberTable.SelectBuilder().
		Columns("space_id"). // Only select one column for efficiency
		Where(qb.Eq("space_id"), qb.Eq("user_id")).
		Query(*r.session)
	
	err := query.BindMap(qb.M{"space_id": spaceID, "user_id": userID}).GetRelease(&member)
	if err != nil {
		if err == gocql.ErrNotFound {
			return false, nil
		}
		log.Printf("[IsMember] Error checking membership for user %s in space %d: %v", userID, spaceID, err)
		return false, err
	}

	return true, nil
}

func (r *SpaceMembersRepository) AddMember(spaceID int64, userID string) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	// Create member record
	member := models.SpaceMember{
		SpaceID:  spaceID,
		UserID:   userID,
		JoinedAt: time.Now(),
		Deaf:     false,
		Mute:     false,
		Flags:    0,
		Pending:  false,
	}

	// Insert into space_members table
	query := models.SpaceMemberTable.InsertBuilder().Query(*r.session).BindStruct(member)
	if err := query.ExecRelease(); err != nil {
		log.Printf("[AddMember] Error adding member %s to space %d: %v", userID, spaceID, err)
		return err
	}

	// Insert into space_members_by_user table for indexing
	memberByUser := models.SpaceMembersByUser{
		UserID:   userID,
		SpaceID:  spaceID,
		JoinedAt: member.JoinedAt,
	}

	query2 := models.SpaceMembersByUserTable.InsertBuilder().Query(*r.session).BindStruct(memberByUser)
	if err := query2.ExecRelease(); err != nil {
		log.Printf("[AddMember] Error adding member %s to space_members_by_user for space %d: %v", userID, spaceID, err)
		// Try to rollback the first insert
		r.RemoveMember(spaceID, userID)
		return err
	}

	return nil
}

func (r *SpaceMembersRepository) RemoveMember(spaceID int64, userID string) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	// Remove from space_members table
	query := models.SpaceMemberTable.DeleteBuilder().Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(*r.session)
	if err := query.Bind(spaceID, userID).ExecRelease(); err != nil {
		log.Printf("[RemoveMember] Error removing member %s from space %d: %v", userID, spaceID, err)
		return err
	}

	// Remove from space_members_by_user table
	query2 := models.SpaceMembersByUserTable.DeleteBuilder().Where(qb.Eq("user_id"), qb.Eq("space_id")).Query(*r.session)
	if err := query2.Bind(userID, spaceID).ExecRelease(); err != nil {
		log.Printf("[RemoveMember] Error removing member %s from space_members_by_user for space %d: %v", userID, spaceID, err)
		// Continue anyway since the main record was deleted
	}

	return nil
}

func (r *SpaceMembersRepository) UpdateMember(spaceID int64, userID string, updates map[string]interface{}) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	if len(updates) == 0 {
		return fmt.Errorf("no updates provided")
	}

	// Use gocqlx UpdateBuilder for proper query construction
	updateBuilder := models.SpaceMemberTable.UpdateBuilder().
		Where(qb.Eq("space_id"), qb.Eq("user_id"))
	
	// Add SET clauses for each update field
	for field, value := range updates {
		updateBuilder = updateBuilder.Set(field)
		updates[field] = value // Ensure the value is in the updates map for binding
	}
	
	// Add WHERE clause values to the updates map
	updates["space_id"] = spaceID
	updates["user_id"] = userID
	
	query := updateBuilder.Query(*r.session)
	if err := query.BindMap(updates).ExecRelease(); err != nil {
		log.Printf("[UpdateMember] Error updating member %s in space %d: %v", userID, spaceID, err)
		return err
	}

	return nil
}

// Note: Role management has been moved to SpaceMemberRolesRepository
// Use SpaceMemberRolesRepository for all role-related operations
