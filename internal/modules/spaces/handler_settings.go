package spaces

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// ListInvites GET /spaces/:id/invites - live invites with their inviters. Manage space.
func (h *Handler) ListInvites(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	invites, users, err := h.svc.ListInvites(c.Context(), user.ID, spaceID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	out := make([]fiber.Map, 0, len(invites))
	for i := range invites {
		out = append(out, inviteToJSON(&invites[i], users[invites[i].InviterID]))
	}
	return c.JSON(out)
}

// DeleteInvite DELETE /spaces/:id/invites/:code - revoke an invite (Manage space, or its creator).
func (h *Handler) DeleteInvite(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	// A federated code is code@domain, with the "@" percent-encoded in the path.
	code := strings.TrimSpace(inviteCodeParam(c))
	if code == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid invite code"})
	}
	if err := h.svc.DeleteInvite(c.Context(), user.ID, spaceID, code); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID, "code": code})
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListAuditLog GET /spaces/:id/audit-log?before=<entry id>&limit=50&action=<type>.
// Response: { entries: [...], users: { "<id>": {...} } }. Manage space.
func (h *Handler) ListAuditLog(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var before int64
	if raw := strings.TrimSpace(c.Query("before")); raw != "" {
		before, err = id.Parse(raw)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid before"})
		}
	}
	limit := 0
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid limit"})
		}
	}
	action := strings.TrimSpace(c.Query("action"))
	entries, users, err := h.svc.ListAuditLog(c.Context(), user.ID, spaceID, before, limit, action)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	outEntries := make([]fiber.Map, 0, len(entries))
	for _, e := range entries {
		m := fiber.Map{
			"id":          id.Format(e.ID),
			"action_type": e.ActionType,
			"user_id":     id.Format(e.UserID),
			"target_id":   e.TargetID,
			"created_at":  e.CreatedAt,
		}
		if e.Changes != "" && json.Valid([]byte(e.Changes)) {
			// Stored as a JSON object; embedded as-is.
			m["changes"] = json.RawMessage(e.Changes)
		}
		if e.Reason != "" {
			m["reason"] = e.Reason
		}
		outEntries = append(outEntries, m)
	}
	outUsers := make(map[string]fiber.Map, len(users))
	for uid, u := range users {
		outUsers[id.Format(uid)] = userSummary(u)
	}
	return c.JSON(fiber.Map{"entries": outEntries, "users": outUsers})
}

// Widget GET /spaces/:id/widget.json - public, no auth. 404 unless the widget is enabled.
func (h *Handler) Widget(c fiber.Ctx) error {
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "space not found"})
	}
	info, err := h.svc.Widget(c.Context(), spaceID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	out := fiber.Map{
		"id":             id.Format(info.ID),
		"name":           info.Name,
		"icon":           info.Icon,
		"description":    info.Description,
		"member_count":   info.MemberCount,
		"presence_count": info.PresenceCount,
		"instant_invite": nil,
	}
	if info.InviteCode != "" {
		out["instant_invite"] = info.InviteCode
		out["invite_room_name"] = info.RoomName
	}
	c.Set("Cache-Control", "public, max-age=60")
	c.Set("Access-Control-Allow-Origin", "*")
	return c.JSON(out)
}
