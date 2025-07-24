package spaces

import (
	"log"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/events"
	"github.com/StrafeChat/equinox/src/repository"
	"github.com/StrafeChat/equinox/src/services"
	"github.com/gofiber/fiber/v3"
)

type UpdateSpaceRequest struct {
	Name        *string `json:"name,omitempty" validate:"omitempty,min=2,max=100"`
	Description *string `json:"description,omitempty" validate:"omitempty,max=1024"`
	NameAcronym *string `json:"name_acronym,omitempty" validate:"omitempty,min=1,max=10"`
	Icon        *string `json:"icon,omitempty"`
	Banner      *string `json:"banner,omitempty"`
}

func UpdateSpace(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is the space owner
	membershipService := services.NewSpaceMembershipService(database.Session)
	if !membershipService.IsSpaceOwner(spaceID, user.ID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "Only space owners can update space settings",
		})
	}

	// Parse request body
	body := new(UpdateSpaceRequest)
	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Validate request
	if body.Name != nil && (len(*body.Name) < 2 || len(*body.Name) > 100) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Name must be between 2 and 100 characters",
		})
	}
	if body.Description != nil && len(*body.Description) > 1024 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Description must be less than 1024 characters",
		})
	}
	if body.NameAcronym != nil && (len(*body.NameAcronym) < 1 || len(*body.NameAcronym) > 10) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Name acronym must be between 1 and 10 characters",
		})
	}

	// Build updates map
	updates := make(map[string]interface{})
	if body.Name != nil {
		updates["name"] = *body.Name
	}
	if body.Description != nil {
		updates["description"] = *body.Description
	}
	if body.NameAcronym != nil {
		updates["name_acronym"] = *body.NameAcronym
	}
	if body.Icon != nil {
		updates["icon"] = *body.Icon
	}
	if body.Banner != nil {
		updates["banner"] = *body.Banner
	}

	if len(updates) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "No fields to update",
		})
	}

	// Add updated_at timestamp
	updates["updated_at"] = time.Now().Unix()

	// Update space in database
	spaceRepo := repository.NewSpaceRepository(database.Session)
	if err := spaceRepo.UpdateSpace(spaceID, updates); err != nil {
		log.Printf("Error updating space %d: %v", spaceID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to update space",
		})
	}

	// Get updated space for response
	updatedSpace, err := spaceRepo.GetSpace(spaceID)
	if err != nil {
		log.Printf("Error getting updated space %d: %v", spaceID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get updated space",
		})
	}

	// Publish space update event for real-time updates
	spaceUpdateEvent := events.SpaceUpdateEvent{
		Type:      "SPACE_UPDATED",
		SpaceID:   spaceID,
		Updates:   updates,
		UpdatedBy: user.ID,
		Timestamp: time.Now().Unix(),
	}

	if err := events.PublishSpaceUpdate(spaceUpdateEvent); err != nil {
		log.Printf("Failed to publish space update event: %v", err)
		// Don't fail the request if event publishing fails
	}

	return c.JSON(fiber.Map{
		"message": "Space updated successfully",
		"space":   updatedSpace,
	})
}
