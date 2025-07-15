package handlers_v1

import (
	"errors"
	"log"
	"os"
	"strings"

	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/services"
)

func ResendVerificationPost(c fiber.Ctx) error {
	// Check if email verification is enabled
	emailVerificationEnabled := strings.ToLower(os.Getenv("ENABLE_EMAIL_VERIFICATION")) == "true"
	if !emailVerificationEnabled {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Email verification is not enabled",
		})
	}

	body := new(struct {
		Email string `json:"email"`
	})

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid JSON format",
		})
	}

	if body.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Email is required",
		})
	}

	// Find user by email
	userByEmailQuery := models.UserByEmailTable.SelectBuilder().
		Columns("id").
		Where(qb.Eq("email")).
		Limit(1)

	q := userByEmailQuery.Query(*database.Session).BindMap(qb.M{
		"email": body.Email,
	})

	var emailUser models.UserByEmail
	err := q.GetRelease(&emailUser)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": "No account found with this email address",
			})
		}
		log.Printf("Error finding user by email: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while processing your request",
		})
	}

	// Get user details to check verification status
	userQuery := models.UserTable.SelectBuilder().
		Columns("id", "username", "verified_email").
		Where(qb.Eq("id")).
		Limit(1)

	userQ := userQuery.Query(*database.Session).BindMap(qb.M{
		"id": emailUser.ID,
	})

	var user models.User
	err = userQ.GetRelease(&user)
	if err != nil {
		log.Printf("Error retrieving user details: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while processing your request",
		})
	}

	// Check if user is already verified
	if user.VerifiedEmail {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "This email address is already verified",
		})
	}

	// Delete any existing verification tokens for this user
	deleteTokenQuery := models.VerificationTokenTable.DeleteBuilder().
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{"user_id": user.ID})
	deleteTokenQuery.ExecRelease() // Ignore errors here

	// Send new verification email
	emailService := services.NewEmailService()
	if err := emailService.SendVerificationEmail(user.ID, body.Email, user.Username); err != nil {
		log.Printf("Error sending verification email: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while sending verification email",
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Verification email sent successfully. Please check your email.",
	})
}
