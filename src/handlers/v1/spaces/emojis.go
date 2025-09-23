package spaces

import (
	"log"
	"regexp"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/services"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

// CreateCustomEmojiInput represents the input for creating a custom emoji
type CreateCustomEmojiInput struct {
	Shortcode string `json:"shortcode" validate:"required,min=2,max=32"`
	FileID    string `json:"file_id" validate:"required"`
	Name      string `json:"name" validate:"required,min=1,max=64"`
}

// CustomEmojiResponse represents the response format for custom emoji
type CustomEmojiResponse struct {
	ID        string `json:"id"`
	SpaceID   string `json:"space_id"`
	Shortcode string `json:"shortcode"`
	FileID    string `json:"file_id"`
	Name      string `json:"name"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// validateShortcode validates custom emoji shortcode format
func validateShortcode(shortcode string) bool {
	// Must be alphanumeric and underscores only, 2-32 characters
	re := regexp.MustCompile(`^[a-zA-Z0-9_]{2,32}$`)
	return re.MatchString(shortcode)
}

// GetSpaceEmojis handles GET /spaces/:id/emojis
func GetSpaceEmojis(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")

	// Validate space ID format (should be numeric string)
	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	membershipService := services.NewSpaceMembershipService(database.Session)
	if !membershipService.IsSpaceMember(spaceID, user.ID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Get custom emojis for the space using string space ID
	var customEmojis []models.CustomEmojiBySpace
	query := models.CustomEmojiBySpaceTable.SelectBuilder().Where(qb.Eq("space_id")).Query(*database.Session)
	if err := query.BindMap(qb.M{"space_id": spaceIDStr}).SelectRelease(&customEmojis); err != nil {
		log.Printf("Failed to get custom emojis for space %s: %v", spaceIDStr, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get custom emojis",
		})
	}

	// Convert to response format
	responses := make([]CustomEmojiResponse, len(customEmojis))
	for i, emoji := range customEmojis {
		responses[i] = CustomEmojiResponse{
			ID:        emoji.ID,
			SpaceID:   emoji.SpaceID,
			Shortcode: emoji.Shortcode,
			FileID:    emoji.FileID,
			Name:      emoji.Name,
			CreatedBy: emoji.CreatedBy,
			CreatedAt: emoji.CreatedAt,
			UpdatedAt: emoji.CreatedAt, // CustomEmojiBySpace doesn't have UpdatedAt
		}
	}

	return c.JSON(fiber.Map{
		"emojis": responses,
	})
}

// CreateCustomEmoji handles POST /spaces/:id/emojis
func CreateCustomEmoji(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")

	// Validate space ID format (should be numeric string)
	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	membershipService := services.NewSpaceMembershipService(database.Session)
	if !membershipService.IsSpaceMember(spaceID, user.ID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Check if user has permission to manage emojis
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, user.ID, spaceIDStr, utils.MANAGE_EMOJIS)
	if err != nil {
		log.Printf("Error checking MANAGE_EMOJIS permission: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage emojis",
		})
	}

	// Parse request body
	body := new(CreateCustomEmojiInput)
	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Validate input
	if body.Shortcode == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Shortcode is required",
		})
	}

	if !validateShortcode(body.Shortcode) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Shortcode must be 2-32 characters and contain only letters, numbers, and underscores",
		})
	}

	if body.FileID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "File ID is required",
		})
	}

	if body.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Name is required",
		})
	}

	if len(body.Name) > 64 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Name must be less than 64 characters",
		})
	}

	// Check if shortcode already exists in this space by querying the by_space table
	var existingEmojis []models.CustomEmojiBySpace
	checkQuery := models.CustomEmojiBySpaceTable.SelectBuilder().Where(qb.Eq("space_id")).Query(*database.Session)
	if err := checkQuery.BindMap(qb.M{"space_id": spaceIDStr}).SelectRelease(&existingEmojis); err == nil {
		// Check if any emoji has the same shortcode
		for _, emoji := range existingEmojis {
			if emoji.Shortcode == body.Shortcode {
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{
					"error": "An emoji with this shortcode already exists in this space",
				})
			}
		}
	}

	// Generate emoji ID and timestamps
	emojiID := helpers.GenerateMessageID().String()
	now := time.Now().Format(time.RFC3339)

	// Create custom emoji
	customEmoji := models.CustomEmoji{
		ID:        emojiID,
		SpaceID:   spaceIDStr,
		Shortcode: body.Shortcode,
		FileID:    body.FileID,
		Name:      body.Name,
		CreatedBy: user.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Insert into custom_emojis table
	insertQuery := models.CustomEmojiTable.InsertBuilder().Query(*database.Session).BindStruct(customEmoji)
	if err := insertQuery.ExecRelease(); err != nil {
		log.Printf("Failed to create custom emoji: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create custom emoji",
		})
	}

	// Insert into custom_emojis_by_space table for efficient listing
	customEmojiBySpace := models.CustomEmojiBySpace{
		SpaceID:   spaceIDStr,
		CreatedAt: now,
		ID:        emojiID,
		Shortcode: body.Shortcode,
		FileID:    body.FileID,
		Name:      body.Name,
		CreatedBy: user.ID,
	}

	insertBySpaceQuery := models.CustomEmojiBySpaceTable.InsertBuilder().Query(*database.Session).BindStruct(customEmojiBySpace)
	if err := insertBySpaceQuery.ExecRelease(); err != nil {
		log.Printf("Failed to create custom emoji by space: %v", err)
		// Try to clean up the main table entry
		deleteQuery := models.CustomEmojiTable.DeleteBuilder().Where(qb.Eq("id")).Query(*database.Session)
		deleteQuery.BindMap(qb.M{"id": emojiID}).ExecRelease()
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create custom emoji",
		})
	}

	// Return the created emoji
	response := CustomEmojiResponse{
		ID:        customEmoji.ID,
		SpaceID:   customEmoji.SpaceID,
		Shortcode: customEmoji.Shortcode,
		FileID:    customEmoji.FileID,
		Name:      customEmoji.Name,
		CreatedBy: customEmoji.CreatedBy,
		CreatedAt: customEmoji.CreatedAt,
		UpdatedAt: customEmoji.UpdatedAt,
	}

	return c.Status(fiber.StatusCreated).JSON(response)
}

// GetCustomEmoji handles GET /spaces/:id/emojis/:shortcode
func GetCustomEmoji(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")
	shortcode := c.Params("shortcode")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	membershipService := services.NewSpaceMembershipService(database.Session)
	if !membershipService.IsSpaceMember(spaceID, user.ID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Get the custom emoji by searching through space emojis
	var customEmojis []models.CustomEmojiBySpace
	query := models.CustomEmojiBySpaceTable.SelectBuilder().Where(qb.Eq("space_id")).Query(*database.Session)
	if err := query.BindMap(qb.M{"space_id": spaceIDStr}).SelectRelease(&customEmojis); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get custom emojis",
		})
	}

	// Find the emoji with matching shortcode
	var foundEmoji *models.CustomEmojiBySpace
	for _, emoji := range customEmojis {
		if emoji.Shortcode == shortcode {
			foundEmoji = &emoji
			break
		}
	}

	if foundEmoji == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Custom emoji not found",
		})
	}

	// Return the emoji
	response := CustomEmojiResponse{
		ID:        foundEmoji.ID,
		SpaceID:   foundEmoji.SpaceID,
		Shortcode: foundEmoji.Shortcode,
		FileID:    foundEmoji.FileID,
		Name:      foundEmoji.Name,
		CreatedBy: foundEmoji.CreatedBy,
		CreatedAt: foundEmoji.CreatedAt,
		UpdatedAt: foundEmoji.CreatedAt, // CustomEmojiBySpace doesn't have UpdatedAt
	}

	return c.JSON(response)
}

// DeleteCustomEmoji handles DELETE /spaces/:id/emojis/:shortcode
func DeleteCustomEmoji(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")
	shortcode := c.Params("shortcode")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	membershipService := services.NewSpaceMembershipService(database.Session)
	if !membershipService.IsSpaceMember(spaceID, user.ID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Check if user has permission to manage emojis
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, user.ID, spaceIDStr, utils.MANAGE_EMOJIS)
	if err != nil {
		log.Printf("Error checking MANAGE_EMOJIS permission: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage emojis",
		})
	}

	// Check if the emoji exists by searching through space emojis
	var customEmojis []models.CustomEmojiBySpace
	checkQuery := models.CustomEmojiBySpaceTable.SelectBuilder().Where(qb.Eq("space_id")).Query(*database.Session)
	if err := checkQuery.BindMap(qb.M{"space_id": spaceIDStr}).SelectRelease(&customEmojis); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get custom emojis",
		})
	}

	// Find the emoji with matching shortcode
	var foundEmoji *models.CustomEmojiBySpace
	for _, emoji := range customEmojis {
		if emoji.Shortcode == shortcode {
			foundEmoji = &emoji
			break
		}
	}

	if foundEmoji == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Custom emoji not found",
		})
	}

	// Delete from custom_emojis table
	deleteQuery := models.CustomEmojiTable.DeleteBuilder().Where(qb.Eq("id")).Query(*database.Session)
	if err := deleteQuery.BindMap(qb.M{"id": foundEmoji.ID}).ExecRelease(); err != nil {
		log.Printf("Failed to delete custom emoji: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to delete custom emoji",
		})
	}

	// Delete from custom_emojis_by_space table
	deleteBySpaceQuery := models.CustomEmojiBySpaceTable.DeleteBuilder().Where(qb.Eq("space_id"), qb.Eq("created_at"), qb.Eq("id")).Query(*database.Session)
	if err := deleteBySpaceQuery.BindMap(qb.M{"space_id": spaceIDStr, "created_at": foundEmoji.CreatedAt, "id": foundEmoji.ID}).ExecRelease(); err != nil {
		log.Printf("Failed to delete custom emoji from by_space table: %v", err)
		// Don't fail the request if this cleanup fails
	}

	return c.JSON(fiber.Map{
		"message": "Custom emoji deleted successfully",
	})
}
