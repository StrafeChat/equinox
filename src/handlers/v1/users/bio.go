package handlers_v1

import (
	"log"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type UpdateBioPayload struct {
	Bio string `json:"bio"`
}

func UpdateBio(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)

	var payload UpdateBioPayload
	if err := c.Bind().JSON(&payload); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	// Validate bio length
	if len(payload.Bio) > 190 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Bio cannot be longer than 190 characters"})
	}

	stmt := models.UserTable.UpdateBuilder().
		Set("bio").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"bio": payload.Bio,
			"id":  user.ID,
		})

	if err := stmt.ExecRelease(); err != nil {
		log.Printf("Failed to update user bio: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to update bio"})
	}

	// Invalidate user cache if necessary
	// Cache.Delete("user:" + user.ID)

	// Fetch updated user to return
	var updatedUser models.User
	selectStmt := models.UserTable.SelectBuilder().Where(qb.Eq("id")).Query(*database.Session)
	if err := selectStmt.BindMap(qb.M{"id": user.ID}).Get(&updatedUser); err != nil {
		log.Printf("Failed to fetch updated user: %v", err)
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Bio updated, but failed to fetch updated user data"})
	}

	return c.Status(fiber.StatusOK).JSON(updatedUser)
} 