package spaces

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/nebula"
)

var spaceIconMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// PostSpaceIcon uploads an image to Nebula and sets the space icon. POST /spaces/:id/icon (multipart field "file").
func (h *Handler) PostSpaceIcon(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	cfg := h.cfg
	if cfg == nil || strings.TrimSpace(cfg.Nebula.BaseURL) == "" || strings.TrimSpace(cfg.Nebula.UploadSecret) == "" {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "file uploads are not configured"})
	}
	publicBase := strings.TrimRight(strings.TrimSpace(cfg.Nebula.PublicURL), "/")
	if publicBase == "" {
		publicBase = strings.TrimRight(strings.TrimSpace(cfg.Nebula.BaseURL), "/")
	}

	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}

	fh, err := c.FormFile("file")
	if err != nil || fh == nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "missing file field"})
	}
	if fh.Size <= 0 || fh.Size > int64(cfg.Nebula.AvatarMaxBytes) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid file size"})
	}

	ext := strings.ToLower(path.Ext(fh.Filename))
	if ext == "" {
		ext = ".png"
	}
	mime := spaceIconMIME[ext]
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
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	key := "spaces/icons/" + id.Format(spaceID) + "/" + hex.EncodeToString(suffix[:]) + ext

	if err := nebula.Put(c.Context(), cfg.Nebula.BaseURL, cfg.Nebula.UploadSecret, key, src, mime, fh.Size); err != nil {
		logger.Err("spaces", err, map[string]any{"space_id": spaceID, "key": key})
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "could not store icon"})
	}

	iconURL := publicBase + "/v1/" + key
	space, err := h.svc.UpdateSpaceIcon(c.Context(), user.ID, spaceID, iconURL)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a member of this space"})
		}
		if errors.Is(err, ErrInsufficientSpacePermission) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if errors.Is(err, ErrSpaceNotFound) {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "space not found"})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(spaceToJSON(space))
}
