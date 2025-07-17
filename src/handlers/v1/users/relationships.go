package handlers_v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

func RelationshipsPost(c fiber.Ctx) error {
	body := new(types.RelationshipsPost)

	if err := c.Bind().Body(body); err != nil {
		log.Printf("Error parsing request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON format."})
	}

	if body.Username == "" || body.Discriminator == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "You need to provide a username and discriminator."})
	}

	var userNameAndDiscrimUser models.UserByUsernameAndDiscriminator
	var userErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		userNameAndDiscrimUser, userErr = helpers.GetUserByUsernameAndDiscriminator(body.Username, body.Discriminator)
	}()
	wg.Wait()

	if userErr != nil {
		log.Printf("Error finding user by username and discriminator: %v", userErr)
		if errors.Is(userErr, gocql.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "User not found."})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while checking for the user."})
	}

	if userNameAndDiscrimUser.ID == c.Locals("user").(models.User).ID {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "You cannot send a friend request to yourself."})
	}

	var exists bool
	var checkErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		exists, checkErr = helpers.CheckExistingRelationship(c.Locals("user").(models.User).ID, userNameAndDiscrimUser.ID)
	}()
	wg.Wait()

	if checkErr != nil {
		log.Printf("Error checking relationships: %v", checkErr)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while checking for the relationship."})
	}
	if exists {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "A relationship already exists with this user."})
	}

	// Create new relationship
	relationshipId := helpers.GenerateRelationshipID()

	senderId, err := strconv.ParseInt(c.Locals("user").(models.User).ID, 10, 64)
	if err != nil {
		log.Printf("Error converting SenderId: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to create relationship."})
	}

	recipientId, err := strconv.ParseInt(userNameAndDiscrimUser.ID, 10, 64)
	if err != nil {
		log.Printf("Error converting RecipientId: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to create relationship."})
	}

	newRelationship := models.Relationship{
		Id:          relationshipId,
		SenderId:    senderId,
		RecipientId: recipientId,
		CreatedAt:   time.Now(),
	}

	// Insert relationships concurrently
	var insertErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		insertErr = helpers.InsertRelationship(newRelationship)
	}()
	wg.Wait()

	if insertErr != nil {
		log.Printf("Error creating relationship: %v", insertErr)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to create relationship."})
	}

	event := types.RelationshipEvent{
		Type:        types.RelationshipEventCreate,
		ID:          strconv.FormatInt(newRelationship.Id, 10),
		SenderId:    strconv.FormatInt(newRelationship.SenderId, 10),
		RecipientId: strconv.FormatInt(newRelationship.RecipientId, 10),
		CreatedAt:   newRelationship.CreatedAt.UnixMilli(),
	}

	eventJson, err := json.Marshal(event)
	if err != nil {
		log.Printf("Error marshaling relationship event: %v", err)
	} else {
		if err := database.Rdb.Publish("RELATIONSHIP_EVENTS", string(eventJson)).Err(); err != nil {
			log.Printf("Error publishing relationship event: %v", err)
		}
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"message":      "Friend request sent successfully.",
		"relationship": newRelationship,
	})
}

func RelationshipsPut(c fiber.Ctx) error {
	params := new(types.RelationshipsPutParams)
	params.ID = c.Params("id")

	if params.ID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "You need to provide an ID."})
	}

	relationshipId, err := strconv.ParseInt(params.ID, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid relationship ID format."})
	}

	// Get relationship concurrently
	var relationship models.Relationship
	var relationshipErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		relationship, relationshipErr = helpers.GetRelationshipByID(relationshipId)
	}()
	wg.Wait()

	if relationshipErr != nil {
		log.Printf("Error finding relationship by id: %v", relationshipErr)
		if errors.Is(relationshipErr, gocql.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Relationship not found."})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while checking for the relationship."})
	}

	if relationship == (models.Relationship{}) {
		log.Printf("Error: relationship is empty")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid relationship data."})
	}

	recipientIdStr := strconv.FormatInt(relationship.RecipientId, 10)
	if recipientIdStr != c.Locals("user").(models.User).ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "You can only accept friend requests sent to you."})
	}

	// Get both users concurrently
	var recipient, sender models.User
	var recipientErr, senderErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		recipient, recipientErr = helpers.GetUserByID(relationship.RecipientId)
	}()
	go func() {
		defer wg.Done()
		sender, senderErr = helpers.GetUserByID(relationship.SenderId)
	}()
	wg.Wait()

	if recipientErr != nil {
		log.Printf("Error getting recipient user: %v", recipientErr)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while accepting the friend request."})
	}
	if senderErr != nil {
		log.Printf("Error getting sender user: %v", senderErr)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while accepting the friend request."})
	}

	// Update relationships concurrently
	var updateErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		updateErr = helpers.UpdateRelationships(&recipient, &sender, relationship)
	}()
	wg.Wait()

	if updateErr != nil {
		log.Printf("Error updating relationships: %v", updateErr)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to update relationships."})
	}

	// Delete relationship records concurrently
	var deleteErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		deleteErr = helpers.DeleteRelationship(relationship, params.ID)
	}()
	wg.Wait()

	if deleteErr != nil {
		log.Printf("Error deleting relationship records: %v", deleteErr)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to delete relationship records."})
	}

	event := types.RelationshipEvent{
		Type:        types.RelationshipEventAccept,
		ID:          strconv.FormatInt(relationship.Id, 10),
		SenderId:    strconv.FormatInt(relationship.SenderId, 10),
		RecipientId: strconv.FormatInt(relationship.RecipientId, 10),
		CreatedAt:   time.Now().UnixMilli(),
	}

	eventJson, err := json.Marshal(event)
	if err != nil {
		log.Printf("Error marshaling relationship accept event: %v", err)
	} else {
		if err := database.Rdb.Publish("RELATIONSHIP_EVENTS", string(eventJson)).Err(); err != nil {
			log.Printf("Error publishing relationship accept event: %v", err)
		}
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Friend request accepted successfully."})
}

