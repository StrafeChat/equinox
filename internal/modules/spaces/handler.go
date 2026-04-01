package spaces

import (
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
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
		logger.Err("spaces", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusCreated).JSON(spaceToJSON(space))
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
	// Return space IDs and joined_at; client can then fetch full space details or we batch-load spaces
	out := make([]fiber.Map, 0, len(rows))
	for _, row := range rows {
		space, err := h.svc.GetSpace(c.Context(), row.SpaceID)
		if err != nil || space == nil {
			continue
		}
		out = append(out, spaceToJSON(space))
	}
	return c.JSON(out)
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
		if err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a member of this space"})
		}
		if err == ErrSpaceNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "space not found"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(spaceToJSON(space))
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
	roomList, err := h.svc.ListSpaceRooms(c.Context(), user.ID, spaceID)
	if err != nil {
		if err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a member of this space"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := make([]fiber.Map, 0, len(roomList))
	for _, r := range roomList {
		out = append(out, spaceRoomToJSON(r))
	}
	return c.JSON(out)
}

func spaceRoomToJSON(r *rooms.Room) fiber.Map {
	m := fiber.Map{
		"id":         id.Format(r.ID),
		"type":       r.Type,
		"name":       r.Name,
		"topic":      r.Topic,
		"position":   r.Position,
		"created_at": r.CreatedAt,
		"updated_at": r.UpdatedAt,
	}
	if r.SpaceID != nil {
		m["space_id"] = id.Format(*r.SpaceID)
	}
	if r.ParentID != nil {
		m["parent_id"] = id.Format(*r.ParentID)
	}
	if r.LastMessageID != nil {
		m["last_message_id"] = id.Format(*r.LastMessageID)
	}
	return m
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
	if s.EveryoneRoleID != 0 {
		m["everyone_role_id"] = id.Format(s.EveryoneRoleID)
	}
	return m
}

func spaceRoleToJSON(r *SpaceRole) fiber.Map {
	return fiber.Map{
		"id":           id.Format(r.ID),
		"name":         r.Name,
		"permissions":  r.Permissions,
		"position":     r.Position,
		"color":        r.Color,
		"hoist":        r.Hoist,
		"mentionable":  r.Mentionable,
		"created_at":   r.CreatedAt,
		"updated_at":   r.UpdatedAt,
	}
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
		if err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a member of this space"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
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
	inv, err := h.svc.CreateInvite(c.Context(), user.ID, spaceID)
	if err != nil {
		if err == ErrNotMember || err == ErrNotSpaceOwner {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{
		"code":      inv.Code,
		"space_id":  id.Format(inv.SpaceID),
		"inviter_id": id.Format(inv.InviterID),
		"created_at": inv.CreatedAt,
	})
}

// InvitePreview returns public space info for an invite (no auth). GET /spaces/invites/:code.
func (h *Handler) InvitePreview(c fiber.Ctx) error {
	code := c.Params("code")
	space, inviterName, err := h.svc.GetInvitePreview(c.Context(), code)
	if err != nil {
		if err == ErrInviteNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "invite not found"})
		}
		if err == ErrSpaceNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "space not found"})
		}
		logger.Err("spaces", err, map[string]any{"code": code})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := fiber.Map{
		"space":    spaceToJSON(space),
		"inviter":  nil,
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
		if err == ErrInviteNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "invite not found"})
		}
		logger.Err("spaces", err, map[string]any{"code": code})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusOK).JSON(spaceToJSON(space))
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
		if err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a member of this space"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
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
		if err == ErrMissingPerm || err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if err.Error() == "invalid role name" {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
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
		if err == ErrMissingPerm || err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if err == ErrRoleNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "role not found"})
		}
		if err == ErrCannotEditEveryone {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
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
		if err == ErrMissingPerm || err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if err == ErrCannotEditEveryone {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
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
		if err == ErrMissingPerm || err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if err == ErrRoleNotFound {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unknown role id"})
		}
		if err.Error() == "cannot change roles of the space owner" {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
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
		if err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a member of this space"})
		}
		if err == ErrInvalidRoom {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
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
		if err == ErrMissingPerm || err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if err == ErrInvalidRoom || err == ErrRoleNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
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
		if err == ErrMissingPerm || err == ErrNotMember {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if err == ErrInvalidRoom {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.SendStatus(http.StatusNoContent)
}
