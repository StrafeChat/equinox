package users

import (
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// A bot's profile - display name, about me, bio, accent colour, avatar and banner - is
// edited by the owner of its application, through the same profile machinery a person
// uses on their own account (same limits, same Nebula storage, same USER_UPDATE fan-out).
// These live in the users package because the applications package cannot import it
// (users -> oauth -> applications would cycle), so the applications store is injected.

// SetApplications enables the /applications/:id/bot/* profile endpoints.
func (h *Handler) SetApplications(apps applications.Repository) { h.apps = apps }

// botFor resolves the application in :id, checks the caller owns it and that it has a bot,
// and loads the bot account. ok=false means the error response was already written.
func (h *Handler) botFor(c fiber.Ctx) (*auth.User, bool) {
	user := auth.GetUser(c)
	if user == nil {
		_ = c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		return nil, false
	}
	if h.apps == nil {
		_ = c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
		return nil, false
	}
	appID, err := id.Parse(c.Params("id"))
	if err != nil {
		_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid application id"})
		return nil, false
	}
	app, err := h.apps.GetByID(c.Context(), appID)
	if err != nil {
		logger.Err("users", err, map[string]any{"application_id": appID})
		_ = c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		return nil, false
	}
	if app == nil {
		_ = c.Status(http.StatusNotFound).JSON(fiber.Map{"error": applications.ErrNotFound.Error()})
		return nil, false
	}
	if app.OwnerID != user.ID {
		_ = c.Status(http.StatusForbidden).JSON(fiber.Map{"error": applications.ErrNotOwner.Error()})
		return nil, false
	}
	if !app.HasBot() {
		_ = c.Status(http.StatusNotFound).JSON(fiber.Map{"error": applications.ErrNoBot.Error()})
		return nil, false
	}
	bot, err := h.userRepo.GetByID(c.Context(), app.BotUserID)
	if err != nil {
		logger.Err("users", err, map[string]any{"bot_user_id": app.BotUserID})
		_ = c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		return nil, false
	}
	if bot == nil || !bot.Bot {
		_ = c.Status(http.StatusNotFound).JSON(fiber.Map{"error": applications.ErrNoBot.Error()})
		return nil, false
	}
	return bot, true
}

// BotProfileJSON is the bot's public profile, as the application endpoints return it.
func BotProfileJSON(u *auth.User) fiber.Map {
	return fiber.Map{
		"id":           id.Format(u.ID),
		"username":     u.Username,
		"display_name": u.DisplayName,
		"avatar":       u.Avatar,
		"banner":       u.Banner,
		"bio":          u.Bio,
		"about_me":     u.AboutMe,
		"accent_color": u.AccentColor,
		"public_flags": auth.PublicFlags(u),
		"bot":          true,
	}
}

// PatchBotProfile PATCH /applications/:id/bot - display_name, bio, about_me, accent_color.
// Avatar and banner go through the upload endpoints; a bot has no presence to set.
func (h *Handler) PatchBotProfile(c fiber.Ctx) error {
	bot, ok := h.botFor(c)
	if !ok {
		return nil
	}
	var upd auth.ProfileUpdate
	msg, valid := ParsePatchMeBody(c.Body(), &upd)
	if !valid {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": msg})
	}
	if upd.Avatar != nil || upd.Banner != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "upload the avatar or banner with POST /applications/:id/bot/avatar|banner"})
	}
	upd.Presence = nil
	if !hasProfileUpdate(&upd) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "no fields to update"})
	}
	updated, err := h.userRepo.UpdateProfile(c.Context(), bot.ID, &upd)
	if err != nil {
		logger.Err("users", err, map[string]any{"bot_user_id": bot.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if updated == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	if h.redis != nil {
		h.broadcastUserProfile(c.Context(), updated)
	}
	return c.JSON(BotProfileJSON(updated))
}

// PostBotAvatar POST /applications/:id/bot/avatar (multipart "file").
func (h *Handler) PostBotAvatar(c fiber.Ctx) error {
	bot, ok := h.botFor(c)
	if !ok {
		return nil
	}
	updated := h.uploadProfileImage(c, bot, imageAvatar)
	if updated == nil {
		return nil
	}
	return c.JSON(BotProfileJSON(updated))
}

// PostBotBanner POST /applications/:id/bot/banner (multipart "file").
func (h *Handler) PostBotBanner(c fiber.Ctx) error {
	bot, ok := h.botFor(c)
	if !ok {
		return nil
	}
	updated := h.uploadProfileImage(c, bot, imageBanner)
	if updated == nil {
		return nil
	}
	return c.JSON(BotProfileJSON(updated))
}
