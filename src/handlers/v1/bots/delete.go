package handlers_v1

import (
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

func DeleteBot(c fiber.Ctx) error {
	botID := c.Params("id")
	user := c.Locals("user").(models.User)
	userID := user.ID

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
			"error": "You don't have permission to delete this bot",
		})
	}

	// Get bot user details for username lookup deletion
	var botUser models.User
	botUserQuery := qb.Select(models.UserTable.Name()).
		Columns("username", "discriminator").
		Where(qb.Eq("id")).
		Query(*database.Session)

	if err := botUserQuery.Bind(botID).Get(&botUser); err != nil {
		log.Printf("Error fetching bot user for deletion: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch bot user data",
		})
	}

	// Create batch for deletion
	batch := database.Session.NewBatch(gocql.LoggedBatch)

	// Delete from bots table
	deleteBotQuery := qb.Delete(models.BotTable.Name()).
		Where(qb.Eq("user_id")).
		Query(*database.Session)
	batch.Query(deleteBotQuery.Statement(), botID)

	// Delete from bots_by_token table
	deleteBotByTokenQuery := qb.Delete(models.BotByTokenTable.Name()).
		Where(qb.Eq("bot_token")).
		Query(*database.Session)
	batch.Query(deleteBotByTokenQuery.Statement(), bot.Token)

	// Delete from users table
	deleteUserQuery := qb.Delete(models.UserTable.Name()).
		Where(qb.Eq("id")).
		Query(*database.Session)
	batch.Query(deleteUserQuery.Statement(), botID)

	// Delete from users_by_username_and_discriminator table
	deleteUsernameQuery := qb.Delete(models.UserByUsernameAndDiscriminatorTable.Name()).
		Where(qb.Eq("username"), qb.Eq("discriminator")).
		Query(*database.Session)
	batch.Query(deleteUsernameQuery.Statement(), botUser.Username, botUser.Discriminator)

	// Execute batch deletion
	if err := database.Session.ExecuteBatch(batch); err != nil {
		log.Printf("Error deleting bot: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to delete bot",
		})
	}

	// Update owner's bots list
	var owner models.User
	ownerQuery := qb.Select(models.UserTable.Name()).
		Columns("bots").
		Where(qb.Eq("id")).
		Query(*database.Session)

	if err := ownerQuery.Bind(userID).Get(&owner); err != nil {
		log.Printf("Error fetching owner for bot list update: %v", err)
	} else {
		// Remove bot from owner's bots list
		updatedBots := []string{}
		for _, existingBotID := range owner.Bots {
			if existingBotID != botID {
				updatedBots = append(updatedBots, existingBotID)
			}
		}

		updateOwnerQuery := qb.Update(models.UserTable.Name()).
			Set("bots", "updated_at").
			Where(qb.Eq("id")).
			Query(*database.Session)

		if err := updateOwnerQuery.Bind(updatedBots, time.Now(), userID).Exec(); err != nil {
			log.Printf("Error updating owner's bots list after deletion: %v", err)
		}
	}

	return c.Status(204).Send(nil)
}