package repository

import (
	"fmt"
	"log"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v2"
	"github.com/scylladb/gocqlx/v2/qb"

	"github.com/StrafeChat/equinox/src/database/models"
)

type SpaceRepository struct {
	session *gocqlx.Session
}

func NewSpaceRepository(session *gocqlx.Session) *SpaceRepository {
	return &SpaceRepository{
		session: session,
	}
}

func (r *SpaceRepository) GetSpace(spaceID int64) (*models.Space, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	var space models.Space
	query := qb.Select("spaces").Where(qb.Eq("id")).Query(*r.session)
	if err := query.Bind(spaceID).GetRelease(&space); err != nil {
		if err == gocql.ErrNotFound {
			return nil, fmt.Errorf("space not found")
		}
		log.Printf("[GetSpace] Error getting space %d: %v", spaceID, err)
		return nil, err
	}

	return &space, nil
}

func (r *SpaceRepository) CreateSpace(space *models.Space) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	query := models.SpaceTable.InsertBuilder().Query(*r.session).BindStruct(space)
	if err := query.ExecRelease(); err != nil {
		log.Printf("[CreateSpace] Error creating space: %v", err)
		return err
	}

	return nil
}

func (r *SpaceRepository) UpdateSpace(spaceID int64, updates map[string]interface{}) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	if len(updates) == 0 {
		return fmt.Errorf("no updates provided")
	}

	// Build update query - don't add extra WHERE clause as UpdateBuilder already handles primary key
	updateBuilder := models.SpaceTable.UpdateBuilder()
	
	// Collect fields and values in deterministic order
	var setValues []interface{}
	for field, value := range updates {
		updateBuilder = updateBuilder.Set(field)
		setValues = append(setValues, value)
	}
	
	// Build the query
	query := updateBuilder.Query(*r.session)
	
	// Debug logging
	log.Printf("[UpdateSpace] Query: %s", query.String())
	log.Printf("[UpdateSpace] SET values count: %d, values: %+v", len(setValues), setValues)
	log.Printf("[UpdateSpace] WHERE value (spaceID): %d", spaceID)
	
	// Bind SET values first, then WHERE value (primary key)
	allValues := append(setValues, spaceID)
	log.Printf("[UpdateSpace] All values count: %d, values: %+v", len(allValues), allValues)
	
	if err := query.Bind(allValues...).ExecRelease(); err != nil {
		log.Printf("[UpdateSpace] Error updating space %d: %v", spaceID, err)
		return err
	}

	return nil
}

func (r *SpaceRepository) DeleteSpace(spaceID int64) error {
	if r.session == nil {
		return fmt.Errorf("database session not initialized")
	}

	query := models.SpaceTable.DeleteBuilder().Where(qb.Eq("id")).Query(*r.session)
	if err := query.Bind(spaceID).ExecRelease(); err != nil {
		log.Printf("[DeleteSpace] Error deleting space %d: %v", spaceID, err)
		return err
	}

	return nil
}

func (r *SpaceRepository) GetAllSpaces() ([]models.Space, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	var spaces []models.Space
	query := qb.Select("spaces").Query(*r.session)
	if err := query.SelectRelease(&spaces); err != nil {
		log.Printf("[GetAllSpaces] Error getting all spaces: %v", err)
		return nil, err
	}

	return spaces, nil
}

func (r *SpaceRepository) GetUserSpaces(userID string) ([]models.Space, error) {
	if r.session == nil {
		return nil, fmt.Errorf("database session not initialized")
	}

	// First get space IDs from space_members_by_user
	var spaceIDs []int64
	query := qb.Select("space_members_by_user").Where(qb.Eq("user_id")).Query(*r.session)
	if err := query.Bind(userID).SelectRelease(&spaceIDs); err != nil {
		log.Printf("[GetUserSpaces] Error getting space IDs for user %s: %v", userID, err)
		return nil, err
	}

	if len(spaceIDs) == 0 {
		return []models.Space{}, nil
	}

	// Get space details for each space ID
	spaces := make([]models.Space, 0, len(spaceIDs))
	for _, spaceID := range spaceIDs {
		space, err := r.GetSpace(spaceID)
		if err != nil {
			log.Printf("[GetUserSpaces] Error getting space %d: %v", spaceID, err)
			continue
		}
		spaces = append(spaces, *space)
	}

	return spaces, nil
}
