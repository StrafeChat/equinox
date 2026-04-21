package users

import (
	"fmt"
	"net/http"

	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

type Handler struct {
	userRepo  auth.UserRepository
	roomsRepo rooms.Repository
	redis     *redis.Client
	cfg       *config.Config
}

func NewHandler(userRepo auth.UserRepository, roomsRepo rooms.Repository, redis *redis.Client, cfg *config.Config) *Handler {
	return &Handler{userRepo: userRepo, roomsRepo: roomsRepo, redis: redis, cfg: cfg}
}

// Me returns the current user's info. Requires auth.
func (h *Handler) Me(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	return c.JSON(fiber.Map{
		"id":            id.Format(user.ID),
		"email":         user.Email,
		"username":      user.Username,
		"discriminator": fmt.Sprintf("%04d", user.Discriminator),
		"display_name":  user.DisplayName,
		"bio":           user.Bio,
		"about_me":      user.AboutMe,
		"avatar":        user.Avatar,
		"banner":        user.Banner,
		"accent_color":  user.AccentColor,
		"presence":      auth.ToPublicPresence(user.Presence, false),
	})
}

// PatchMe updates the current user's profile. Requires auth.
func (h *Handler) PatchMe(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var upd auth.ProfileUpdate
	msg, ok := ParsePatchMeBody(c.Body(), &upd)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": msg})
	}
	if !hasProfileUpdate(&upd) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "no fields to update"})
	}

	updated, err := h.userRepo.UpdateProfile(c.Context(), user.ID, &upd)
	if err != nil {
		logger.Err("users", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if updated == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}

	// Real-time: publish PRESENCE_UPDATE when presence changed (Redis Pub/Sub -> WebSocket)
	if upd.Presence != nil && h.redis != nil {
		region := "default"
		if h.cfg != nil {
			region = h.cfg.Stargate.Region
		}
		// Self sees actual status; friends see public (invisible → offline)
		presenceSelf := auth.ToPublicPresence(updated.Presence, false)
		presenceOthers := auth.ToPublicPresence(updated.Presence, true)
		stargate.PublishToUser(c.Context(), h.redis, updated.ID, "PRESENCE_UPDATE", map[string]interface{}{
			"user_id":  id.Format(updated.ID),
			"presence": presenceSelf,
		}, region)
		friendIDs := updated.Relationships
		if len(friendIDs) > 0 {
			stargate.PublishToUsers(c.Context(), h.redis, friendIDs, "PRESENCE_UPDATE", map[string]interface{}{
				"user_id":  id.Format(updated.ID),
				"presence": presenceOthers,
			}, region)
		}
	}

	if hasProfileFieldsForRealtime(&upd) && h.redis != nil {
		h.broadcastUserProfile(c.Context(), updated)
	}

	return c.JSON(fiber.Map{
		"id":            id.Format(updated.ID),
		"email":         updated.Email,
		"username":      updated.Username,
		"discriminator": fmt.Sprintf("%04d", updated.Discriminator),
		"display_name":  updated.DisplayName,
		"bio":           updated.Bio,
		"about_me":      updated.AboutMe,
		"avatar":        updated.Avatar,
		"banner":        updated.Banner,
		"accent_color":  updated.AccentColor,
		"presence":      auth.ToPublicPresence(updated.Presence, false),
	})
}

func hasProfileUpdate(u *auth.ProfileUpdate) bool {
	if u == nil {
		return false
	}
	if u.DisplayName != nil || u.Bio != nil || u.AboutMe != nil ||
		u.Avatar != nil || u.Banner != nil || u.AccentColor != nil || u.Presence != nil {
		return true
	}
	return false
}

func hasProfileFieldsForRealtime(u *auth.ProfileUpdate) bool {
	if u == nil {
		return false
	}
	return u.DisplayName != nil || u.Bio != nil || u.AboutMe != nil ||
		u.Avatar != nil || u.Banner != nil || u.AccentColor != nil
}
