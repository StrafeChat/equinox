package spaces

import (
	"fmt"
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/events"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/repository"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type CreateSpaceInput struct {
	Name        string  `json:"name" validate:"required,min=2,max=100"`
	Description *string `json:"description,omitempty" validate:"omitempty,max=1024"`
	Icon        *string `json:"icon,omitempty"`
}

// createDefaultRoomsAndSections creates the default rooms and sections for a new space
func createDefaultRoomsAndSections(spaceID int64, userID string) error {
	now := time.Now()

	// Create Text Rooms section
	textRoomsSection := models.Room{
		ID:         helpers.GenerateRoomID().String(),
		Creator:    &userID,
		Recipients: []string{},
		Type:       types.RoomTypeSpaceSection,
		SpaceID:    &spaceID,
		Name:       helpers.StringPtr("Text Rooms"),
		Topic:      helpers.StringPtr("Text rooms section"),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Create Voice Rooms section
	voiceRoomsSection := models.Room{
		ID:         helpers.GenerateRoomID().String(),
		Creator:    &userID,
		Recipients: []string{},
		Type:       types.RoomTypeSpaceSection,
		SpaceID:    &spaceID,
		Name:       helpers.StringPtr("Voice Rooms"),
		Topic:      helpers.StringPtr("Voice rooms section"),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Create General text room
	generalTextRoom := models.Room{
		ID:         helpers.GenerateRoomID().String(),
		Creator:    &userID,
		Recipients: []string{},
		Type:       types.RoomTypeTextRoom,
		SpaceID:    &spaceID,
		ParentID:   &textRoomsSection.ID,
		Name:       helpers.StringPtr("General"),
		Topic:      helpers.StringPtr("General text room"),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Create General voice room
	generalVoiceRoom := models.Room{
		ID:         helpers.GenerateRoomID().String(),
		Creator:    &userID,
		Recipients: []string{},
		Type:       types.RoomTypeVoiceRoom,
		SpaceID:    &spaceID,
		ParentID:   &voiceRoomsSection.ID,
		Name:       helpers.StringPtr("General"),
		Topic:      helpers.StringPtr("General voice room"),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Insert all rooms
	rooms := []models.Room{textRoomsSection, voiceRoomsSection, generalTextRoom, generalVoiceRoom}
	for _, room := range rooms {
		query := models.RoomTable.InsertBuilder().Query(*database.Session).BindStruct(room)
		if err := query.ExecRelease(); err != nil {
			log.Printf("Failed to create default room %s: %v", *room.Name, err)
			return err
		}
	}

	return nil
}

func CreateSpace(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)

	body := new(CreateSpaceInput)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Validate input
	if body.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Space name is required",
		})
	}

	if len(body.Name) < 2 || len(body.Name) > 100 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Space name must be between 2 and 100 characters",
		})
	}

	if body.Description != nil && len(*body.Description) > 1024 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Space description must be less than 1024 characters",
		})
	}

	// Generate space ID
	spaceID := helpers.GenerateSpaceID().Int64()
	now := time.Now()

	// Generate name acronym
	nameAcronym := helpers.GenerateNameAcronym(body.Name)

	// Create space
	space := models.Space{
		ID:                          spaceID,
		Name:                        body.Name,
		NameAcronym:                 nameAcronym,
		Description:                 body.Description,
		Icon:                        body.Icon,
		OwnerID:                     user.ID,
		VerificationLevel:           0, // None
		DefaultMessageNotifications: 0, // All messages
		ExplicitContentFilter:       0, // Disabled
		Features:                    []string{},
		AfkTimeout:                  300, // 5 minutes
		SystemRoomFlags:             0,
		PreferredLocale:             "en-US",
		NsfwLevel:                   0, // Default
		CreatedAt:                   now,
		UpdatedAt:                   now,
	}

	// Insert space into database
	query := models.SpaceTable.InsertBuilder().Query(*database.Session).BindStruct(space)
	if err := query.ExecRelease(); err != nil {
		log.Printf("Failed to create space: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create space",
		})
	}

	// Add owner as space member
	spaceMember := models.SpaceMember{
		SpaceID:  spaceID,
		UserID:   user.ID,
		JoinedAt: now,
		Deaf:     false,
		Mute:     false,
		Flags:    0,
		Pending:  false,
	}

	// Insert space member
	memberQuery := models.SpaceMemberTable.InsertBuilder().Query(*database.Session).BindStruct(spaceMember)
	if err := memberQuery.ExecRelease(); err != nil {
		log.Printf("Failed to add owner as space member: %v", err)
		// Try to clean up the space
		deleteQuery := qb.Delete("spaces").Where(qb.Eq("id")).Query(*database.Session).Bind(spaceID)
		deleteQuery.ExecRelease()
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create space membership",
		})
	}

	// Add to space_members_by_user table
	spaceMemberByUser := models.SpaceMembersByUser{
		UserID:   user.ID,
		SpaceID:  spaceID,
		JoinedAt: now,
	}

	memberByUserQuery := models.SpaceMembersByUserTable.InsertBuilder().Query(*database.Session).BindStruct(spaceMemberByUser)
	if err := memberByUserQuery.ExecRelease(); err != nil {
		log.Printf("Failed to add space to user's space list: %v", err)
		// Continue anyway, this is not critical
	}

	// Update user's spaces array
	updatedSpaces := append(user.Spaces, spaceID)
	updateUserQuery := qb.Update("users").Set("spaces", "updated_at").Where(qb.Eq("id")).Query(*database.Session).Bind(updatedSpaces, now, user.ID)
	if err := updateUserQuery.ExecRelease(); err != nil {
		log.Printf("Failed to update user's spaces array: %v", err)
		// Continue anyway, this is not critical
	}

	// Create default @everyone role
	spaceRolesRepo := repository.NewSpaceRolesRepository(database.Session)
	if err := spaceRolesRepo.CreateDefaultEveryoneRole(spaceID); err != nil {
		log.Printf("Failed to create default @everyone role: %v", err)
		// Continue anyway, this is not critical for space creation
	}

	// Create default rooms and sections
	if err := createDefaultRoomsAndSections(spaceID, user.ID); err != nil {
		log.Printf("Failed to create default rooms and sections: %v", err)
		// Continue anyway, this is not critical for space creation
	}

	// Publish space creation event to Redis for Stargate to broadcast
	spaceEventData := map[string]interface{}{
		"id":                            fmt.Sprintf("%d", space.ID),
		"name":                          space.Name,
		"name_acronym":                  space.NameAcronym,
		"description":                   space.Description,
		"icon":                          space.Icon,
		"banner":                        space.Banner,
		"owner_id":                      space.OwnerID,
		"verification_level":            space.VerificationLevel,
		"default_message_notifications": space.DefaultMessageNotifications,
		"explicit_content_filter":       space.ExplicitContentFilter,
		"features":                      space.Features,
		"afk_room_id":                   space.AfkRoomID,
		"afk_timeout":                   space.AfkTimeout,
		"system_room_id":                space.SystemRoomID,
		"system_room_flags":             space.SystemRoomFlags,
		"rules_room_id":                 space.RulesRoomID,
		"max_presences":                 space.MaxPresences,
		"max_members":                   space.MaxMembers,
		"vanity_url_code":               space.VanityUrlCode,
		"preferred_locale":              space.PreferredLocale,
		"public_updates_room_id":        space.PublicUpdatesRoomID,
		"max_video_room_users":          space.MaxVideoRoomUsers,
		"nsfw_level":                    space.NsfwLevel,
		"created_at":                    space.CreatedAt,
		"updated_at":                    space.UpdatedAt,
	}

	if err := events.PublishSpaceCreateEvent(spaceEventData); err != nil {
		log.Printf("Failed to publish space create event: %v", err)
		// Continue anyway, this is not critical for the API response
	}

	// Return the created space
	return c.Status(fiber.StatusCreated).JSON(types.Space{
		ID:                          space.ID,
		Name:                        space.Name,
		NameAcronym:                 space.NameAcronym,
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
