package handlers_v1

import (
	"log"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/events"
	"github.com/StrafeChat/equinox/src/services"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
	"github.com/gocql/gocql"
)

type AddBotToSpaceRequest struct {
	BotID string `json:"bot_id" validate:"required"`
}

type RemoveBotFromSpaceRequest struct {
	BotID string `json:"bot_id" validate:"required"`
}

// AddBotToSpace handles POST /spaces/:id/bots
func AddBotToSpace(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	userID := user.ID
	spaceIDStr := c.Params("id")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	var req AddBotToSpaceRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Check if user has permission to manage the space (space owner, administrator, or manage space permission)
	membershipService := services.NewSpaceMembershipService(database.Session)
	
	// Debug membership check
	log.Printf("[DEBUG] Checking membership for user %s in space %d", userID, spaceID)
	membershipService.DebugSpaceMembership(spaceID, userID)
	
	// First check if user is a member of the space
	if !membershipService.IsSpaceMember(spaceID, userID) {
		log.Printf("[DEBUG] User %s is NOT a member of space %d", userID, spaceID)
		return c.Status(403).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}
	
	log.Printf("[DEBUG] User %s IS a member of space %d", userID, spaceID)
	
	// Check if user is space owner
	if membershipService.IsSpaceOwner(spaceID, userID) {
		// Space owners can manage bots
	} else {
		// Check if user has ADMINISTRATOR or MANAGE_SPACE permission
		userIDInt, err := strconv.ParseInt(userID, 10, 64)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "Internal server error",
			})
		}
		
		hasAdminPerm, err := utils.CheckPermission(database.Session, userIDInt, spaceID, utils.ADMINISTRATOR)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to check permissions",
			})
		}
		
		hasManageSpacePerm, err := utils.CheckPermission(database.Session, userIDInt, spaceID, utils.MANAGE_SPACE)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to check permissions",
			})
		}
		
		if !hasAdminPerm && !hasManageSpacePerm {
			return c.Status(403).JSON(fiber.Map{
				"error": "You don't have permission to manage bots in this space",
			})
		}
	}



	// Verify the bot exists
	var bot models.Bot
	botQuery := qb.Select(models.BotTable.Name()).
		Where(qb.Eq("user_id")).
		Query(*database.Session)

	if err := botQuery.Bind(req.BotID).Get(&bot); err != nil {
		return c.Status(404).JSON(fiber.Map{
			"error": "Bot not found",
		})
	}

	// Check if user can add this bot (either owns it or it's public)
	if bot.OwnerID != userID && !bot.Public {
		return c.Status(403).JSON(fiber.Map{
			"error": "You can only add your own bots or public bots to spaces",
		})
	}

	// Check if bot is already in the space
	if membershipService.IsSpaceMember(spaceID, req.BotID) {
		return c.Status(409).JSON(fiber.Map{
			"error": "Bot is already a member of this space",
		})
	}

	// Add bot to space
	now := time.Now()

	// Create batch for adding bot to space
	batch := database.Session.NewBatch(gocql.LoggedBatch)

	// Insert into space_members
	spaceMemberInsertQuery := qb.Insert(models.SpaceMemberTable.Name()).
		Columns("space_id", "user_id", "joined_at", "deaf", "mute", "flags", "pending").
		Query(*database.Session)
	batch.Query(spaceMemberInsertQuery.Statement(), spaceID, req.BotID, now, false, false, 0, false)

	// Insert into space_members_by_user
	spaceMemberByUserInsertQuery := qb.Insert(models.SpaceMembersByUserTable.Name()).
		Columns("user_id", "space_id", "joined_at").
		Query(*database.Session)
	batch.Query(spaceMemberByUserInsertQuery.Statement(), req.BotID, spaceID, now)

	// Execute batch
	if err := database.Session.ExecuteBatch(batch); err != nil {
		log.Printf("Error adding bot to space: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to add bot to space",
		})
	}

	// Update bot user's spaces list
	var botUser models.User
	botUserQuery := qb.Select(models.UserTable.Name()).
		Columns("spaces").
		Where(qb.Eq("id")).
		Query(*database.Session)

	if err := botUserQuery.Bind(req.BotID).Get(&botUser); err == nil {
		updatedSpaces := append(botUser.Spaces, spaceID)
		updateBotUserQuery := qb.Update(models.UserTable.Name()).
			Set("spaces", "updated_at").
			Where(qb.Eq("id")).
			Query(*database.Session)

		if err := updateBotUserQuery.Bind(updatedSpaces, time.Now(), req.BotID).Exec(); err != nil {
			log.Printf("Error updating bot user's spaces list: %v", err)
		}
	}

	// Publish space member add event
	if err := events.PublishSpaceMemberAddEvent(spaceID, req.BotID, ""); err != nil {
		log.Printf("Error publishing space member add event: %v", err)
	}

	return c.Status(201).JSON(fiber.Map{
		"message": "Bot added to space successfully",
	})
}

