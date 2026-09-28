package spaces

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// spaceError maps a spaces-service sentinel error to an HTTP response. Centralized so a
// newly introduced service error only needs a case added here once. The recurring bug this
// replaces: every handler used to hand-roll its own switch over a subset of the errors its
// service call could return, and a service change (e.g. CreateInvite switching from an
// owner-only check to a permission check, introducing ErrMissingPerm) would silently 500
// instead of 403/404 wherever the matching handler wasn't updated in lockstep - which
// happened repeatedly (CreateInvite, the kick/ban family, and every room/role handler that
// can hit ErrSpaceNotFound via canManageRoles/canManageRooms all missed at least one case).
func spaceError(c fiber.Ctx, err error, logFields map[string]any) error {
	switch {
	case errors.Is(err, ErrSpaceNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "space not found"})
	case errors.Is(err, ErrInviteNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "invite not found"})
	case errors.Is(err, ErrRoleNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "role not found"})
	case errors.Is(err, ErrEmojiNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "emoji not found"})
	case errors.Is(err, ErrInvalidEmojiName), errors.Is(err, ErrEmojiNameTaken), errors.Is(err, ErrEmojiLimit):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrInvalidRoom):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
	case errors.Is(err, ErrNotMember), errors.Is(err, ErrMissingPerm), errors.Is(err, ErrRoleHierarchy),
		errors.Is(err, ErrCannotModerateOwner), errors.Is(err, ErrNotSpaceOwner), errors.Is(err, ErrPermissionEscalation),
		errors.Is(err, ErrInsufficientSpacePermission), errors.Is(err, ErrBanned):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrCannotEditEveryone), errors.Is(err, ErrInvalidSlowmode), errors.Is(err, ErrInvalidRoomType),
		errors.Is(err, ErrInvalidReorder), errors.Is(err, ErrOwnerCannotLeave), errors.Is(err, ErrInvalidSpaceName),
		errors.Is(err, ErrNothingToPatch), errors.Is(err, ErrInvalidRoleName), errors.Is(err, ErrCannotChangeOwnerRoles),
		errors.Is(err, ErrInvalidRoomName), errors.Is(err, ErrInvalidTopic), errors.Is(err, ErrInvalidDescription),
		errors.Is(err, ErrInvalidSystemRoom), errors.Is(err, ErrInvalidAFKRoom), errors.Is(err, ErrInvalidAFKTimeout),
		errors.Is(err, ErrInvalidNotifLevel), errors.Is(err, ErrInvalidSystemFlags), errors.Is(err, ErrInvalidWidgetRoom),
		errors.Is(err, ErrInvalidInvite), errors.Is(err, ErrInvalidUserLimit), errors.Is(err, ErrInvalidBitrate),
		errors.Is(err, ErrAlreadyOwner), errors.Is(err, ErrSpaceNameMismatch):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	logger.Err("spaces", err, logFields)
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

type Handler struct {
	svc *Service
	cfg *config.Config
}

func NewHandler(svc *Service, cfg *config.Config) *Handler {
	return &Handler{svc: svc, cfg: cfg}
}

// Create creates a new space. POST /spaces. Body: { "name": "...", "description": "...", "icon": "..." }.
func (h *Handler) Create(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var body CreateSpaceInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	space, err := h.svc.CreateSpace(c.Context(), user.ID, &body)
	if err != nil {
		return spaceError(c, err, map[string]any{"user_id": user.ID})
	}
	return c.Status(http.StatusCreated).JSON(h.spaceWithRoles(c, space))
}

