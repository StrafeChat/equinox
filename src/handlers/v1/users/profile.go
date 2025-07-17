package handlers_v1

import (
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

type ProfileUpdateRequest struct {
	Bio     *string `json:"bio"`
	AboutMe *string `json:"about_me"`
}

func UpdateProfile(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)

	var req ProfileUpdateRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Update user's bio and about_me if provided
	if req.Bio != nil {
		user.Bio = req.Bio
	}
	if req.AboutMe != nil {
		user.AboutMe = req.AboutMe
	}
	user.UpdatedAt = time.Now()

	// Update in database
	stmt := models.UserTable.UpdateBuilder().
		Set("bio", "about_me", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindStruct(&user)

	if err := stmt.ExecRelease(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to update profile",
		})
	}

	return c.Status(fiber.StatusOK).JSON(user)
}
