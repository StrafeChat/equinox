package spaces

import (
	"log"
	"strconv"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

// GetUserSpaces returns all spaces that the authenticated user is a member of
func GetUserSpaces(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)

	// Get user's spaces from space_members_by_user table
	var spaceMemberships []models.SpaceMembersByUser
	query := models.SpaceMembersByUserTable.SelectBuilder().Where(qb.Eq("user_id")).Query(*database.Session).Bind(user.ID)
	if err := query.SelectRelease(&spaceMemberships); err != nil {
		log.Printf("Failed to get user spaces: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get user spaces",
		})
	}

	// Get space details for each space
	var spaces []types.Space
	for _, membership := range spaceMemberships {
		var space models.Space
		spaceQuery := models.SpaceTable.SelectBuilder().Where(qb.Eq("id")).Query(*database.Session).Bind(membership.SpaceID)
		if err := spaceQuery.GetRelease(&space); err != nil {
			log.Printf("Failed to get space details for space %d: %v", membership.SpaceID, err)
			continue // Skip this space if we can't get its details
		}

		// Convert to response type
		spaces = append(spaces, types.Space{
			ID:                          space.ID,
			Name:                        space.Name,
			Description:                 space.Description,
			Icon:                        space.Icon,
			Banner:                      space.Banner,
			OwnerID:                     space.OwnerID,
			VerificationLevel:           space.VerificationLevel,
			DefaultMessageNotifications: space.DefaultMessageNotifications,
			ExplicitContentFilter:       space.ExplicitContentFilter,
			Features:                    space.Features,
			AfkRoomID:                   space.AfkRoomID,
			AfkTimeout:                  space.AfkTimeout,
			SystemRoomID:                space.SystemRoomID,
			SystemRoomFlags:             space.SystemRoomFlags,
			RulesRoomID:                 space.RulesRoomID,
			MaxPresences:                space.MaxPresences,
			MaxMembers:                  space.MaxMembers,
			VanityUrlCode:               space.VanityUrlCode,
			PreferredLocale:             space.PreferredLocale,
			PublicUpdatesRoomID:         space.PublicUpdatesRoomID,
			MaxVideoRoomUsers:           space.MaxVideoRoomUsers,
			NsfwLevel:                   space.NsfwLevel,
			CreatedAt:                   space.CreatedAt,
			UpdatedAt:                   space.UpdatedAt,
		})
	}

	return c.JSON(spaces)
}

// GetSpace returns details for a specific space
func GetSpace(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")

	if spaceIDStr == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Space ID is required",
		})
	}

	// Convert string ID to int64
	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID format",
		})
	}

	// Check if user is a member of this space
	var spaceMember models.SpaceMember
	memberQuery := models.SpaceMemberTable.SelectBuilder().Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(*database.Session).Bind(spaceID, user.ID)
	if err := memberQuery.GetRelease(&spaceMember); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Get space details
	var space models.Space
	spaceQuery := models.SpaceTable.SelectBuilder().Where(qb.Eq("id")).Query(*database.Session).Bind(spaceID)
	if err := spaceQuery.GetRelease(&space); err != nil {
		log.Printf("Failed to get space details: %v", err)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Space not found",
		})
	}

	// Return space details
	return c.JSON(types.Space{
		ID:                          space.ID,
		Name:                        space.Name,
		Description:                 space.Description,
		Icon:                        space.Icon,
		Banner:                      space.Banner,
		OwnerID:                     space.OwnerID,
		VerificationLevel:           space.VerificationLevel,
		DefaultMessageNotifications: space.DefaultMessageNotifications,
		ExplicitContentFilter:       space.ExplicitContentFilter,
		Features:                    space.Features,
		AfkRoomID:                   space.AfkRoomID,
		AfkTimeout:                  space.AfkTimeout,
		SystemRoomID:                space.SystemRoomID,
		SystemRoomFlags:             space.SystemRoomFlags,
		RulesRoomID:                 space.RulesRoomID,
		MaxPresences:                space.MaxPresences,
		MaxMembers:                  space.MaxMembers,
		VanityUrlCode:               space.VanityUrlCode,
		PreferredLocale:             space.PreferredLocale,
		PublicUpdatesRoomID:         space.PublicUpdatesRoomID,
		MaxVideoRoomUsers:           space.MaxVideoRoomUsers,
		NsfwLevel:                   space.NsfwLevel,
		CreatedAt:                   space.CreatedAt,
		UpdatedAt:                   space.UpdatedAt,
	})
}
