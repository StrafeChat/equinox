package handlers_v1

import (
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type BannerUpdateRequest struct {
	Banner string `json:"banner"`
}

func UpdateBanner(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)

	var req BannerUpdateRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Update user's banner
	user.Banner = &req.Banner
	user.UpdatedAt = time.Now()

	// Update in database
	stmt := models.UserTable.UpdateBuilder().
		Set("banner", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindStruct(&user)

	if err := stmt.ExecRelease(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to update banner",
		})
	}

	return c.Status(fiber.StatusOK).JSON(user)
}
