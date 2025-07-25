package handlers_v1

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

type CreateBotRequest struct {
	Username      string `json:"username" validate:"required,min=2,max=32"`
	Discriminator int    `json:"discriminator" validate:"required,min=1,max=9999"`
	Description   string `json:"description" validate:"max=190"`
	Public        bool   `json:"public"`
	Discoverable  bool   `json:"discoverable"`
}

type CreateBotResponse struct {
	UserID       string `json:"user_id"`
	Token        string `json:"token"`
	Username     string `json:"username"`
	Description  string `json:"description"`
	Public       bool   `json:"public"`
	Discoverable bool   `json:"discoverable"`
}

// generateDiscordStyleToken creates a Discord-style bot token with three parts separated by periods
// Part 1: Base64-encoded bot ID
// Part 2: Base64-encoded timestamp
// Part 3: HMAC-SHA256 digest (truncated and base64-encoded)
func generateDiscordStyleTokenForCreate(botID string) (string, error) {
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

func CreateBot(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	userID := user.ID

	var req CreateBotRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Generate bot user ID
	botUserID := helpers.GenerateUserID()
	botUserIDStr := strconv.FormatInt(botUserID.Int64(), 10)

	// Generate Discord-style bot token
	botToken, err := generateDiscordStyleTokenForCreate(botUserIDStr)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to generate bot token",
		})
	}

	// Check if username and discriminator combination is available
	discriminator := req.Discriminator
	var existingUser models.UserByUsernameAndDiscriminator
	query := qb.Select(models.UserByUsernameAndDiscriminatorTable.Name()).
		Where(qb.Eq("username"), qb.Eq("discriminator")).
		Query(*database.Session)

	if err := query.Bind(req.Username, discriminator).Get(&existingUser); err == nil {
		return c.Status(409).JSON(fiber.Map{
			"error": "Username and discriminator combination already taken",
		})
	}

	// Use batch to ensure atomicity
	batch := database.Session.NewBatch(gocql.LoggedBatch)

	// Insert bot user
	botUserInsertQuery := qb.Insert(models.UserTable.Name()).
		Columns("id", "username", "discriminator", "bot", "created_at", "updated_at").
		Query(*database.Session)
	batch.Query(botUserInsertQuery.Statement(), botUserIDStr, req.Username, discriminator, true, time.Now(), time.Now())

	// Insert bot
	botInsertQuery := qb.Insert(models.BotTable.Name()).
		Columns("user_id", "owner_id", "bot_token", "public", "discoverable", "description").
		Query(*database.Session)
	batch.Query(botInsertQuery.Statement(), botUserIDStr, userID, botToken, req.Public, req.Discoverable, req.Description)

	// Insert bot token lookup
	botByTokenInsertQuery := qb.Insert(models.BotByTokenTable.Name()).
		Columns("bot_token", "user_id").
		Query(*database.Session)
	batch.Query(botByTokenInsertQuery.Statement(), botToken, botUserIDStr)

	// Insert username lookup
	userByUsernameInsertQuery := qb.Insert(models.UserByUsernameAndDiscriminatorTable.Name()).
		Columns("username", "discriminator", "id").
		Query(*database.Session)
	batch.Query(userByUsernameInsertQuery.Statement(), req.Username, discriminator, botUserIDStr)

	// Execute batch
	if err := database.Session.ExecuteBatch(batch); err != nil {
		log.Printf("Error creating bot: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to create bot",
		})
	}

	// Create default avatar for the bot by calling Nebula
	go createDefaultBotAvatar(botUserIDStr)

	// Update owner's bots list
	var owner models.User
	ownerQuery := qb.Select(models.UserTable.Name()).
		Where(qb.Eq("id")).
		Query(*database.Session)

	if err := ownerQuery.Bind(userID).Get(&owner); err != nil {
		log.Printf("Error fetching owner: %v", err)
	} else {
		// Add bot to owner's bots list
		updatedBots := append(owner.Bots, botUserIDStr)
		updateOwnerQuery := qb.Update(models.UserTable.Name()).
			Set("bots", "updated_at").
			Where(qb.Eq("id")).
			Query(*database.Session)

		if err := updateOwnerQuery.Bind(updatedBots, time.Now(), userID).Exec(); err != nil {
			log.Printf("Error updating owner's bots list: %v", err)
		}
	}

	response := CreateBotResponse{
		UserID:       botUserIDStr,
		Token:        botToken,
		Username:     req.Username,
		Description:  req.Description,
		Public:       req.Public,
		Discoverable: req.Discoverable,
	}

	return c.Status(201).JSON(response)
}

// createDefaultBotAvatar calls Nebula to create a default avatar for the bot
func createDefaultBotAvatar(botUserID string) {
	nebulaURL := os.Getenv("NEBULA_URL")
	if nebulaURL == "" {
		nebulaURL = "http://localhost:3001" // Default Nebula URL
	}

	// Create request payload
	payload := map[string]string{
		"bot_user_id": botUserID,
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling payload for bot avatar creation: %v", err)
		return
	}

	// Make HTTP request to Nebula
	resp, err := http.Post(nebulaURL+"/api/v1/bots/create-default-avatar", "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		log.Printf("Error calling Nebula for bot avatar creation: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Nebula returned non-200 status for bot avatar creation: %d", resp.StatusCode)
	}
}