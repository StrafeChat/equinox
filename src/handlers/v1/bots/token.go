package handlers_v1

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

type RegenerateTokenResponse struct {
	Token string `json:"token"`
}

// generateDiscordStyleToken creates a Discord-style bot token with three parts separated by periods
// Part 1: Base64-encoded bot ID
// Part 2: Base64-encoded timestamp
// Part 3: HMAC-SHA256 digest (truncated and base64-encoded)
func generateDiscordStyleTokenForRegenerate(botID string) (string, error) {
	// Part 1: Base64-encoded bot ID
	part1 := base64.StdEncoding.EncodeToString([]byte(botID))

	// Part 2: Base64-encoded timestamp (current time as 4-byte big-endian integer)
	timestamp := time.Now().Unix()
	timestampBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(timestampBytes, uint32(timestamp))
	part2 := base64.StdEncoding.EncodeToString(timestampBytes)

	// Part 3: HMAC-SHA256 digest
	// Generate a random secret key for HMAC (in production, this should be a server secret)
	secretKey := make([]byte, 32)
	_, err := rand.Read(secretKey)
	if err != nil {
		return "", err
	}

	// Create HMAC of the first two parts
	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(part1 + "." + part2))
	hmacDigest := mac.Sum(nil)
	
	// Use first 27 bytes of HMAC and base64 encode (Discord-like length)
	part3 := base64.StdEncoding.EncodeToString(hmacDigest[:27])

	// Combine all parts with periods
	return fmt.Sprintf("%s.%s.%s", part1, part2, part3), nil
}

func RegenerateToken(c fiber.Ctx) error {
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
			"error": "You don't have permission to regenerate this bot's token",
		})
	}

	// Generate new Discord-style bot token
	newToken, err := generateDiscordStyleTokenForRegenerate(botID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to generate new bot token",
		})
	}

	// Create batch for token update
	batch := database.Session.NewBatch(gocql.LoggedBatch)

	// Update bot token in bots table
	updateBotTokenQuery := qb.Update(models.BotTable.Name()).
		Set("bot_token").
		Where(qb.Eq("user_id")).
		Query(*database.Session)
	batch.Query(updateBotTokenQuery.Statement(), newToken, botID)

	// Delete old token from bots_by_token table
	deleteOldTokenQuery := qb.Delete(models.BotByTokenTable.Name()).
		Where(qb.Eq("bot_token")).
		Query(*database.Session)
	batch.Query(deleteOldTokenQuery.Statement(), bot.Token)

	// Insert new token in bots_by_token table
	insertNewTokenQuery := qb.Insert(models.BotByTokenTable.Name()).
		Columns("bot_token", "user_id").
		Query(*database.Session)
	batch.Query(insertNewTokenQuery.Statement(), newToken, botID)

	// Execute batch
	if err := database.Session.ExecuteBatch(batch); err != nil {
		log.Printf("Error regenerating bot token: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to regenerate bot token",
		})
	}

	response := RegenerateTokenResponse{
		Token: newToken,
	}

	return c.JSON(response)
}