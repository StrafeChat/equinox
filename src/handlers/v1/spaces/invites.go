package spaces

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/scylladb/gocqlx/v2/qb"
)

type CreateInviteInput struct {
	MaxUses   *int   `json:"max_uses,omitempty"`
	ExpiresIn *int64 `json:"expires_in,omitempty"` // Duration in seconds
}

type InviteResponse struct {
	ID        string       `json:"id"`
	Code      string       `json:"code"`
	SpaceID   string       `json:"space_id"`
	InviterID string       `json:"inviter_id"`
	MaxUses   *int         `json:"max_uses,omitempty"`
	Uses      int          `json:"uses"`
	ExpiresAt *time.Time   `json:"expires_at,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	Inviter   *InviterInfo `json:"inviter,omitempty"`
}

type InviterInfo struct {
	Username      string  `json:"username"`
	Discriminator int     `json:"discriminator"`
	DisplayName   *string `json:"display_name,omitempty"`
	Avatar        *string `json:"avatar,omitempty"`
}

type UseInviteInput struct {
	Code string `json:"code"`
}

// generateInviteCode generates a random 6-8 character invite code
func generateInviteCode() string {
	bytes := make([]byte, 4)
	rand.Read(bytes)
	return strings.ToUpper(hex.EncodeToString(bytes)[:6])
}

// CreateInvite creates a new space invite
func CreateInvite(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space and has permission to create invites
	var spaceMember models.SpaceMember
	query := qb.Select("space_members").Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(*database.Session)
	if err := query.Bind(spaceID, user.ID).Get(&spaceMember); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Check if user is the space owner or has appropriate permissions
	var space models.Space
	spaceQuery := qb.Select("spaces").Where(qb.Eq("id")).Query(*database.Session)
	if err := spaceQuery.Bind(spaceID).Get(&space); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Space not found",
		})
	}

	// For now, only space owners can create invites
	// TODO: Add role-based permissions for invite creation
	if space.OwnerID != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "Only space owners can create invites",
		})
	}

	body := new(CreateInviteInput)
	if err := c.Bind().Body(body); err != nil {
		log.Printf("Failed to parse request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body",
		})
	}
	log.Printf("Parsed request body: MaxUses=%v, ExpiresIn=%v", body.MaxUses, body.ExpiresIn)

	// Generate unique invite code
	var code string
	var attempts int
	for attempts < 10 {
		code = generateInviteCode()
		// Check if code already exists
		var existingInvite models.SpaceInviteByCode
		checkQuery := qb.Select("space_invites_by_code").Where(qb.Eq("code")).Query(*database.Session)
		if err := checkQuery.Bind(code).Get(&existingInvite); err != nil {
			// Code doesn't exist, we can use it
			break
		}
		attempts++
	}

	if attempts >= 10 {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to generate unique invite code",
		})
	}

	// Calculate expiration time
	var expiresAt *time.Time
	if body.ExpiresIn != nil {
		expTime := time.Now().Add(time.Duration(*body.ExpiresIn) * time.Second)
		expiresAt = &expTime
	}

	// Create invite
	inviteID := uuid.New().String()
	createdAt := time.Now()
	log.Printf("Generated invite ID: %s, expires_at: %v", inviteID, expiresAt)

	invite := models.SpaceInvite{
		ID:        inviteID,
		SpaceID:   spaceID,
		Code:      code,
		InviterID: user.ID,
		MaxUses:   body.MaxUses,
		Uses:      0,
		ExpiresAt: expiresAt,
		CreatedAt: createdAt,
	}

	// Insert into main table
	log.Printf("Creating invite with ID: %s, SpaceID: %d, Code: %s", invite.ID, invite.SpaceID, invite.Code)
	log.Printf("Database session status: %v", database.Session != nil)
	if err := models.SpaceInviteTable.InsertQuery(*database.Session).BindStruct(invite).ExecRelease(); err != nil {
		log.Printf("Failed to create invite in main table - Error: %v, Type: %T", err, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create invite",
			"details": err.Error(),
		})
	}
	log.Printf("Successfully inserted invite into main table")

	// Insert into by_code table
	inviteByCode := models.SpaceInviteByCode{
		Code:      invite.Code,
		ID:        invite.ID,
		SpaceID:   invite.SpaceID,
		InviterID: invite.InviterID,
		MaxUses:   invite.MaxUses,
		Uses:      invite.Uses,
		ExpiresAt: invite.ExpiresAt,
		CreatedAt: invite.CreatedAt,
	}

	if err := models.SpaceInviteByCodeTable.InsertQuery(*database.Session).BindStruct(inviteByCode).ExecRelease(); err != nil {
		log.Printf("Failed to create invite by code - Error: %v, Type: %T", err, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create invite",
			"details": err.Error(),
		})
	}
	log.Printf("Successfully inserted invite into by_code table")

	// Insert into by_space table
	inviteBySpace := models.SpaceInviteBySpace{
		SpaceID:   invite.SpaceID,
		ID:        invite.ID,
		Code:      invite.Code,
		InviterID: invite.InviterID,
		MaxUses:   invite.MaxUses,
		Uses:      invite.Uses,
		ExpiresAt: invite.ExpiresAt,
		CreatedAt: invite.CreatedAt,
	}

	if err := models.SpaceInviteBySpaceTable.InsertQuery(*database.Session).BindStruct(inviteBySpace).ExecRelease(); err != nil {
		log.Printf("Failed to create invite by space - Error: %v, Type: %T", err, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create invite",
			"details": err.Error(),
		})
	}
	log.Printf("Successfully inserted invite into by_space table")

	// Get inviter info
	var inviterInfo InviterInfo
	userQuery := qb.Select("users").Where(qb.Eq("id")).Query(*database.Session)
	if err := userQuery.Bind(user.ID).Get(&user); err == nil {
		inviterInfo = InviterInfo{
			Username:      user.Username,
			Discriminator: user.Discriminator,
			DisplayName:   user.DisplayName,
			Avatar:        user.Avatar,
		}
	}

	response := InviteResponse{
		ID:        invite.ID,
		Code:      invite.Code,
		SpaceID:   strconv.FormatInt(invite.SpaceID, 10),
		InviterID: invite.InviterID,
		MaxUses:   invite.MaxUses,
		Uses:      invite.Uses,
		ExpiresAt: invite.ExpiresAt,
		CreatedAt: invite.CreatedAt,
		Inviter:   &inviterInfo,
	}

	return c.Status(fiber.StatusCreated).JSON(response)
}

// GetSpaceInvites gets all invites for a space
func GetSpaceInvites(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	var spaceMember models.SpaceMember
	query := qb.Select("space_members").Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(*database.Session)
	if err := query.Bind(spaceID, user.ID).Get(&spaceMember); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Check if user has permission to view invites (for now, only space owners)
	var space models.Space
	spaceQuery := qb.Select("spaces").Where(qb.Eq("id")).Query(*database.Session)
	if err := spaceQuery.Bind(spaceID).Get(&space); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Space not found",
		})
	}

	if space.OwnerID != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "Only space owners can view invites",
		})
	}

	// Get all invites for the space
	log.Printf("Getting invites for space ID: %d", spaceID)
	var invites []models.SpaceInviteBySpace
	invitesQuery := qb.Select("space_invites_by_space").Where(qb.Eq("space_id")).Query(*database.Session)
	if err := invitesQuery.Bind(spaceID).Select(&invites); err != nil {
		log.Printf("Failed to get space invites: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get invites",
		})
	}
	log.Printf("Found %d invites for space %d", len(invites), spaceID)

	// Convert to response format and get inviter info
	var responses []InviteResponse
	for _, invite := range invites {
		// Skip expired invites
		if invite.ExpiresAt != nil && invite.ExpiresAt.Before(time.Now()) {
			continue
		}

		// Skip invites that have reached max uses
		if invite.MaxUses != nil && invite.Uses >= *invite.MaxUses {
			continue
		}

		// Get inviter info
		var inviter models.User
		var inviterInfo *InviterInfo
		userQuery := qb.Select("users").Where(qb.Eq("id")).Query(*database.Session)
		if err := userQuery.Bind(invite.InviterID).Get(&inviter); err == nil {
			inviterInfo = &InviterInfo{
				Username:      inviter.Username,
				Discriminator: inviter.Discriminator,
				DisplayName:   inviter.DisplayName,
				Avatar:        inviter.Avatar,
			}
		}

		response := InviteResponse{
			ID:        invite.ID,
			Code:      invite.Code,
			SpaceID:   strconv.FormatInt(invite.SpaceID, 10),
			InviterID: invite.InviterID,
			MaxUses:   invite.MaxUses,
			Uses:      invite.Uses,
			ExpiresAt: invite.ExpiresAt,
			CreatedAt: invite.CreatedAt,
			Inviter:   inviterInfo,
		}
		responses = append(responses, response)
	}

	return c.JSON(responses)
}

// DeleteInvite deletes a space invite
func DeleteInvite(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	spaceIDStr := c.Params("id")
	inviteID := c.Params("inviteId")

	spaceID, err := strconv.ParseInt(spaceIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid space ID",
		})
	}

	// Check if user is a member of the space
	var spaceMember models.SpaceMember
	query := qb.Select("space_members").Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(*database.Session)
	if err := query.Bind(spaceID, user.ID).Get(&spaceMember); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You are not a member of this space",
		})
	}

	// Get the invite to check ownership and get the code
	var invite models.SpaceInvite
	inviteQuery := qb.Select("space_invites").Where(qb.Eq("id")).Query(*database.Session)
	if err := inviteQuery.Bind(inviteID).Get(&invite); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Invite not found",
		})
	}

	// Check if user has permission to delete the invite
	var space models.Space
	spaceQuery := qb.Select("spaces").Where(qb.Eq("id")).Query(*database.Session)
	if err := spaceQuery.Bind(spaceID).Get(&space); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Space not found",
		})
	}

	// Only space owners or invite creators can delete invites
	if space.OwnerID != user.ID && invite.InviterID != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You don't have permission to delete this invite",
		})
	}

	// Delete from all tables
	deleteQuery := qb.Delete("space_invites").Where(qb.Eq("id")).Query(*database.Session)
	if err := deleteQuery.Bind(inviteID).Exec(); err != nil {
		log.Printf("Failed to delete invite: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to delete invite",
		})
	}

	deleteByCodeQuery := qb.Delete("space_invites_by_code").Where(qb.Eq("code")).Query(*database.Session)
	if err := deleteByCodeQuery.Bind(invite.Code).Exec(); err != nil {
		log.Printf("Failed to delete invite by code: %v", err)
	}

	deleteBySpaceQuery := qb.Delete("space_invites_by_space").Where(qb.Eq("space_id"), qb.Eq("created_at"), qb.Eq("id")).Query(*database.Session)
	if err := deleteBySpaceQuery.Bind(invite.SpaceID, invite.CreatedAt, invite.ID).Exec(); err != nil {
		log.Printf("Failed to delete invite by space: %v", err)
	}

	return c.Status(fiber.StatusNoContent).Send(nil)
}

// UseInvite allows a user to join a space using an invite code
func UseInvite(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	inviteCode := c.Params("code")

	// Get invite by code
	var invite models.SpaceInviteByCode
	inviteQuery := qb.Select("space_invites_by_code").Where(qb.Eq("code")).Query(*database.Session)
	if err := inviteQuery.Bind(inviteCode).Get(&invite); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Invalid invite code",
		})
	}

	// Check if invite is expired
	if invite.ExpiresAt != nil && invite.ExpiresAt.Before(time.Now()) {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{
			"error": "Invite has expired",
		})
	}

	// Check if invite has reached max uses
	if invite.MaxUses != nil && invite.Uses >= *invite.MaxUses {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{
			"error": "Invite has reached maximum uses",
		})
	}

	// Check if user is already a member of the space
	var existingMember models.SpaceMember
	memberQuery := qb.Select("space_members").Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(*database.Session)
	if err := memberQuery.Bind(invite.SpaceID, user.ID).Get(&existingMember); err == nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "You are already a member of this space",
		})
	}

	// Add user to space
	now := time.Now()
	spaceMember := models.SpaceMember{
		SpaceID:  invite.SpaceID,
		UserID:   user.ID,
		JoinedAt: now,
		Deaf:     false,
		Mute:     false,
		Flags:    0,
		Pending:  false,
	}

	// Insert into space_members
	insertMemberQuery := qb.Insert("space_members").Columns("space_id", "user_id", "nick", "avatar", "joined_at", "premium_since", "deaf", "mute", "flags", "pending", "permissions", "communication_disabled_until").Query(*database.Session)
	if err := insertMemberQuery.Bind(spaceMember.SpaceID, spaceMember.UserID, spaceMember.Nick, spaceMember.Avatar, spaceMember.JoinedAt, spaceMember.PremiumSince, spaceMember.Deaf, spaceMember.Mute, spaceMember.Flags, spaceMember.Pending, spaceMember.Permissions, spaceMember.CommunicationDisabledUntil).Exec(); err != nil {
		log.Printf("Failed to add user to space: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to join space",
		})
	}

	// Insert into space_members_by_user
	spaceMemberByUser := models.SpaceMembersByUser{
		UserID:   user.ID,
		SpaceID:  invite.SpaceID,
		JoinedAt: now,
	}

	insertMemberByUserQuery := qb.Insert("space_members_by_user").Columns("user_id", "space_id", "joined_at").Query(*database.Session)
	if err := insertMemberByUserQuery.Bind(spaceMemberByUser.UserID, spaceMemberByUser.SpaceID, spaceMemberByUser.JoinedAt).Exec(); err != nil {
		log.Printf("Failed to add user to space_members_by_user: %v", err)
	}

	// Increment invite usage count
	newUses := invite.Uses + 1
	updateUsesQuery := qb.Update("space_invites").Set("uses").Where(qb.Eq("id")).Query(*database.Session)
	if err := updateUsesQuery.Bind(newUses, invite.ID).Exec(); err != nil {
		log.Printf("Failed to update invite uses: %v", err)
	}

	updateUsesByCodeQuery := qb.Update("space_invites_by_code").Set("uses").Where(qb.Eq("code")).Query(*database.Session)
	if err := updateUsesByCodeQuery.Bind(newUses, invite.Code).Exec(); err != nil {
		log.Printf("Failed to update invite uses by code: %v", err)
	}

	updateUsesBySpaceQuery := qb.Update("space_invites_by_space").Set("uses").Where(qb.Eq("space_id"), qb.Eq("created_at"), qb.Eq("id")).Query(*database.Session)
	if err := updateUsesBySpaceQuery.Bind(newUses, invite.SpaceID, invite.CreatedAt, invite.ID).Exec(); err != nil {
		log.Printf("Failed to update invite uses by space: %v", err)
	}

	// Get space info to return
	var space models.Space
	spaceQuery := qb.Select("spaces").Where(qb.Eq("id")).Query(*database.Session)
	if err := spaceQuery.Bind(invite.SpaceID).Get(&space); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get space info",
		})
	}

	return c.JSON(fiber.Map{
		"message":  fmt.Sprintf("Successfully joined %s", space.Name),
		"space_id": strconv.FormatInt(space.ID, 10),
		"space":    space,
	})
}

// GetInviteInfo gets information about an invite by code (public endpoint)
func GetInviteInfo(c fiber.Ctx) error {
	inviteCode := c.Params("code")

	// Get invite by code
	var invite models.SpaceInviteByCode
	inviteQuery := qb.Select("space_invites_by_code").Where(qb.Eq("code")).Query(*database.Session)
	if err := inviteQuery.Bind(inviteCode).Get(&invite); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Invalid invite code",
		})
	}

	// Check if invite is expired
	if invite.ExpiresAt != nil && invite.ExpiresAt.Before(time.Now()) {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{
			"error": "Invite has expired",
		})
	}

	// Check if invite has reached max uses
	if invite.MaxUses != nil && invite.Uses >= *invite.MaxUses {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{
			"error": "Invite has reached maximum uses",
		})
	}

	// Get space info
	var space models.Space
	spaceQuery := qb.Select("spaces").Where(qb.Eq("id")).Query(*database.Session)
	if err := spaceQuery.Bind(invite.SpaceID).Get(&space); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get space info",
		})
	}

	// Get member count
	var memberCount int64
	var members []models.SpaceMember
	memberCountQuery := qb.Select("space_members").Where(qb.Eq("space_id")).Query(*database.Session)
	if err := memberCountQuery.Bind(invite.SpaceID).Select(&members); err != nil {
		memberCount = 0
	} else {
		memberCount = int64(len(members))
	}

	// Get inviter info
	var inviter models.User
	var inviterInfo *InviterInfo
	userQuery := qb.Select("users").Where(qb.Eq("id")).Query(*database.Session)
	if err := userQuery.Bind(invite.InviterID).Get(&inviter); err == nil {
		inviterInfo = &InviterInfo{
			Username:      inviter.Username,
			Discriminator: inviter.Discriminator,
			DisplayName:   inviter.DisplayName,
			Avatar:        inviter.Avatar,
		}
	}

	// Return invite info
	return c.JSON(fiber.Map{
		"space_id":             strconv.FormatInt(space.ID, 10),
		"space_name":           space.Name,
		"space_icon":           space.Icon,
		"space_banner":         space.Banner,
		"space_name_acronym":   space.NameAcronym,
		"member_count":         memberCount,
		"inviter_id":           invite.InviterID,
		"inviter_username":     inviterInfo.Username,
		"inviter_display_name": inviterInfo.DisplayName,
		"inviter_avatar":       inviterInfo.Avatar,
		"expires_at":           invite.ExpiresAt,
		"max_uses":             invite.MaxUses,
		"uses":                 invite.Uses,
		"code":                 invite.Code,
	})
}
