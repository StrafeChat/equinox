package handlers_v1

import (
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
	"github.com/gocql/gocql"
)

type UpdateBotRequest struct {
	Username          *string `json:"username" validate:"omitempty,min=2,max=32"`
	Description       *string `json:"description" validate:"omitempty,max=190"`
	Public            *bool   `json:"public"`
	Discoverable      *bool   `json:"discoverable"`
	TermsOfServiceURL *string `json:"terms_of_service_url" validate:"omitempty,url"`
	PrivacyPolicyURL  *string `json:"privacy_policy_url" validate:"omitempty,url"`
}

func UpdateBot(c fiber.Ctx) error {
	botID := c.Params("id")
	user := c.Locals("user").(models.User)
	userID := user.ID

	var req UpdateBotRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Validate request

	// Get bot details
	var bot models.Bot
	botQuery := qb.Select(models.BotTable.Name()).
		Where(qb.Eq("user_id")).
		Query(*database.Session)

	if err := botQuery.Bind(botID).Get(&bot); err != nil {
		return c.Status(404).JSON(fiber.Map{
			"error": "Bot not found",
		})
	}

	// Check if user owns this bot
	if bot.OwnerID != userID {
		return c.Status(403).JSON(fiber.Map{
			"error": "You don't have permission to modify this bot",
		})
	}

	// Check if username is being updated and if it's available
	if req.Username != nil && *req.Username != "" {
		discriminator := 0000 // Bots always have discriminator 0000
		var existingUser models.UserByUsernameAndDiscriminator
		usernameQuery := qb.Select(models.UserByUsernameAndDiscriminatorTable.Name()).
			Where(qb.Eq("username"), qb.Eq("discriminator")).
			Query(*database.Session)

		if err := usernameQuery.Bind(*req.Username, discriminator).Get(&existingUser); err == nil {
			// Username exists, check if it's the same bot
			if existingUser.ID != botID {
				return c.Status(409).JSON(fiber.Map{
					"error": "Username already taken",
				})
			}
		}
	}

	// Prepare update queries
	batch := database.Session.NewBatch(gocql.LoggedBatch)

	// Update bot table
	botUpdateFields := []string{}
	botUpdateValues := []interface{}{}

	if req.Description != nil {
		botUpdateFields = append(botUpdateFields, "description")
		botUpdateValues = append(botUpdateValues, *req.Description)
	}
	if req.Public != nil {
		botUpdateFields = append(botUpdateFields, "public")
		botUpdateValues = append(botUpdateValues, *req.Public)
	}
	if req.Discoverable != nil {
		botUpdateFields = append(botUpdateFields, "discoverable")
		botUpdateValues = append(botUpdateValues, *req.Discoverable)
	}
	if req.TermsOfServiceURL != nil {
		botUpdateFields = append(botUpdateFields, "terms_of_service_url")
		botUpdateValues = append(botUpdateValues, *req.TermsOfServiceURL)
	}
	if req.PrivacyPolicyURL != nil {
		botUpdateFields = append(botUpdateFields, "privacy_policy_url")
		botUpdateValues = append(botUpdateValues, *req.PrivacyPolicyURL)
	}

	if len(botUpdateFields) > 0 {
		botUpdateQuery := qb.Update(models.BotTable.Name()).
			Set(botUpdateFields...).
			Where(qb.Eq("user_id")).
			Query(*database.Session)

		botUpdateValues = append(botUpdateValues, botID)
		batch.Query(botUpdateQuery.Statement(), botUpdateValues...)
	}

	// Update user table if username is being changed
	if req.Username != nil && *req.Username != "" {
		// Get current bot user data
		var botUser models.User
		botUserQuery := qb.Select(models.UserTable.Name()).
			Columns("username").
			Where(qb.Eq("id")).
			Query(*database.Session)

		if err := botUserQuery.Bind(botID).Get(&botUser); err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to fetch bot user data",
			})
		}

		// Update username in users table
		userUpdateQuery := qb.Update(models.UserTable.Name()).
			Set("username", "updated_at").
			Where(qb.Eq("id")).
			Query(*database.Session)
		batch.Query(userUpdateQuery.Statement(), *req.Username, time.Now(), botID)

		// Delete old username lookup
		deleteOldUsernameQuery := qb.Delete(models.UserByUsernameAndDiscriminatorTable.Name()).
			Where(qb.Eq("username"), qb.Eq("discriminator")).
			Query(*database.Session)
		batch.Query(deleteOldUsernameQuery.Statement(), botUser.Username, 0000)

		// Insert new username lookup
		insertNewUsernameQuery := qb.Insert(models.UserByUsernameAndDiscriminatorTable.Name()).
			Columns("username", "discriminator", "id").
			Query(*database.Session)
		batch.Query(insertNewUsernameQuery.Statement(), *req.Username, 0000, botID)
	}

	// Execute batch if there are updates
	if batch.Size() > 0 {
		if err := database.Session.ExecuteBatch(batch); err != nil {
			log.Printf("Error updating bot: %v", err)
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to update bot",
			})
		}
	}

	// Return updated bot data
	return GetBot(c)
}