func RelationshipsDelete(c fiber.Ctx) error {
	params := new(types.RelationshipsDeleteParams)
	params.ID = c.Params("id")

	if params.ID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "You need to provide an ID."})
	}

	relationshipId, err := strconv.ParseInt(params.ID, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid relationship ID format."})
	}

	if containsString(c.Locals("user").(models.User).Relationships, strconv.FormatInt(relationshipId, 10)) {
		user := c.Locals("user").(models.User)
		otherUserById := models.UserTable.SelectBuilder().
			Columns("id", "username", "email", "discriminator", "display_name", "about_me", "bio", "bot", "created_at", "updated_at", "avatar", "banner", "accent_color", "locale", "presence", "bots", "relationships").
			Where(qb.Eq("id")).
			Limit(1)

		var otherUser models.User
		userByIdQuery := otherUserById.Query(*database.Session).
			BindStruct(models.User{
				ID: strconv.FormatInt(relationshipId, 10),
			})
		if queryErr := userByIdQuery.GetRelease(&otherUser); queryErr != nil {
			if errors.Is(err, gocql.ErrNotFound) {
				return c.Status(401).SendString("Unauthorized")
			}
			return c.Status(500).SendString("Internal Server Error")
		}
		removeErr := helpers.RemoveRelationships(&user, &otherUser)
		if removeErr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "Failed to remove friend")
		}

		event := types.RelationshipEvent{
			Type:        types.RelationshipEventDelete,
			ID:          strconv.FormatInt(relationshipId, 10),
			SenderId:    c.Locals("user").(models.User).ID,
			RecipientId: otherUser.ID,
			CreatedAt:   time.Now().Unix(),
		}

		eventJSON, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			log.Printf("Error marshaling relationship event: %v", marshalErr)
		} else {
			if marshalErr := database.Rdb.Publish("RELATIONSHIP_EVENTS", string(eventJSON)).Err(); marshalErr != nil {
				log.Printf("Error publishing relationship event: %v", marshalErr)
			}
		}

		return c.SendStatus(fiber.StatusNoContent)
	}

	// Get relationship concurrently
	var relationship models.Relationship
	var relationshipErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		relationship, relationshipErr = helpers.GetRelationshipByID(relationshipId)
	}()
	wg.Wait()

	if relationshipErr != nil {
		if errors.Is(relationshipErr, gocql.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Relationship not found."})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while fetching the relationship."})
	}

	userID, err := strconv.ParseInt(c.Locals("user").(models.User).ID, 10, 64)
	if err != nil {
		log.Printf("Error converting user ID: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid user ID"})
	}

	if relationship.SenderId != userID && relationship.RecipientId != userID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "You don't have permission to delete this relationship."})
	}

	// Delete relationship records concurrently
	var deleteErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		deleteErr = helpers.DeleteRelationship(relationship, params.ID)
	}()
	wg.Wait()

	if deleteErr != nil {
		log.Printf("Error deleting relationship records: %v", deleteErr)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to delete relationship records."})
	}

	event := types.RelationshipEvent{
		Type:        types.RelationshipEventDelete,
		ID:          params.ID,
		SenderId:    strconv.FormatInt(relationship.SenderId, 10),
		RecipientId: strconv.FormatInt(relationship.RecipientId, 10),
		CreatedAt:   time.Now().Unix(),
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		log.Printf("Error marshaling relationship event: %v", err)
	} else {
		if err := database.Rdb.Publish("RELATIONSHIP_EVENTS", string(eventJSON)).Err(); err != nil {
			log.Printf("Error publishing relationship event: %v", err)
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}

func containsString(slice []string, str string) bool {
	fmt.Println(slice)
	for _, v := range slice {
		if v == str {
			return true
		}
	}
	return false
}
