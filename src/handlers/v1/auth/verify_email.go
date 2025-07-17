package handlers_v1

import (
	"errors"
	"log"
	"time"

	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
)

func VerifyEmailGet(c fiber.Ctx) error {
	token := c.Query("token")
	if token == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Verification token is required",
		})
	}

	// Get verification token from database
	verificationTokenQuery := models.VerificationTokenTable.SelectBuilder().
		Columns("verification_token", "user_id", "email", "expires_at").
		Where(qb.Eq("verification_token")).
		Limit(1)

	q := verificationTokenQuery.Query(*database.Session).BindMap(qb.M{
		"verification_token": token,
	})

	var verificationToken models.VerificationToken
	err := q.GetRelease(&verificationToken)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid or expired verification token",
			})
		}
		log.Printf("Error retrieving verification token: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while verifying email",
		})
	}

	// Check if token is expired
	if time.Now().After(verificationToken.ExpiresAt) {
		// Delete expired token
		deleteTokenQuery := models.VerificationTokenTable.DeleteBuilder().
			Where(qb.Eq("verification_token")).
			Query(*database.Session).
			BindMap(qb.M{"verification_token": token})
		deleteTokenQuery.ExecRelease()

		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Verification token has expired",
		})
	}

	// Update user's verified_email status
	updateUserQuery := models.UserTable.UpdateBuilder().
		Set("verified_email", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindMap(qb.M{
			"id":             verificationToken.UserID,
			"verified_email": true,
			"updated_at":     time.Now(),
		})

	if err := updateUserQuery.ExecRelease(); err != nil {
		log.Printf("Error updating user verification status: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while verifying email",
		})
	}

	// Delete the verification token after successful verification
	deleteTokenQuery := models.VerificationTokenTable.DeleteBuilder().
		Where(qb.Eq("verification_token")).
		Query(*database.Session).
		BindMap(qb.M{"verification_token": token})

	if err := deleteTokenQuery.ExecRelease(); err != nil {
		log.Printf("Error deleting verification token: %v", err)
		// Don't return error here as the main operation succeeded
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Email verified successfully",
	})
}
