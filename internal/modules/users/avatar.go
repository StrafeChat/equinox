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

// PostAvatar accepts multipart field "file", uploads to Nebula, sets user avatar URL, broadcasts USER_UPDATE.
func (h *Handler) PostAvatar(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if h.cfg == nil || strings.TrimSpace(h.cfg.Nebula.BaseURL) == "" || strings.TrimSpace(h.cfg.Nebula.UploadSecret) == "" {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "avatar uploads are not configured"})
	}
	publicBase := strings.TrimRight(strings.TrimSpace(h.cfg.Nebula.PublicURL), "/")
	if publicBase == "" {
		publicBase = strings.TrimRight(strings.TrimSpace(h.cfg.Nebula.BaseURL), "/")
	}

	fh, err := c.FormFile("file")
	if err != nil || fh == nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "missing file field"})
	}
	if fh.Size <= 0 || fh.Size > int64(h.cfg.Nebula.AvatarMaxBytes) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid file size"})
	}

	ext := strings.ToLower(path.Ext(fh.Filename))
	if ext == "" {
		ext = ".png"
	}
	mime := avatarMIME[ext]
	if mime == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unsupported image type (use jpg, png, gif, or webp)"})
	}

	src, err := fh.Open()
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "could not read file"})
	}
	defer src.Close()

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		logger.Err("users", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	key := "avatars/" + id.Format(user.ID) + "/" + hex.EncodeToString(suffix[:]) + ext

	if err := nebula.Put(c.Context(), h.cfg.Nebula.BaseURL, h.cfg.Nebula.UploadSecret, key, src, mime, fh.Size); err != nil {
		logger.Err("users", err, map[string]any{"user_id": user.ID, "key": key})
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "could not store avatar"})
	}

	avatarURL := publicBase + "/v1/" + key
	upd := &auth.ProfileUpdate{Avatar: &avatarURL}
	updated, err := h.userRepo.UpdateProfile(c.Context(), user.ID, upd)
	if err != nil {
		logger.Err("users", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if updated == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}

	h.broadcastUserProfile(c.Context(), updated)

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
