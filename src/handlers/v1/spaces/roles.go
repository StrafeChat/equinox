package spaces

import (
	"log"
	"strconv"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/events"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/repository"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
)

// Request/Response structs
type CreateRoleRequest struct {
	Name        string                 `json:"name" validate:"required,min=1,max=100"`
	Color       *string                `json:"color,omitempty"`
	Permissions map[string]interface{} `json:"permissions,omitempty"`
	Mentionable *bool                  `json:"mentionable,omitempty"`
	Hoist       *bool                  `json:"hoist,omitempty"`
}

type UpdateRoleRequest struct {
	Name        *string                `json:"name,omitempty" validate:"omitempty,min=1,max=100"`
	Color       *string                `json:"color,omitempty"`
	Permissions map[string]interface{} `json:"permissions,omitempty"`
	Position    *int                   `json:"position,omitempty"`
	Mentionable *bool                  `json:"mentionable,omitempty"`
	Hoist       *bool                  `json:"hoist,omitempty"`
}

type RoleResponse struct {
	ID          string                 `json:"id"`
	SpaceID     string                 `json:"space_id"`
	Name        string                 `json:"name"`
	Color       *string                `json:"color"`
	Permissions map[string]interface{} `json:"permissions"`
	Position    int                    `json:"position"`
	Mentionable bool                   `json:"mentionable"`
	Hoist       bool                   `json:"hoist"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
}

type PermissionResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

// Helper functions
func convertRoleToResponse(role *models.SpaceRole) RoleResponse {
	permissionsMap := make(map[string]interface{})
	for _, perm := range role.Permissions {
		permissionsMap[perm] = true
	}
	return RoleResponse{
		ID:          role.RoleID,
		SpaceID:     strconv.FormatInt(role.SpaceID, 10),
		Name:        role.Name,
		Color:       role.Color,
		Permissions: permissionsMap,
		Position:    role.Position,
		Mentionable: role.Mentionable,
		Hoist:       role.Hoist,
		CreatedAt:   role.CreatedAt,
		UpdatedAt:   role.UpdatedAt,
	}
}

func isSpaceMember(spaceID int64, userID string) bool {
	membersRepo := repository.NewSpaceMembersRepository(database.Session)
	isMember, err := membersRepo.IsMember(spaceID, userID)
	if err != nil {
		log.Printf("Error checking space membership: %v", err)
		return false
	}
	return isMember
}

func isSpaceOwner(spaceID int64, userID string) bool {
	spaceRepo := repository.NewSpaceRepository(database.Session)
	space, err := spaceRepo.GetSpace(spaceID)
	if err != nil {
		log.Printf("Error getting space: %v", err)
		return false
	}
	return space.OwnerID == userID
}

// Handler functions
func GetSpaceRoles(c fiber.Ctx) error {
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

	// Get all roles for the space
	rolesRepo := repository.NewSpaceRolesRepository(database.Session)
	roles, err := rolesRepo.GetSpaceRoles(spaceID)
	if err != nil {
		log.Printf("Failed to get space roles: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get space roles",
		})
	}

	// Convert to response format
	responseRoles := make([]RoleResponse, len(roles))
	for i, role := range roles {
		responseRoles[i] = convertRoleToResponse(&role)
	}

	return c.JSON(fiber.Map{
		"roles": responseRoles,
	})
}

func CreateRole(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user has permission to manage roles
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, user.ID, strconv.FormatInt(spaceID, 10), utils.MANAGE_ROLES)
	if err != nil {
		log.Printf("[CreateRole] Error checking permissions: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage roles",
		})
	}

	body := new(CreateRoleRequest)
	if err := c.Bind().JSON(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Validate required fields
	if body.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Role name is required",
		})
	}

	// Get highest position for new role
	rolesRepo := repository.NewSpaceRolesRepository(database.Session)
	maxPosition, err := rolesRepo.GetMaxPosition(spaceID)
	if err != nil {
		log.Printf("Failed to get max position: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create role",
		})
	}

	// Create new role
	now := time.Now()
	roleID := helpers.GenerateRoleID().String()

	var permissions []string
	if body.Permissions != nil {
		permissions = utils.PermissionsFromMap(body.Permissions)
	}

	mentionable := true
	if body.Mentionable != nil {
		mentionable = *body.Mentionable
	}

	hoist := false
	if body.Hoist != nil {
		hoist = *body.Hoist
	}

	role := models.SpaceRole{
		SpaceID:     spaceID,
		RoleID:      roleID,
		Name:        body.Name,
		Color:       body.Color,
		Permissions: permissions,
		Position:    maxPosition + 1,
		Mentionable: mentionable,
		Hoist:       hoist,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Insert role
	if err := rolesRepo.CreateRole(role); err != nil {
		log.Printf("Failed to create role: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create role",
		})
	}

	// Publish role create event
	roleData := map[string]interface{}{
		"role_id":     roleID,
		"name":        role.Name,
		"color":       role.Color,
		"permissions": role.Permissions,
		"position":    role.Position,
		"mentionable": role.Mentionable,
		"hoist":       role.Hoist,
		"created_at":  role.CreatedAt,
	}
	if err := events.PublishSpaceRoleCreateEvent(spaceID, roleData, user.ID); err != nil {
		log.Printf("Error publishing role create event: %v", err)
		// Don't fail the request if event publishing fails
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"role": convertRoleToResponse(&role),
	})
}

// UpdateRole handles PATCH /spaces/{spaceId}/roles/{roleId}
// UpdateRole handles PUT /spaces/{spaceId}/roles/{roleId}
func UpdateRole(c fiber.Ctx) error {
	spaceIDStr := c.Params("id")
	roleID := c.Params("roleId")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	user := c.Locals("user").(models.User)
	userID := user.ID

	// Check if user has permission to manage roles
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, userID, strconv.FormatInt(spaceID, 10), utils.MANAGE_ROLES)
	if err != nil {
		log.Printf("[UpdateRole] Error checking permissions: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage roles",
		})
	}

	var req UpdateRoleRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}

	// Don't allow editing @everyone role name
	if roleID == "@everyone" && req.Name != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Cannot change @everyone role name",
		})
	}

	// Build updates map
	updates := make(map[string]interface{})
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Color != nil {
		updates["color"] = *req.Color
	}
	if req.Permissions != nil {
		permissions := utils.PermissionsFromMap(req.Permissions)
		updates["permissions"] = permissions
	}
	if req.Position != nil {
		updates["position"] = *req.Position
	}
	if req.Mentionable != nil {
		updates["mentionable"] = *req.Mentionable
	}
	if req.Hoist != nil {
		updates["hoist"] = *req.Hoist
	}

	if len(updates) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "No updates provided",
		})
	}

	updates["updated_at"] = time.Now()

	// Update role
	rolesRepo := repository.NewSpaceRolesRepository(database.Session)
	if err := rolesRepo.UpdateRole(spaceID, roleID, updates); err != nil {
		log.Printf("Error updating role: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to update role",
		})
	}

	// Get updated role
	updatedRole, err := rolesRepo.GetRole(spaceID, roleID)
	if err != nil {
		log.Printf("Error getting updated role: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get updated role",
		})
	}

	// Publish role update event
	roleData := map[string]interface{}{
		"role_id":     updatedRole.RoleID,
		"name":        updatedRole.Name,
		"color":       updatedRole.Color,
		"permissions": updatedRole.Permissions,
		"position":    updatedRole.Position,
		"mentionable": updatedRole.Mentionable,
		"hoist":       updatedRole.Hoist,
		"updated_at":  updatedRole.UpdatedAt,
	}
	if err := events.PublishSpaceRoleUpdateEvent(spaceID, roleID, userID, roleData); err != nil {
		log.Printf("Error publishing role update event: %v", err)
		// Don't fail the request if event publishing fails
	}

	return c.JSON(convertRoleToResponse(updatedRole))
}

// DeleteRole handles DELETE /spaces/{spaceId}/roles/{roleId}
func DeleteRole(c fiber.Ctx) error {
	spaceIDStr := c.Params("id")
	roleID := c.Params("roleId")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	user := c.Locals("user").(models.User)
	userID := user.ID

	// Check if user has permission to manage roles
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, userID, strconv.FormatInt(spaceID, 10), utils.MANAGE_ROLES)
	if err != nil {
		log.Printf("[DeleteRole] Error checking permissions: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage roles",
		})
	}

	// Delete role using the new system that removes it from all members
	rolesRepo := repository.NewSpaceRolesRepository(database.Session)
	memberRolesRepo := repository.NewSpaceMemberRolesRepository(*database.Session)
	if err := rolesRepo.DeleteRole(spaceID, roleID, memberRolesRepo); err != nil {
		if err.Error() == "cannot delete @everyone role" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "Cannot delete @everyone role",
			})
		}
		log.Printf("Error deleting role: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to delete role",
		})
	}

	// Publish role delete event
	if err := events.PublishSpaceRoleDeleteEvent(spaceID, roleID, userID); err != nil {
		log.Printf("Error publishing role delete event: %v", err)
		// Don't fail the request if event publishing fails
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetAvailablePermissions handles GET /permissions
func GetAvailablePermissions(c fiber.Ctx) error {
	permissions := utils.GetAllPermissions()

	var response []PermissionResponse
	for _, perm := range permissions {
		response = append(response, PermissionResponse{
			ID:          perm.ID,
			Name:        perm.Name,
			Description: perm.Description,
			Category:    perm.Category,
		})
	}

	return c.JSON(response)
}

// AddRoleToMember handles POST /spaces/{spaceId}/members/{userId}/roles/{roleId}
func AddRoleToMember(c fiber.Ctx) error {
	spaceIDStr := c.Params("id")
	targetUserID := c.Params("userId")
	roleID := c.Params("roleId")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	user := c.Locals("user").(models.User)
	userID := user.ID

	// Check if user has permission to manage roles
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, userID, strconv.FormatInt(spaceID, 10), utils.MANAGE_ROLES)
	if err != nil {
		log.Printf("[AddRoleToMember] Error checking permissions: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage roles",
		})
	}

	// Add role to member using the new junction table
	memberRolesRepo := repository.NewSpaceMemberRolesRepository(*database.Session)
	if err := memberRolesRepo.AddRoleToMember(nil, spaceID, targetUserID, roleID, userID); err != nil {
		log.Printf("Error adding role to member: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to add role to member",
		})
	}

	// Get updated member roles for event
	memberRoles, err := memberRolesRepo.GetMemberRoles(nil, spaceID, targetUserID)
	if err != nil {
		log.Printf("Error getting member roles for event: %v", err)
	} else {
		// Convert to role IDs for event
		roleIDs := make([]string, len(memberRoles))
		for i, role := range memberRoles {
			roleIDs[i] = role.RoleID
		}
		// Publish member role update event
		if err := events.PublishSpaceMemberRoleUpdateEvent(spaceID, targetUserID, userID, roleIDs); err != nil {
			log.Printf("Error publishing member role update event: %v", err)
			// Don't fail the request if event publishing fails
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// RemoveRoleFromMember handles DELETE /spaces/{spaceId}/members/{userId}/roles/{roleId}
func RemoveRoleFromMember(c fiber.Ctx) error {
	spaceIDStr := c.Params("id")
	targetUserID := c.Params("userId")
	roleID := c.Params("roleId")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	user := c.Locals("user").(models.User)
	userID := user.ID

	// Check if user has permission to manage roles
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, userID, strconv.FormatInt(spaceID, 10), utils.MANAGE_ROLES)
	if err != nil {
		log.Printf("[RemoveRoleFromMember] Error checking permissions: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to check permissions",
		})
	}
	if !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to manage roles",
		})
	}

	// Remove role from member using the new junction table
	memberRolesRepo := repository.NewSpaceMemberRolesRepository(*database.Session)
	if err := memberRolesRepo.RemoveRoleFromMember(nil, spaceID, targetUserID, roleID); err != nil {
		log.Printf("Error removing role from member: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to remove role from member",
		})
	}

	// Get updated member roles for event
	memberRoles, err := memberRolesRepo.GetMemberRoles(nil, spaceID, targetUserID)
	if err != nil {
		log.Printf("Error getting member roles for event: %v", err)
	} else {
		// Convert to role IDs for event
		roleIDs := make([]string, len(memberRoles))
		for i, role := range memberRoles {
			roleIDs[i] = role.RoleID
		}
		// Publish member role update event
		if err := events.PublishSpaceMemberRoleUpdateEvent(spaceID, targetUserID, userID, roleIDs); err != nil {
			log.Printf("Error publishing member role update event: %v", err)
			// Don't fail the request if event publishing fails
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// CheckUserPermission handles GET /spaces/{spaceId}/permissions/{permission}
func CheckUserPermission(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")
	permission := c.Params("permission")

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

	// Check if user has the requested permission
	hasPermission, err := utils.CheckPermissionFromContext(database.Session, user.ID, strconv.FormatInt(spaceID, 10), permission)
	if err != nil {
		log.Printf("Error checking permission: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Internal server error",
		})
	}

	return c.JSON(fiber.Map{
		"has_permission": hasPermission,
	})
}
