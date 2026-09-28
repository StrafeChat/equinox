package spaces

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/nebula"
)

var emojiMIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

func spaceEmojiToJSON(e *SpaceEmoji) fiber.Map {
	return fiber.Map{
		"id":         id.Format(e.ID),
		"space_id":   id.Format(e.SpaceID),
		"name":       e.Name,
		"url":        e.URL,
		"animated":   e.Animated,
		"creator_id": id.Format(e.CreatorID),
		"created_at": e.CreatedAt,
		"updated_at": e.UpdatedAt,
	}
}

func spaceEmojiRefToJSON(e *SpaceEmojiRef) fiber.Map {
	return fiber.Map{
		"id":       id.Format(e.ID),
		"space_id": id.Format(e.SpaceID),
		"name":     e.Name,
		"url":      e.URL,
		"animated": e.Animated,
	}
}

// ListEmojis GET /spaces/:id/emojis
func (h *Handler) ListEmojis(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	list, err := h.svc.ListEmojis(c.Context(), user.ID, spaceID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	out := make([]fiber.Map, 0, len(list))
	for i := range list {
		out = append(out, spaceEmojiToJSON(&list[i]))
	}
	return c.JSON(out)
}

// ListMyEmojis GET /emojis - every custom emoji from every space the caller is in.
func (h *Handler) ListMyEmojis(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	list, err := h.svc.ListEmojisForUser(c.Context(), user.ID)
	if err != nil {
		return spaceError(c, err, map[string]any{"user_id": user.ID})
	}
	out := make([]fiber.Map, 0, len(list))
	for i := range list {
		out = append(out, spaceEmojiToJSON(&list[i]))
	}
	return c.JSON(out)
}

// GetEmojiByID GET /emojis/:emojiId - resolve an emoji used outside its home space.
func (h *Handler) GetEmojiByID(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	emojiID, err := id.Parse(c.Params("emojiId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid emoji id"})
	}
	ref, err := h.svc.LookupEmoji(c.Context(), emojiID)
	if err != nil {
		return spaceError(c, err, map[string]any{"emoji_id": emojiID})
	}
	return c.JSON(spaceEmojiRefToJSON(ref))
}

// PostEmoji POST /spaces/:id/emojis (multipart: "file", "name"). Permission is checked
// before the upload so an unauthorised caller can't fill Nebula with images.
func (h *Handler) PostEmoji(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	cfg := h.cfg
	if cfg == nil || strings.TrimSpace(cfg.Nebula.BaseURL) == "" || strings.TrimSpace(cfg.Nebula.UploadSecret) == "" {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "file uploads are not configured"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	if err := h.svc.CanManageEmojis(c.Context(), user.ID, spaceID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	name, err := ValidateEmojiName(c.FormValue("name"))
	if err != nil {
		return spaceError(c, err, nil)
	}
	fh, err := c.FormFile("file")
	if err != nil || fh == nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "missing file field"})
	}
	maxBytes := cfg.Nebula.EmojiMaxBytes
	if maxBytes <= 0 {
		maxBytes = 512 * 1024
	}
	if fh.Size <= 0 || fh.Size > int64(maxBytes) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "emoji images must be under " + formatKB(maxBytes)})
	}
	ext := strings.ToLower(path.Ext(fh.Filename))
	mime := emojiMIME[ext]
	if mime == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unsupported image type (use png, jpg, gif, or webp)"})
	}
	src, err := fh.Open()
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "could not read file"})
	}
	defer src.Close()

	emojiID := id.Next()
	key := "emojis/" + id.Format(spaceID) + "/" + id.Format(emojiID) + ext
	if err := nebula.Put(c.Context(), cfg.Nebula.BaseURL, cfg.Nebula.UploadSecret, key, src, mime, fh.Size); err != nil {
		logger.Err("spaces", err, map[string]any{"space_id": spaceID, "key": key})
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "could not store emoji"})
	}
	url := nebula.ObjectURL(cfg.Nebula.BaseURL, cfg.Nebula.PublicURL, key)
	e, err := h.svc.CreateEmoji(c.Context(), user.ID, spaceID, emojiID, name, url, ext == ".gif")
	if err != nil {
		_ = nebula.Delete(c.Context(), cfg.Nebula.BaseURL, cfg.Nebula.UploadSecret, key)
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.Status(http.StatusCreated).JSON(spaceEmojiToJSON(e))
}

// PatchEmoji PATCH /spaces/:id/emojis/:emojiId {"name": "..."}
func (h *Handler) PatchEmoji(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	emojiID, err := id.Parse(c.Params("emojiId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid emoji id"})
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	e, err := h.svc.RenameEmoji(c.Context(), user.ID, spaceID, emojiID, body.Name)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID, "emoji_id": emojiID})
	}
	return c.JSON(spaceEmojiToJSON(e))
}

// DeleteEmoji DELETE /spaces/:id/emojis/:emojiId
func (h *Handler) DeleteEmoji(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	emojiID, err := id.Parse(c.Params("emojiId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid emoji id"})
	}
	e, err := h.svc.DeleteEmoji(c.Context(), user.ID, spaceID, emojiID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID, "emoji_id": emojiID})
	}
	// Existing messages keep rendering the old URL until the blob is gone; that's the
	// intended "deleted emoji" behaviour (they fall back to :name:).
	if h.cfg != nil && strings.TrimSpace(h.cfg.Nebula.BaseURL) != "" {
		if key := nebula.KeyFromURL(e.URL); key != "" {
			if err := nebula.Delete(c.Context(), h.cfg.Nebula.BaseURL, h.cfg.Nebula.UploadSecret, key); err != nil {
				logger.Err("spaces", err, map[string]any{"space_id": spaceID, "key": key})
			}
		}
	}
	return c.SendStatus(http.StatusNoContent)
}

func formatKB(bytes int) string {
	kb := bytes / 1024
	if kb%1024 == 0 && kb >= 1024 {
		return itoa(kb/1024) + " MB"
	}
	return itoa(kb) + " KB"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
