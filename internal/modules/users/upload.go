package users

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/nebula"
)

var avatarMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// profileImageKind is which profile image an upload replaces: "avatars" or "banners" (the
// Nebula key prefix), each with its own size cap.
type profileImageKind string

const (
	imageAvatar profileImageKind = "avatars"
	imageBanner profileImageKind = "banners"
)

// uploadProfileImage takes the multipart "file" field, stores it on Nebula under
// <kind>/<user id>/<random>.<ext>, points the user's avatar or banner at it and broadcasts
// USER_UPDATE. Shared by a person's own profile and by a bot's (edited by its owner). On
// failure it has already written the error response and returns nil.
func (h *Handler) uploadProfileImage(c fiber.Ctx, target *auth.User, kind profileImageKind) *auth.User {
	if h.cfg == nil || strings.TrimSpace(h.cfg.Nebula.BaseURL) == "" || strings.TrimSpace(h.cfg.Nebula.UploadSecret) == "" {
		_ = c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "image uploads are not configured"})
		return nil
	}
	publicBase := strings.TrimRight(strings.TrimSpace(h.cfg.Nebula.PublicURL), "/")
	if publicBase == "" {
		publicBase = strings.TrimRight(strings.TrimSpace(h.cfg.Nebula.BaseURL), "/")
	}
	maxBytes := h.cfg.Nebula.AvatarMaxBytes
	if kind == imageBanner && h.cfg.Nebula.BannerMaxBytes > 0 {
		maxBytes = h.cfg.Nebula.BannerMaxBytes
	}

	fh, err := c.FormFile("file")
	if err != nil || fh == nil {
		_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "missing file field"})
		return nil
	}
	if fh.Size <= 0 || fh.Size > int64(maxBytes) {
		_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid file size"})
		return nil
	}
	ext := strings.ToLower(path.Ext(fh.Filename))
	if ext == "" {
		ext = ".png"
	}
	mime := avatarMIME[ext]
	if mime == "" {
		_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unsupported image type (use jpg, png, gif, or webp)"})
		return nil
	}
	src, err := fh.Open()
	if err != nil {
		_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "could not read file"})
		return nil
	}
	defer src.Close()

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		logger.Err("users", err, map[string]any{"user_id": target.ID})
		_ = c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		return nil
	}
	key := string(kind) + "/" + id.Format(target.ID) + "/" + hex.EncodeToString(suffix[:]) + ext
	if err := nebula.Put(c.Context(), h.cfg.Nebula.BaseURL, h.cfg.Nebula.UploadSecret, key, src, mime, fh.Size); err != nil {
		logger.Err("users", err, map[string]any{"user_id": target.ID, "key": key})
		_ = c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "could not store image"})
		return nil
	}

	url := publicBase + "/v1/" + key
	upd := &auth.ProfileUpdate{}
	if kind == imageBanner {
		upd.Banner = &url
	} else {
		upd.Avatar = &url
	}
	updated, err := h.userRepo.UpdateProfile(c.Context(), target.ID, upd)
	if err != nil {
		logger.Err("users", err, map[string]any{"user_id": target.ID})
		_ = c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		return nil
	}
	if updated == nil {
		_ = c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
		return nil
	}
	h.broadcastUserProfile(c.Context(), updated)
	return updated
}

// ownProfileJSON is the self view returned by the profile endpoints.
func ownProfileJSON(u *auth.User) fiber.Map {
	return fiber.Map{
		"id":             id.Format(u.ID),
		"email":          u.Email,
		"verified_email": u.VerifiedEmail,
		"username":       u.Username,
		"discriminator":  fmt.Sprintf("%04d", u.Discriminator),
		"display_name":   u.DisplayName,
		"bio":            u.Bio,
		"about_me":       u.AboutMe,
		"avatar":         u.Avatar,
		"banner":         u.Banner,
		"accent_color":   u.AccentColor,
		"presence":       auth.ToPublicPresence(u.Presence, false),
	}
}
