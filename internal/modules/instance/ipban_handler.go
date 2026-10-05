package instance

import (
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// BlockBannedIPs refuses requests from a banned network. Mounted on /auth only - the two
// doors a banned person has (registering again, signing in to an alt) - and deliberately
// not on everything: an administrator who mistypes a range and bans their own address can
// still undo it from the session they already hold, and a mistaken ban never takes the
// instance's API down for whoever sits behind the same NAT with a live session.
func (h *Handler) BlockBannedIPs() fiber.Handler {
	return func(c fiber.Ctx) error {
		if b := h.svc.IPBanFor(c.Context(), c.IP()); b != nil {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{
				"error":  "this network is banned from this instance",
				"code":   "ip_banned",
				"reason": b.Reason,
			})
		}
		return c.Next()
	}
}

func (h *Handler) ListIPBans(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	bans, err := h.svc.ListIPBans(c.Context(), user.ID)
	if err != nil {
		return moderationError(c, err)
	}
	// Who banned it, hydrated in one read like the account bans page.
	ids := make([]int64, 0, len(bans))
	for _, b := range bans {
		ids = append(ids, b.BannedBy)
	}
	byID := map[int64]*auth.User{}
	if len(ids) > 0 && h.svc.mod != nil {
		if users, err := h.svc.mod.Users.GetByIDs(c.Context(), ids); err == nil {
			for _, u := range users {
				byID[u.ID] = u
			}
		}
	}
	out := make([]fiber.Map, 0, len(bans))
	for _, b := range bans {
		out = append(out, fiber.Map{"ban": b, "banned_by": userSummaryJSON(byID[b.BannedBy])})
	}
	return c.JSON(out)
}

func (h *Handler) BanIP(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in IPBanInput
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	b, err := h.svc.BanIP(c.Context(), user.ID, in)
	if err != nil {
		return moderationError(c, err)
	}
	return c.Status(http.StatusCreated).JSON(b)
}

// UnbanIP takes the range as ?cidr= rather than a path segment: a "/" inside a path
// parameter survives neither every proxy nor Fiber's router reliably.
func (h *Handler) UnbanIP(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if err := h.svc.UnbanIP(c.Context(), user.ID, c.Query("cidr")); err != nil {
		return moderationError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}
