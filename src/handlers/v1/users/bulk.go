package handlers_v1

import (
	"fmt"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

// BulkUserRequest defines the structure for the bulk user request body
type BulkUserRequest struct {
	IDs []string `json:"ids"`
}

func BulkGetUsers(c fiber.Ctx) error {
	body := new(BulkUserRequest)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
		})
	}
	fmt.Println("AHH")
	userIds := body.IDs

	if len(userIds) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "No user IDs provided",
		})
	}

	// Limit the number of users that can be fetched at once
	if len(userIds) > 100 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Too many user IDs. Maximum is 100.",
		})
	}

	users := make(map[string]interface{})
	for _, id := range userIds {
		var user models.User
		userById := models.UserTable.SelectBuilder().
			Columns("id", "username", "discriminator", "display_name", "avatar", "banner", "presence").
			Where(qb.Eq("id")).
			Limit(1)

		userByIdQuery := userById.Query(*database.Session).
			BindStruct(models.User{
				ID: id,
			})

		if err := userByIdQuery.GetRelease(&user); err == nil {
			users[id] = fiber.Map{
				"Username":      user.Username,
				"Discriminator": user.Discriminator,
				"DisplayName":   user.DisplayName,
				"Avatar":        user.Avatar,
				"Banner":        user.Banner,
				"Presence":      user.Presence,
			}
		}
	}

	return c.JSON(fiber.Map{
		"users": users,
	})
}