// RemoveBotFromSpace handles DELETE /spaces/:id/bots
func RemoveBotFromSpace(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	userID := user.ID
	spaceIDStr := c.Params("id")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	var req RemoveBotFromSpaceRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Check if user has permission to manage the space (space owner, administrator, or manage space permission)
	membershipService := services.NewSpaceMembershipService(database.Session)
	
	// First check if user is a member of the space
	if !membershipService.IsSpaceMember(spaceID, userID) {
		return c.Status(403).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}
	
	// Check if user is space owner
	if membershipService.IsSpaceOwner(spaceID, userID) {
		// Space owners can manage bots
	} else {
		// Check if user has ADMINISTRATOR or MANAGE_SPACE permission
		userIDInt, err := strconv.ParseInt(userID, 10, 64)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "Internal server error",
			})
		}
		
		hasAdminPerm, err := utils.CheckPermission(database.Session, userIDInt, spaceID, utils.ADMINISTRATOR)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to check permissions",
			})
		}
		
		hasManageSpacePerm, err := utils.CheckPermission(database.Session, userIDInt, spaceID, utils.MANAGE_SPACE)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to check permissions",
			})
		}
		
		if !hasAdminPerm && !hasManageSpacePerm {
			return c.Status(403).JSON(fiber.Map{
				"error": "You don't have permission to manage bots in this space",
			})
		}
	}

	// Verify the bot exists
	var bot models.Bot
	botQuery := qb.Select(models.BotTable.Name()).
		Where(qb.Eq("user_id")).
		Query(*database.Session)

	if err := botQuery.Bind(req.BotID).Get(&bot); err != nil {
		return c.Status(404).JSON(fiber.Map{
			"error": "Bot not found",
		})
	}

	// Check if user can remove this bot (either owns it or it's public)
	if bot.OwnerID != userID && !bot.Public {
		return c.Status(403).JSON(fiber.Map{
			"error": "You can only remove your own bots or public bots from spaces",
		})
	}

	// Check if bot is in the space
	if !membershipService.IsSpaceMember(spaceID, req.BotID) {
		return c.Status(404).JSON(fiber.Map{
			"error": "Bot is not a member of this space",
		})
	}

	// Remove bot from space
	batch := database.Session.NewBatch(gocql.LoggedBatch)

	// Delete from space_members
	deleteSpaceMemberQuery := qb.Delete(models.SpaceMemberTable.Name()).
		Where(qb.Eq("space_id"), qb.Eq("user_id")).
		Query(*database.Session)
	batch.Query(deleteSpaceMemberQuery.Statement(), spaceID, req.BotID)

	// Delete from space_members_by_user
	deleteSpaceMemberByUserQuery := qb.Delete(models.SpaceMembersByUserTable.Name()).
		Where(qb.Eq("user_id"), qb.Eq("space_id")).
		Query(*database.Session)
	batch.Query(deleteSpaceMemberByUserQuery.Statement(), req.BotID, spaceID)

	// Execute batch
	if err := database.Session.ExecuteBatch(batch); err != nil {
		log.Printf("Error removing bot from space: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to remove bot from space",
		})
	}

	// Update bot user's spaces list
	var botUser models.User
	botUserQuery := qb.Select(models.UserTable.Name()).
		Columns("spaces").
		Where(qb.Eq("id")).
		Query(*database.Session)

	if err := botUserQuery.Bind(req.BotID).Get(&botUser); err == nil {
		updatedSpaces := []int64{}
		for _, existingSpaceID := range botUser.Spaces {
			if existingSpaceID != spaceID {
				updatedSpaces = append(updatedSpaces, existingSpaceID)
			}
		}

		updateBotUserQuery := qb.Update(models.UserTable.Name()).
			Set("spaces", "updated_at").
			Where(qb.Eq("id")).
			Query(*database.Session)

		if err := updateBotUserQuery.Bind(updatedSpaces, time.Now(), req.BotID).Exec(); err != nil {
			log.Printf("Error updating bot user's spaces list: %v", err)
		}
	}

	// Publish space member remove event
	if err := events.PublishSpaceMemberLeaveEvent(spaceID, req.BotID, ""); err != nil {
		log.Printf("Error publishing space member remove event: %v", err)
	}

	return c.Status(200).JSON(fiber.Map{
		"message": "Bot removed from space successfully",
	})
}

// GetSpaceBots handles GET /spaces/:id/bots
func GetSpaceBots(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	userID := user.ID
	spaceIDStr := c.Params("id")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	membershipService := services.NewSpaceMembershipService(database.Session)
	if !membershipService.IsSpaceMember(spaceID, userID) {
		return c.Status(403).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Get all space members
	var spaceMembers []models.SpaceMember
	spaceMembersQuery := qb.Select(models.SpaceMemberTable.Name()).
		Where(qb.Eq("space_id")).
		Query(*database.Session)

	if err := spaceMembersQuery.Bind(spaceID).Select(&spaceMembers); err != nil {
		log.Printf("Error fetching space members: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch space members",
		})
	}

	// Filter for bot members and get their details
	botUserIDs := []string{}
	for _, member := range spaceMembers {
		// Check if this member is a bot
		var user models.User
		userQuery := qb.Select(models.UserTable.Name()).
			Columns("bot").
			Where(qb.Eq("id")).
			Query(*database.Session)

		if err := userQuery.Bind(member.UserID).Get(&user); err == nil && user.Bot {
			botUserIDs = append(botUserIDs, member.UserID)
		}
	}

	if len(botUserIDs) == 0 {
		return c.JSON([]BotResponse{})
	}

	// Get bot details
	var bots []models.Bot
	botsQuery := qb.Select(models.BotTable.Name()).
		Where(qb.In("user_id")).
		Query(*database.Session)

	if err := botsQuery.Bind(botUserIDs).Select(&bots); err != nil {
		log.Printf("Error fetching bots: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to fetch bots",
		})
	}

	// Get bot user details
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