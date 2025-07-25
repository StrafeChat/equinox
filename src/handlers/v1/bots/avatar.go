package handlers_v1

import (
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

type UpdateAvatarRequest struct {
	Avatar string `json:"avatar"`
}

func UpdateBotAvatar(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	botID := c.Params("id")

	// Parse request body
	var req UpdateAvatarRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	if req.Avatar == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Avatar ID is required",
		})
	}

	// Check if bot exists and user owns it
	var bot models.Bot
	botQuery := qb.Select("bots").Where(qb.Eq("user_id")).Query(*database.Session)
	if err := botQuery.Bind(botID).Get(&bot); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Bot not found",
		})
	}

	// Check if user owns the bot
	if bot.OwnerID != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't own this bot",
		})
	}

	// Update bot user avatar in users table with the provided avatar ID
	userUpdateQuery := qb.Update("users").
		Set("avatar", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session)

	if err := userUpdateQuery.Bind(req.Avatar, time.Now(), botID).Exec(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to update bot avatar in database",
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Bot avatar updated successfully",
		"avatar":  req.Avatar,
	})
}