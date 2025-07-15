package handlers_v1

import (
	"log"
	"strconv"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

func MeGet(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)

	relationshipsBySender := models.RelationshipBySenderTable.SelectBuilder().
		Columns("*").
		Where(qb.Eq("sender_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"sender_id": user.ID,
		})

	var senderRelationships []models.RelationshipBySender
	if err := relationshipsBySender.SelectRelease(&senderRelationships); err != nil {
		log.Printf("Error fetching sender relationships: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to fetch sender relationships",
		})
	}

	relationshipsByRecipient := models.RelationshipByRecipientTable.SelectBuilder().
		Columns("*").
		Where(qb.Eq("recipient_id")).
		Query(*database.Session).
		BindMap(qb.M{
			"recipient_id": user.ID,
		})

	var recipientRelationships []models.RelationshipByRecipient
	if err := relationshipsByRecipient.SelectRelease(&recipientRelationships); err != nil {
		log.Printf("Error fetching recipient relationships: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to fetch recipient relationships",
		})
	}

	relationshipIds := make([]int64, 0)
	log.Printf("Found %d sender relationships: %+v", len(senderRelationships), senderRelationships)
	for _, r := range senderRelationships {
		id, err := strconv.ParseInt(r.Id, 10, 64)
		if err != nil {
			log.Printf("Error parsing sender relationship ID: %v", err)
			continue
		}
		log.Printf("Adding sender relationship ID: %v (sender_id: %v, recipient_id: %v)", id, r.SenderId, r.RecipientId)
		relationshipIds = append(relationshipIds, id)
	}
	log.Printf("Found %d recipient relationships: %+v", len(recipientRelationships), recipientRelationships)
	for _, r := range recipientRelationships {
		id, err := strconv.ParseInt(r.Id, 10, 64)
		if err != nil {
			log.Printf("Error parsing recipient relationship ID: %v", err)
			continue
		}
		log.Printf("Adding recipient relationship ID: %v (sender_id: %v, recipient_id: %v)", id, r.SenderId, r.RecipientId)
		relationshipIds = append(relationshipIds, id)
	}

	// Remove duplicate IDs if any
	seen := make(map[int64]bool)
	uniqueIds := make([]int64, 0)
	for _, id := range relationshipIds {
		if !seen[id] {
			seen[id] = true
			uniqueIds = append(uniqueIds, id)
		}
	}
	relationshipIds = uniqueIds

	log.Printf("Final unique relationship IDs: %v", relationshipIds)

	var relationships []models.Relationship
	if len(relationshipIds) > 0 {
		for _, id := range relationshipIds {
			var rel models.Relationship
			relationshipsQuery := models.RelationshipTable.SelectBuilder().
				Columns("*").
				Where(qb.Eq("id")).
				Query(*database.Session).
				BindMap(qb.M{
					"id": id,
				})

			if err := relationshipsQuery.Get(&rel); err != nil {
				log.Printf("Error fetching relationship detail for ID %v: %v", id, err)
				continue
			}
			relationships = append(relationships, rel)
		}
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("Panic in ClientUserResponseFormat: %v", r)
		}
	}()

	response := helpers.ClientUserResponseFormat(user, relationships)
	log.Printf("User: %+v", user)
	log.Printf("Relationships: %+v", relationships)
	log.Printf("Response: %+v", response)

	return c.Status(fiber.StatusOK).JSON(response)
}
