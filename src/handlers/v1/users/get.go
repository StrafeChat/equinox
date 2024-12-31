package handlers_v1

import (
	"errors"
	"log"
	"strconv"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

func GetUser(c fiber.Ctx) error {
	userId := c.Params("id")
	if userId == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "User ID is required",
		})
	}

	userIdInt, err := strconv.ParseInt(userId, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid user ID format",
		})
	}

	userById := models.UserTable.SelectBuilder().
		Columns("id", "username", "discriminator", "display_name", "avatar", "banner", "bot", "system", "bio", "flags", "about_me", "accent_color", "presence").
		Where(qb.Eq("id")).
		Limit(1)

	var user models.User
	userByIdQuery := userById.Query(*database.Session).
		BindStruct(models.User{
			ID: strconv.FormatInt(userIdInt, 10),
		})

	if err := userByIdQuery.GetRelease(&user); err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": "User not found",
			})
		}
		log.Printf("Error fetching user details: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to fetch user details",
		})
	}

	response := helpers.GetUserResponseFormat(user)
	
	return c.Status(fiber.StatusOK).JSON(response)
}
