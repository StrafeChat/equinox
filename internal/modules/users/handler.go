package users

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/oauth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/nebula"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// ProfileFederator relays profile and presence changes to the instances holding a shadow
// of the user.
type ProfileFederator interface {
	AfterProfileUpdated(ctx context.Context, u *auth.User)
	AfterPresenceChanged(ctx context.Context, u *auth.User)
}

type Handler struct {
	userRepo  auth.UserRepository
	roomsRepo rooms.Repository
	redis     *redis.Client
	cfg       *config.Config
	federator ProfileFederator
	// apps is optional; set with SetApplications to serve bot profile edits (profile_bot.go).
	apps applications.Repository
}

func NewHandler(userRepo auth.UserRepository, roomsRepo rooms.Repository, redis *redis.Client, cfg *config.Config) *Handler {
	return &Handler{userRepo: userRepo, roomsRepo: roomsRepo, redis: redis, cfg: cfg}
}

func (h *Handler) SetFederator(f ProfileFederator) {
	h.federator = f
}

// Me returns the current user's info. Requires auth.
func (h *Handler) Me(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	// An OAuth2 access token only sees the account's email if it was granted the email
	// scope; a session (no scopes recorded) sees everything, as before.
	email, verified := user.Email, user.VerifiedEmail
	if scopes, ok := c.Locals(middleware.LocalsKeyScopes).([]string); ok && !oauth.HasScope(scopes, oauth.ScopeEmail) {
		email, verified = "", false
	}

	return c.JSON(fiber.Map{
		"id":             id.Format(user.ID),
		"email":          email,
		"verified_email": verified,
		"username":       user.Username,
		"discriminator":  fmt.Sprintf("%04d", user.Discriminator),
		"display_name":   user.DisplayName,
		"bio":            user.Bio,
		"about_me":       user.AboutMe,
		"avatar":         user.Avatar,
		"banner":         user.Banner,
		"accent_color":   user.AccentColor,
		"pronouns":       user.Pronouns,
		"birthday_opt_in": user.BirthdayOptIn,
		"birthday":       auth.BirthdayMMDD(user),
		"public_flags":   auth.PublicFlags(user),
		"bot":            user.Bot,
		"presence":       auth.ToPublicPresence(user.Presence, false),
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
	// Avatars and banners are uploaded through POST /users/@me/avatar|banner; PATCH may
	// only clear them or point at something this instance's CDN already hosts. An
	// arbitrary URL here would be loaded by everyone who views the profile - a tracking
	// pixel that reports each viewer's IP to whoever set it.
	if upd.Avatar != nil && !h.isOwnAssetURL(*upd.Avatar) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "avatar must be uploaded via POST /users/@me/avatar"})
	}
	if upd.Banner != nil && !h.isOwnAssetURL(*upd.Banner) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "banner must be uploaded via POST /users/@me/banner"})
	}

	updated, err := h.userRepo.UpdateProfile(c.Context(), user.ID, &upd)
	if err != nil {
		logger.Err("users", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if updated == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}

	// Opting into/out of birthdays adds or removes the user from the day index the daily
	// announcement job reads (only meaningful once a date of birth is set).
	if upd.BirthdayOptIn != nil && !updated.DateOfBirth.IsZero() {
		mo, d := int(updated.DateOfBirth.Month()), updated.DateOfBirth.Day()
		if err := h.userRepo.SetBirthdayIndex(c.Context(), updated.ID, mo, d, *upd.BirthdayOptIn); err != nil {
			logger.Err("users", err, map[string]any{"user_id": updated.ID})
		}
	}

	// Real-time: publish PRESENCE_UPDATE when presence changed (Redis Pub/Sub -> WebSocket)
	if upd.Presence != nil && h.redis != nil {
		region := "default"
		if h.cfg != nil {
			region = h.cfg.Stargate.Region
		}
		// Reaches the user's own devices (real status), friends, and every space they are
		// in (co-members), so a status change is seen everywhere - not just by friends.
		stargate.PublishPresenceUpdate(c.Context(), h.redis, region, updated)
		if h.federator != nil {
			h.federator.AfterPresenceChanged(c.Context(), updated)
		}
	}

	if hasProfileFieldsForRealtime(&upd) && h.redis != nil {
		h.broadcastUserProfile(c.Context(), updated)
	}

	return c.JSON(ownProfileJSON(updated))
}

// isOwnAssetURL reports whether u is empty (clear the field) or an object on this
// instance's own CDN.
func (h *Handler) isOwnAssetURL(u string) bool {
	if u == "" {
		return true
	}
	if h.cfg == nil {
		return false
	}
	base := nebula.PublicBase(h.cfg.Nebula.BaseURL, h.cfg.Nebula.PublicURL)
	return base != "" && strings.HasPrefix(u, base+"/v1/")
}

func hasProfileUpdate(u *auth.ProfileUpdate) bool {
	if u == nil {
		return false
	}
	if u.DisplayName != nil || u.Bio != nil || u.AboutMe != nil ||
		u.Avatar != nil || u.Banner != nil || u.AccentColor != nil || u.Presence != nil ||
		u.Pronouns != nil || u.BirthdayOptIn != nil {
		return true
	}
	return false
}

func hasProfileFieldsForRealtime(u *auth.ProfileUpdate) bool {
	if u == nil {
		return false
	}
	return u.DisplayName != nil || u.Bio != nil || u.AboutMe != nil ||
		u.Avatar != nil || u.Banner != nil || u.AccentColor != nil ||
		u.Pronouns != nil || u.BirthdayOptIn != nil
}