// List returns spaces the current user is a member of. GET /spaces.
func (h *Handler) List(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	rows, err := h.svc.ListSpacesForUser(c.Context(), user.ID)
	if err != nil {
		logger.Err("spaces", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SpaceID)
	}
	list, err := h.svc.GetSpaces(c.Context(), ids)
	if err != nil {
		logger.Err("spaces", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := make([]fiber.Map, 0, len(list))
	for _, space := range list {
		if space == nil {
			continue
		}
		out = append(out, h.spaceWithRoles(c, space))
	}
	return c.JSON(out)
}

// spaceWithRoles is a member-facing space payload: the space plus its roles, so the
// client can evaluate permissions without a second request (Discord ships roles inside
// the guild object for the same reason).
func (h *Handler) spaceWithRoles(c fiber.Ctx, space *Space) fiber.Map {
	m := spaceToJSON(space)
	if roles, err := h.svc.SpaceRoles(c.Context(), space.ID); err == nil {
		m["roles"] = RoleMaps(roles)
	}
	return m
}

// Get returns a space by ID. User must be a member. GET /spaces/:id.
func (h *Handler) Get(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	space, err := h.svc.GetSpaceForUser(c.Context(), user.ID, spaceID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.JSON(h.spaceWithRoles(c, space))
}

// GetRooms returns rooms in the space (text and voice). User must be a member. GET /spaces/:id/rooms.
func (h *Handler) GetRooms(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomList, snap, err := h.svc.ListSpaceRoomsWithOverrides(c.Context(), user.ID, spaceID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	// Per-user read-state: a plain REST refresh used to return none at all for space
	// channels (only the WS READY payload carried it), unlike GET /rooms for PMs. One
	// partition read for every room the user has state in, not one read per room.
	mentionCounts, _ := h.svc.GetMentionCounts(c.Context(), user.ID)
	userRows, _ := h.svc.UserRoomRows(c.Context(), user.ID)
	out := make([]fiber.Map, 0, len(roomList))
	for _, r := range roomList {
		m := spaceRoomToJSON(r)
		AttachOverrides(m, snap.RoomOverridesFor(r.ID))
		row := userRows[r.ID]
		if row != nil && row.LastReadMessageID != nil {
			m["last_read_message_id"] = id.Format(*row.LastReadMessageID)
		}
		if mc := row.DisplayMentionCount(mentionCounts[r.ID]); mc > 0 {
			m["mention_count"] = mc
		}
		out = append(out, m)
	}
	return c.JSON(out)
}

// AckAll marks every text/voice room in the space read for the caller. POST /spaces/:id/ack-all.
func (h *Handler) AckAll(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	if err := h.svc.AckAll(c.Context(), user.ID, spaceID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

func spaceRoomToJSON(r *rooms.Room) fiber.Map {
	return fiber.Map(RoomMap(r))
}

func spaceToJSON(s *Space) fiber.Map {
	m := fiber.Map{
		"id":                            id.Format(s.ID),
		"name":                          s.Name,
		"name_acronym":                  s.NameAcronym,
		"description":                   s.Description,
		"icon":                          s.Icon,
		"banner":                        s.Banner,
		"owner_id":                      id.Format(s.OwnerID),
		"verification_level":            s.VerificationLevel,
		"default_message_notifications": s.DefaultMessageNotif,
		"explicit_content_filter":       s.ExplicitContentFilter,
		"features":                      s.Features,
		"afk_timeout":                   s.AFKTimeout,
		"system_room_flags":             s.SystemRoomFlags,
		"max_presences":                 s.MaxPresences,
		"max_members":                   s.MaxMembers,
		"vanity_url_code":               s.VanityURLCode,
		"preferred_locale":              s.PreferredLocale,
		"max_video_room_users":          s.MaxVideoRoomUsers,
		"widget_enabled":                s.WidgetEnabled,
		"created_at":                    s.CreatedAt,
		"updated_at":                    s.UpdatedAt,
	}
	if s.AFKRoomID != nil {
		m["afk_room_id"] = id.Format(*s.AFKRoomID)
	}
	if s.SystemRoomID != nil {
		m["system_room_id"] = id.Format(*s.SystemRoomID)
	}
	if s.RulesRoomID != nil {
		m["rules_room_id"] = id.Format(*s.RulesRoomID)
	}
	if s.PublicUpdatesRoomID != nil {
		m["public_updates_room_id"] = id.Format(*s.PublicUpdatesRoomID)
	}
	if s.WidgetRoomID != nil {
		m["widget_room_id"] = id.Format(*s.WidgetRoomID)
	}
	if s.EveryoneRoleID != 0 {
		m["everyone_role_id"] = id.Format(s.EveryoneRoleID)
	}
	return m
}

// userSummary is the compact public profile embedded in moderation payloads (invites,
// bans, audit log) so the client can render names and avatars without extra lookups.
func userSummary(u *auth.User) fiber.Map {
	if u == nil {
		return nil
	}
	return fiber.Map{
		"id":            id.Format(u.ID),
		"username":      u.Username,
		"discriminator": u.Discriminator,
		"display_name":  u.DisplayName,
		"avatar":        u.Avatar,
	}
}

func inviteToJSON(inv *SpaceInvite, inviter *auth.User) fiber.Map {
	m := fiber.Map{
		"code":         inv.Code,
		"space_id":     id.Format(inv.SpaceID),
		"inviter_id":   id.Format(inv.InviterID),
		"max_uses":     inv.MaxUses,
		"current_uses": inv.CurrentUses,
		"created_at":   inv.CreatedAt,
	}
	if inv.ExpiresAt != nil {
		m["expires_at"] = inv.ExpiresAt
	}
	if inviter != nil {
		m["inviter"] = userSummary(inviter)
	}
	return m
}

func spaceRoleToJSON(r *SpaceRole) fiber.Map {
	return fiber.Map(RoleMap(r))
}

func roomOverrideToJSON(o *SpaceRoomRoleOverride) fiber.Map {
	return fiber.Map{
		"role_id":    id.Format(o.RoleID),
		"allow":      o.Allow,
		"deny":       o.Deny,
		"created_at": o.CreatedAt,
		"updated_at": o.UpdatedAt,
	}
}

func roomUserOverrideToJSON(o *SpaceRoomUserOverride) fiber.Map {
	return fiber.Map{
		"user_id":    id.Format(o.UserID),
		"allow":      o.Allow,
		"deny":       o.Deny,
		"created_at": o.CreatedAt,
		"updated_at": o.UpdatedAt,
	}
}

// Members returns members of a space with basic user info. GET /spaces/:id/members.
func (h *Handler) Members(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	members, err := h.svc.ListMembers(c.Context(), user.ID, spaceID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	out := make([]fiber.Map, 0, len(members))
	for _, m := range members {
		u := m.User
		roleStrs := make([]string, len(m.Member.RoleIDs))
		for i, rid := range m.Member.RoleIDs {
			roleStrs[i] = id.Format(rid)
		}
		out = append(out, fiber.Map{
			"id":            id.Format(u.ID),
			"username":      u.Username,
			"discriminator": u.Discriminator,
			"display_name":  u.DisplayName,
			"avatar":        u.Avatar,
			"banner":        u.Banner,
			"bio":           u.Bio,
			"about_me":      u.AboutMe,
			"joined_at":     m.Member.JoinedAt,
			"roles":         roleStrs,
			"presence":      auth.ToPublicPresence(u.Presence, true),
		})
	}
	return c.JSON(out)
}

// CreateInvite creates a new invite for the space. POST /spaces/:id/invites.
func (h *Handler) CreateInvite(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var body CreateInviteInput
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &body); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
		}
	}
	inv, err := h.svc.CreateInvite(c.Context(), user.ID, spaceID, &body)
	if err != nil {
		if err == ErrMissingPerm {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to create invites for this space"})
		}
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.Status(http.StatusCreated).JSON(inviteToJSON(inv, user))
}

// InvitePreview returns public space info for an invite (no auth). GET /spaces/invites/:code.
func (h *Handler) InvitePreview(c fiber.Ctx) error {
	code := c.Params("code")
	space, inviterName, err := h.svc.GetInvitePreview(c.Context(), code)
	if err != nil {
		return spaceError(c, err, map[string]any{"code": code})
	}
	out := fiber.Map{
		"space":   spaceToJSON(space),
		"inviter": nil,
	}
	if inviterName != "" {
		out["inviter"] = fiber.Map{"display_name": inviterName}
	}
	return c.JSON(out)
}

// JoinByInvite joins a space via invite code. POST /spaces/invites/:code/join.
func (h *Handler) JoinByInvite(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	code := c.Params("code")
	space, err := h.svc.JoinByInvite(c.Context(), user.ID, code)
	if err != nil {
		return spaceError(c, err, map[string]any{"code": code})
	}
	return c.Status(http.StatusOK).JSON(h.spaceWithRoles(c, space))
}

// KickMember DELETE /spaces/:id/members/:userId
func (h *Handler) KickMember(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	targetUserID, err := id.Parse(c.Params("userId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	if err := h.svc.KickMember(c.Context(), user.ID, spaceID, targetUserID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

type banMemberRequest struct {
	Reason string `json:"reason,omitempty"`
}

// BanMember POST /spaces/:id/bans/:userId. Body: { "reason": "..." } (optional).
func (h *Handler) BanMember(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	targetUserID, err := id.Parse(c.Params("userId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var body banMemberRequest
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &body); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
		}
	}
	if err := h.svc.BanMember(c.Context(), user.ID, spaceID, targetUserID, body.Reason); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// UnbanMember DELETE /spaces/:id/bans/:userId
func (h *Handler) UnbanMember(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	targetUserID, err := id.Parse(c.Params("userId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	if err := h.svc.UnbanMember(c.Context(), user.ID, spaceID, targetUserID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListBans GET /spaces/:id/bans
func (h *Handler) ListBans(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	bans, users, err := h.svc.ListBans(c.Context(), user.ID, spaceID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	out := make([]fiber.Map, 0, len(bans))
	for i := range bans {
		m := fiber.Map{
			"user_id":    id.Format(bans[i].UserID),
			"reason":     bans[i].Reason,
			"banned_by":  id.Format(bans[i].BannedBy),
			"created_at": bans[i].CreatedAt,
		}
		if u := users[bans[i].UserID]; u != nil {
			m["user"] = userSummary(u)
		}
		if u := users[bans[i].BannedBy]; u != nil {
			m["banned_by_user"] = userSummary(u)
		}
		out = append(out, m)
	}
	return c.JSON(out)
}

// Leave POST /spaces/:id/leave
func (h *Handler) Leave(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	if err := h.svc.LeaveSpace(c.Context(), user.ID, spaceID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListRoles GET /spaces/:id/roles
func (h *Handler) ListRoles(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roles, err := h.svc.ListSpaceRoles(c.Context(), user.ID, spaceID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	out := make([]fiber.Map, 0, len(roles))
	for i := range roles {
		out = append(out, spaceRoleToJSON(&roles[i]))
	}
	return c.JSON(out)
}

// CreateRole POST /spaces/:id/roles
func (h *Handler) CreateRole(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var body CreateSpaceRoleInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	role, err := h.svc.CreateSpaceRole(c.Context(), user.ID, spaceID, &body)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.Status(http.StatusCreated).JSON(spaceRoleToJSON(role))
}

// UpdateRole PATCH /spaces/:id/roles/:roleId
func (h *Handler) UpdateRole(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roleID, err := id.Parse(c.Params("roleId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid role id"})
	}
	var body UpdateSpaceRoleInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	role, err := h.svc.UpdateSpaceRole(c.Context(), user.ID, spaceID, roleID, &body)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.JSON(spaceRoleToJSON(role))
}

// DeleteRole DELETE /spaces/:id/roles/:roleId
func (h *Handler) DeleteRole(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roleID, err := id.Parse(c.Params("roleId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid role id"})
	}
	if err := h.svc.DeleteSpaceRole(c.Context(), user.ID, spaceID, roleID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// PutMemberRoles PUT /spaces/:id/members/:userId/roles  body: { "role_ids": ["snowflake", ...] } (without @everyone; server merges)
func (h *Handler) PutMemberRoles(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	targetUserID, err := id.Parse(c.Params("userId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var body struct {
		RoleIDs []string `json:"role_ids"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	ids := make([]int64, 0, len(body.RoleIDs))
	for _, s := range body.RoleIDs {
		rid, err := id.Parse(s)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid role id in list"})
		}
		ids = append(ids, rid)
	}
	if err := h.svc.SetMemberRoles(c.Context(), user.ID, spaceID, targetUserID, ids); err != nil {
		// ErrRoleNotFound here means a bad role_id in the request body, not a URL path
		// resource - 400, not the 404 spaceError would otherwise give it.
		if err == ErrRoleNotFound {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unknown role id"})
		}
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListRoomOverrides GET /spaces/:id/rooms/:roomId/overrides
func (h *Handler) ListRoomOverrides(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomID, err := id.Parse(c.Params("roomId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	ovs, err := h.svc.ListRoomRoleOverrides(c.Context(), user.ID, spaceID, roomID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	out := make([]fiber.Map, 0, len(ovs))
	for i := range ovs {
		out = append(out, roomOverrideToJSON(&ovs[i]))
	}
	return c.JSON(out)
}

// PutRoomOverride PUT /spaces/:id/rooms/:roomId/overrides/:roleId
func (h *Handler) PutRoomOverride(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomID, err := id.Parse(c.Params("roomId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	roleID, err := id.Parse(c.Params("roleId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid role id"})
	}
	var body PutRoomRoleOverrideInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if err := h.svc.PutRoomRoleOverride(c.Context(), user.ID, spaceID, roomID, roleID, &body); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// DeleteRoomOverride DELETE /spaces/:id/rooms/:roomId/overrides/:roleId
func (h *Handler) DeleteRoomOverride(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomID, err := id.Parse(c.Params("roomId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	roleID, err := id.Parse(c.Params("roleId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid role id"})
	}
	if err := h.svc.DeleteRoomRoleOverride(c.Context(), user.ID, spaceID, roomID, roleID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListRoomUserOverrides GET /spaces/:id/rooms/:roomId/overrides/users
func (h *Handler) ListRoomUserOverrides(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomID, err := id.Parse(c.Params("roomId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	ovs, err := h.svc.ListRoomUserOverrides(c.Context(), user.ID, spaceID, roomID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	out := make([]fiber.Map, 0, len(ovs))
	for i := range ovs {
		out = append(out, roomUserOverrideToJSON(&ovs[i]))
	}
	return c.JSON(out)
}

// PutRoomUserOverride PUT /spaces/:id/rooms/:roomId/overrides/users/:userId
func (h *Handler) PutRoomUserOverride(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomID, err := id.Parse(c.Params("roomId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	targetUserID, err := id.Parse(c.Params("userId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var body PutRoomUserOverrideInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if err := h.svc.PutRoomUserOverride(c.Context(), user.ID, spaceID, roomID, targetUserID, &body); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// DeleteRoomUserOverride DELETE /spaces/:id/rooms/:roomId/overrides/users/:userId
func (h *Handler) DeleteRoomUserOverride(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomID, err := id.Parse(c.Params("roomId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	targetUserID, err := id.Parse(c.Params("userId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	if err := h.svc.DeleteRoomUserOverride(c.Context(), user.ID, spaceID, roomID, targetUserID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// PatchRoom PATCH /spaces/:id/rooms/:roomId
func (h *Handler) PatchRoom(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomID, err := id.Parse(c.Params("roomId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var body UpdateRoomInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if err := h.svc.UpdateRoom(c.Context(), user.ID, spaceID, roomID, &body); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID, "room_id": roomID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// DeleteRoom DELETE /spaces/:id/rooms/:roomId
func (h *Handler) DeleteRoom(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	roomID, err := id.Parse(c.Params("roomId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	if err := h.svc.DeleteRoom(c.Context(), user.ID, spaceID, roomID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID, "room_id": roomID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// postRoomRequest matches client JSON: parent_id is a string snowflake (JS cannot safely use int64 in JSON).
type postRoomRequest struct {
	Name        string  `json:"name"`
	Type        int     `json:"type"`
	ParentID    *string `json:"parent_id,omitempty"`
	E2EEEnabled *bool   `json:"e2ee_enabled,omitempty"`
	UserLimit   *int    `json:"user_limit,omitempty"`
	Bitrate     *int    `json:"bitrate,omitempty"`
}

// PostRoom POST /spaces/:id/rooms
func (h *Handler) PostRoom(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var raw postRoomRequest
	if err := json.Unmarshal(c.Body(), &raw); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	body := CreateRoomInput{Name: raw.Name, Type: raw.Type, E2EEEnabled: raw.E2EEEnabled, UserLimit: raw.UserLimit, Bitrate: raw.Bitrate}
	if raw.ParentID != nil && strings.TrimSpace(*raw.ParentID) != "" {
		pid, err := id.Parse(strings.TrimSpace(*raw.ParentID))
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid parent_id"})
		}
		body.ParentID = &pid
	}
	room, err := h.svc.CreateRoom(c.Context(), user.ID, spaceID, &body)
	if err != nil {
		if err == ErrInvalidRoom {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "parent room not found"})
		}
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map(AttachOverrides(RoomMap(room), RoomOverrides{})))
}
