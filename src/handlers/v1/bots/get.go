package handlers_v1

import (
	"log"
	"strconv"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

type BotResponse struct {
	UserID            string  `json:"user_id"`
	Username          string  `json:"username"`
	Discriminator     int     `json:"discriminator"`
	Avatar            *string `json:"avatar"`
	Description       string  `json:"description"`
	Public            bool    `json:"public"`
	Discoverable      bool    `json:"discoverable"`
	TermsOfServiceURL string  `json:"terms_of_service_url"`
	PrivacyPolicyURL  string  `json:"privacy_policy_url"`
	CreatedAt         string  `json:"created_at"`
}

// GetMyBots returns all bots owned by the authenticated user
func GetMyBots(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	userID := user.ID

	// Get user's bots list
	var userWithBots models.User
	userQuery := qb.Select(models.UserTable.Name()).
		Columns("bots").
		Where(qb.Eq("id")).
		Query(*database.Session)

	if err := userQuery.Bind(userID).Get(&userWithBots); err != nil {
		log.Printf("Error fetching user: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch user data",
		})
	}

	if len(userWithBots.Bots) == 0 {
		return c.JSON([]BotResponse{})
	}

	// Convert string IDs to int64 for query
	botIDs := make([]int64, len(userWithBots.Bots))
	for i, botIDStr := range userWithBots.Bots {
		botID, err := strconv.ParseInt(botIDStr, 10, 64)
		if err != nil {
			log.Printf("Error parsing bot ID %s: %v", botIDStr, err)
			continue
		}
		botIDs[i] = botID
	}

	// Get bot details
	var bots []models.Bot
	botsQuery := qb.Select(models.BotTable.Name()).
		Where(qb.In("user_id")).
		Query(*database.Session)

	if err := botsQuery.Bind(userWithBots.Bots).Select(&bots); err != nil {
		log.Printf("Error fetching bots: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch bots",
		})
	}

	// Get user details for each bot
	var botUsers []models.User
	botUsersQuery := qb.Select(models.UserTable.Name()).
		Columns("id", "username", "discriminator", "avatar", "created_at").
		Where(qb.In("id")).
		Query(*database.Session)

	if err := botUsersQuery.Bind(userWithBots.Bots).Select(&botUsers); err != nil {
		log.Printf("Error fetching bot users: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch bot user data",
		})
	}

	// Create map for quick lookup
	botUserMap := make(map[string]models.User)
	for _, botUser := range botUsers {
		botUserMap[botUser.ID] = botUser
	}

	botMap := make(map[string]models.Bot)
	for _, bot := range bots {
		botMap[bot.UserID] = bot
	}

	// Build response
	var response []BotResponse
	for _, botIDStr := range userWithBots.Bots {
		botUser, userExists := botUserMap[botIDStr]
		bot, botExists := botMap[botIDStr]

		if userExists && botExists {
			botResponse := BotResponse{
				UserID:            botUser.ID,
				Username:          botUser.Username,
				Discriminator:     botUser.Discriminator,
				Avatar:            botUser.Avatar,
				Description:       bot.Description,
				Public:            bot.Public,
				Discoverable:      bot.Discoverable,
				TermsOfServiceURL: bot.TermsOfServiceURL,
				PrivacyPolicyURL:  bot.PrivacyPolicyURL,
				CreatedAt:         botUser.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			}
			response = append(response, botResponse)
		}
	}

	return c.JSON(response)
}

// GetDiscoverableBots returns all public and discoverable bots
func GetDiscoverableBots(c fiber.Ctx) error {
	// Get all bots that are public and discoverable
	var bots []models.Bot
	botsQuery := qb.Select(models.BotTable.Name()).
		Where(qb.Eq("public"), qb.Eq("discoverable")).
		Query(*database.Session)

	if err := botsQuery.Bind(true, true).Select(&bots); err != nil {
		log.Printf("Error fetching discoverable bots: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch discoverable bots",
		})
	}

	if len(bots) == 0 {
		return c.JSON([]BotResponse{})
	}

	// Get bot user IDs
	botUserIDs := make([]string, len(bots))
	for i, bot := range bots {
		botUserIDs[i] = bot.UserID
	}

	// Get user details for each bot
	var botUsers []models.User
	botUsersQuery := qb.Select(models.UserTable.Name()).
		Columns("id", "username", "discriminator", "avatar", "created_at").
		Where(qb.In("id")).
		Query(*database.Session)

	if err := botUsersQuery.Bind(botUserIDs).Select(&botUsers); err != nil {
		log.Printf("Error fetching bot users: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch bot user data",
		})
	}

	// Create maps for quick lookup
	botUserMap := make(map[string]models.User)
	for _, botUser := range botUsers {
		botUserMap[botUser.ID] = botUser
	}

	botMap := make(map[string]models.Bot)
	for _, bot := range bots {
		botMap[bot.UserID] = bot
	}

	// Build response
	var response []BotResponse
	for _, botUserID := range botUserIDs {
		botUser, userExists := botUserMap[botUserID]
		bot, botExists := botMap[botUserID]

		if userExists && botExists {
			botResponse := BotResponse{
				UserID:            botUser.ID,
				Username:          botUser.Username,
				Discriminator:     botUser.Discriminator,
				Avatar:            botUser.Avatar,
				Description:       bot.Description,
				Public:            bot.Public,
				Discoverable:      bot.Discoverable,
				TermsOfServiceURL: bot.TermsOfServiceURL,
				PrivacyPolicyURL:  bot.PrivacyPolicyURL,
				CreatedAt:         botUser.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			}
			response = append(response, botResponse)
		}
	}

	return c.JSON(response)
}

// GetBot returns details of a specific bot
func GetBot(c fiber.Ctx) error {
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

	// Check if user has permission to access this bot
	if !bot.Public && bot.OwnerID != userID {
		return c.Status(403).JSON(fiber.Map{
			"error": "You don't have permission to access this bot",
		})
	}

	// Get bot user details
	var botUser models.User
	botUserQuery := qb.Select(models.UserTable.Name()).
		Columns("id", "username", "discriminator", "avatar", "created_at").
		Where(qb.Eq("id")).
		Query(*database.Session)

	if err := botUserQuery.Bind(botID).Get(&botUser); err != nil {
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch bot user data",
		})
	}

	response := BotResponse{
		UserID:            botUser.ID,
		Username:          botUser.Username,
		Discriminator:     botUser.Discriminator,
		Avatar:            botUser.Avatar,
		Description:       bot.Description,
		Public:            bot.Public,
		Discoverable:      bot.Discoverable,
		TermsOfServiceURL: bot.TermsOfServiceURL,
		PrivacyPolicyURL:  bot.PrivacyPolicyURL,
		CreatedAt:         botUser.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}

	return c.JSON(response)
}
