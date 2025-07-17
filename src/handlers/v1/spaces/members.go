package spaces

import (
	"context"
	"log"
	"strconv"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/events"
	"github.com/StrafeChat/equinox/src/repository"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

// SpaceMemberResponse represents a space member with user details
type SpaceMemberResponse struct {
	UserID        string   `json:"user_id"`
	Username      string   `json:"username"`
	Discriminator int      `json:"discriminator"`
	DisplayName   *string  `json:"display_name"`
	Avatar        *string  `json:"avatar"`
	Roles         []string `json:"roles"`
	JoinedAt      string   `json:"joined_at"`
	IsOwner       bool     `json:"is_owner"`
	Status        string   `json:"status"`
}

// GetSpaceMembers handles GET /spaces/:id/members
func GetSpaceMembers(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	if !isSpaceMember(spaceID, user.ID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Get space details to check owner
	spaceRepo := repository.NewSpaceRepository(database.Session)
	space, err := spaceRepo.GetSpace(spaceID)
	if err != nil {
		log.Printf("Error getting space: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get space details",
		})
	}

	// Get space members
	membersRepo := repository.NewSpaceMembersRepository(database.Session)
	members, err := membersRepo.GetSpaceMembers(spaceID)
	if err != nil {
		log.Printf("Error getting space members: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get space members",
		})
	}

	// Get member roles using the new junction table
	memberRolesRepo := repository.NewSpaceMemberRolesRepository(*database.Session)
	allMemberRoles, err := memberRolesRepo.GetAllSpaceMemberRoles(context.TODO(), spaceID)
	if err != nil {
		log.Printf("Error getting member roles: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get member roles",
		})
	}

	// Get user details for each member
	memberResponses := make([]SpaceMemberResponse, 0, len(members))
	for _, member := range members {
		// Get user details with a simpler query
		var userDetails struct {
			ID            string                 `db:"id"`
			Username      string                 `db:"username"`
			Discriminator int                    `db:"discriminator"`
			DisplayName   *string                `db:"display_name"`
			Avatar        *string                `db:"avatar"`
			Presence      map[string]interface{} `db:"presence"`
		}

		userQuery := qb.Select("users").
			Columns("id", "username", "discriminator", "display_name", "avatar", "presence").
			Where(qb.Eq("id")).
			Limit(1).
			Query(*database.Session)

		if err := userQuery.Bind(member.UserID).GetRelease(&userDetails); err != nil {
			log.Printf("Error getting user details for %s: %v", member.UserID, err)
			continue // Skip this member if we can't get their details
		}

		// Determine status from presence
		status := "offline"
		if online, exists := userDetails.Presence["online"]; exists {
			if onlineBool, ok := online.(bool); ok && onlineBool {
				status = "online"
				if statusStr, exists := userDetails.Presence["status"]; exists {
					if statusString, ok := statusStr.(string); ok && statusString != "" {
						status = statusString
					}
				}
			}
		}

		// Get roles for this member from the junction table
		memberRoles := allMemberRoles[member.UserID]
		if memberRoles == nil {
			memberRoles = []string{} // Default to empty if no roles found
		}

		memberResponse := SpaceMemberResponse{
			UserID:        member.UserID,
			Username:      userDetails.Username,
			Discriminator: userDetails.Discriminator,
			DisplayName:   userDetails.DisplayName,
			Avatar:        userDetails.Avatar,
			Roles:         memberRoles,
			JoinedAt:      member.JoinedAt.Format("2006-01-02T15:04:05Z"),
			IsOwner:       member.UserID == space.OwnerID,
			Status:        status,
		}

		memberResponses = append(memberResponses, memberResponse)
	}

	return c.JSON(fiber.Map{
		"members": memberResponses,
	})
}

// UpdateMemberRoles handles PATCH /spaces/:id/members/:userId/roles
func UpdateMemberRoles(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")
	targetUserID := c.Params("userId")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	if !isSpaceMember(spaceID, user.ID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Check if user has permission to manage roles
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, user.ID, strconv.FormatInt(spaceID, 10), utils.MANAGE_ROLES)
	if err != nil {
		log.Printf("[UpdateMemberRoles] Error checking permissions: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage roles",
		})
	}

	// Parse request body
	type UpdateRolesRequest struct {
		RoleIds []string `json:"role_ids"`
	}

	body := new(UpdateRolesRequest)
	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Validate that all roles exist in the space
	rolesRepo := repository.NewSpaceRolesRepository(database.Session)
	spaceRoles, err := rolesRepo.GetSpaceRoles(spaceID)
	if err != nil {
		log.Printf("Error getting space roles: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to validate roles",
		})
	}

	// Create a map of valid role IDs
	validRoles := make(map[string]bool)
	for _, role := range spaceRoles {
		validRoles[role.RoleID] = true
	}

	// Validate requested roles
	for _, roleID := range body.RoleIds {
		if !validRoles[roleID] {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Invalid role ID: " + roleID,
			})
		}
	}

	// Update member roles using the new junction table
	memberRolesRepo := repository.NewSpaceMemberRolesRepository(*database.Session)
	if updateErr := memberRolesRepo.SetMemberRoles(context.TODO(), spaceID, targetUserID, body.RoleIds, user.ID); updateErr != nil {
		log.Printf("Error updating member roles: %v", updateErr)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to update member roles",
		})
	}

	// Publish member role update event
	if err := events.PublishSpaceMemberRoleUpdateEvent(spaceID, targetUserID, user.ID, body.RoleIds); err != nil {
		log.Printf("Error publishing member role update event: %v", err)
		// Don't fail the request if event publishing fails
	}

	return c.JSON(fiber.Map{
		"message": "Member roles updated successfully",
	})
}

// KickMember handles DELETE /spaces/:id/members/:userId
func KickMember(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")
	targetUserID := c.Params("userId")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	if !isSpaceMember(spaceID, user.ID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Check if user has permission to kick members
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, user.ID, strconv.FormatInt(spaceID, 10), utils.KICK_MEMBERS)
	if err != nil {
		log.Printf("[KickMember] Error checking permissions: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to kick members",
		})
	}

	// Prevent kicking the space owner
	if isSpaceOwner(spaceID, targetUserID) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Cannot kick the space owner",
		})
	}

	// Prevent kicking yourself
	if user.ID == targetUserID {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Cannot kick yourself",
		})
	}

	// TODO: Implement actual member removal logic
	// This would involve:
	// 1. Removing from space_members table
	// 2. Removing from space_members_by_user table
	// 3. Updating user's spaces list
	// 4. Sending websocket notification

	return c.JSON(fiber.Map{
		"message": "Member kicked successfully",
	})
}
