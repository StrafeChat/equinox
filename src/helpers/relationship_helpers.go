package helpers

import (
	"errors"
	"strconv"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3/qb"
)

// GetUserByUsernameAndDiscriminator retrieves a user by their username and discriminator
func GetUserByUsernameAndDiscriminator(username string, discriminator int) (models.UserByUsernameAndDiscriminator, error) {
	var user models.UserByUsernameAndDiscriminator
	userByUsernameAndDiscriminator := models.UserByUsernameAndDiscriminatorTable.SelectBuilder().
		Columns("*").
		Where(qb.Eq("username"), qb.Eq("discriminator")).
		Limit(1)

	userByUsernameAndDiscriminatorQuery := userByUsernameAndDiscriminator.Query(*database.Session).
		BindStruct(models.UserByUsernameAndDiscriminator{
			Username:      username,
			Discriminator: discriminator,
		})

	err := userByUsernameAndDiscriminatorQuery.GetRelease(&user)
	return user, err
}

// GetRelationshipByID retrieves a relationship by its ID
func GetRelationshipByID(relationshipId int64) (models.Relationship, error) {
	var relationship models.Relationship
	relationshipById := models.RelationshipTable.SelectBuilder().
		Columns("*").
		Where(qb.Eq("id")).
		Limit(1)

	relationshipByIdQuery := relationshipById.Query(*database.Session).
		BindMap(qb.M{"id": relationshipId})

	err := relationshipByIdQuery.GetRelease(&relationship)
	return relationship, err
}

// GetUserByID retrieves a user by their ID
func GetUserByID(userId int64) (models.User, error) {
	var user models.User
	userQuery := models.UserTable.SelectBuilder().
		Columns("*").
		Where(qb.Eq("id")).
		Limit(1).
		Query(*database.Session).
		BindMap(qb.M{
			"id": userId,
		})
	
	err := userQuery.GetRelease(&user)
	return user, err
}

// CheckExistingRelationship checks for an existing relationship between users
func CheckExistingRelationship(userId, targetUserId string) (bool, error) {
	// First check if the users are already friends by checking their relationships arrays
	var user models.User
	userQuery := models.UserTable.SelectBuilder().
		Columns("relationships").
		Where(qb.Eq("id")).
		Limit(1).
		Query(*database.Session).
		BindMap(qb.M{
			"id": userId,
		})

	err := userQuery.GetRelease(&user)
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return false, err
	}

	// Check if targetUserId is in the relationships array
	for _, relationshipId := range user.Relationships {
		if relationshipId == targetUserId {
			return true, nil
		}
	}

	// Also check if userId is in target user's relationships array
	var targetUser models.User
	targetUserQuery := models.UserTable.SelectBuilder().
		Columns("relationships").
		Where(qb.Eq("id")).
		Limit(1).
		Query(*database.Session).
		BindMap(qb.M{
			"id": targetUserId,
		})

	err = targetUserQuery.GetRelease(&targetUser)
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return false, err
	}

	// Check if userId is in target user's relationships array
	for _, relationshipId := range targetUser.Relationships {
		if relationshipId == userId {
			return true, nil
		}
	}

	// Check relationship by sender
	relationshipBySenderQuery := models.RelationshipBySenderTable.SelectBuilder().
		Columns("*").
		Where(qb.Eq("sender_id")).
		Limit(1).
		Query(*database.Session).
		BindMap(qb.M{
			"sender_id": userId,
		})

	var existingRelationshipBySender models.RelationshipBySender
	err = relationshipBySenderQuery.GetRelease(&existingRelationshipBySender)
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return false, err
	}
	if err == nil && existingRelationshipBySender.RecipientId == targetUserId {
		return true, nil
	}

	// Check relationship by recipient
	relationshipByRecipientQuery := models.RelationshipByRecipientTable.SelectBuilder().
		Columns("*").
		Where(qb.Eq("recipient_id")).
		Limit(1).
		Query(*database.Session).
		BindMap(qb.M{
			"recipient_id": userId,
		})

	var existingRelationshipByRecipient models.RelationshipByRecipient
	err = relationshipByRecipientQuery.GetRelease(&existingRelationshipByRecipient)
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return false, err
	}
	if err == nil && existingRelationshipByRecipient.SenderId == targetUserId {
		return true, nil
	}

	return false, nil
}

// InsertRelationship inserts a new relationship into all required tables
func InsertRelationship(relationship models.Relationship) error {
	// Insert into relationships table
	if err := models.RelationshipTable.InsertBuilder().
		Query(*database.Session).
		BindStruct(&relationship).
		ExecRelease(); err != nil {
		return err
	}

	// Insert into relationships_by_sender table
	relationshipBySender := models.RelationshipBySender{
		SenderId:    strconv.FormatInt(relationship.SenderId, 10),
		RecipientId: strconv.FormatInt(relationship.RecipientId, 10),
		Id:          strconv.FormatInt(relationship.Id, 10),
	}
	if err := models.RelationshipBySenderTable.InsertBuilder().
		Query(*database.Session).
		BindStruct(&relationshipBySender).
		ExecRelease(); err != nil {
		return err
	}

	// Insert into relationships_by_recipient table
	relationshipByRecipient := models.RelationshipByRecipient{
		SenderId:    strconv.FormatInt(relationship.SenderId, 10),
		RecipientId: strconv.FormatInt(relationship.RecipientId, 10),
		Id:          strconv.FormatInt(relationship.Id, 10),
	}
	if err := models.RelationshipByRecipientTable.InsertBuilder().
		Query(*database.Session).
		BindStruct(&relationshipByRecipient).
		ExecRelease(); err != nil {
		return err
	}

	return nil
}

// UpdateRelationships updates both users' relationships
func UpdateRelationships(recipient, sender *models.User, relationship models.Relationship) error {
	// Update recipient's relationships
	if recipient.Relationships == nil {
		recipient.Relationships = []string{}
	}
	recipient.Relationships = append(recipient.Relationships, strconv.FormatInt(relationship.SenderId, 10))

	if err := models.UserTable.UpdateBuilder().
		Set("relationships").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"relationships": recipient.Relationships,
			"id":           recipient.ID,
		}).
		ExecRelease(); err != nil {
		return err
	}

	// Update sender's relationships
	if sender.Relationships == nil {
		sender.Relationships = []string{}
	}
	sender.Relationships = append(sender.Relationships, strconv.FormatInt(relationship.RecipientId, 10))

	if err := models.UserTable.UpdateBuilder().
		Set("relationships").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"relationships": sender.Relationships,
			"id":           sender.ID,
		}).
		ExecRelease(); err != nil {
		return err
	}

	return nil
}

// DeleteRelationship deletes a relationship from all tables
func DeleteRelationship(relationship models.Relationship, relationshipId string) error {
	// Delete from RelationshipByRecipient
	if err := models.RelationshipByRecipientTable.DeleteBuilder().
		Where(qb.Eq("recipient_id"), qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"recipient_id": strconv.FormatInt(relationship.RecipientId, 10),
			"id":          relationshipId,
		}).
		ExecRelease(); err != nil {
		return err
	}

	// Delete from RelationshipBySender
	if err := models.RelationshipBySenderTable.DeleteBuilder().
		Where(qb.Eq("sender_id"), qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"sender_id": strconv.FormatInt(relationship.SenderId, 10),
			"id":       relationshipId,
		}).
		ExecRelease(); err != nil {
		return err
	}

	// Delete from main Relationship table
	if err := models.RelationshipTable.DeleteBuilder().
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{"id": relationshipId}).
		ExecRelease(); err != nil {
		return err
	}

	return nil
}
