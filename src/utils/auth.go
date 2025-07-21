package utils

import (
	"errors"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
)

// GetUserFromContext extracts the authenticated user from the fiber context
// This function expects the user to be set by the VerifyAuth middleware
func GetUserFromContext(c fiber.Ctx) (*models.User, error) {
	userInterface := c.Locals("user")
	if userInterface == nil {
		return nil, errors.New("user not found in context")
	}

	user, ok := userInterface.(models.User)
	if !ok {
		return nil, errors.New("invalid user type in context")
	}

	return &user, nil
}