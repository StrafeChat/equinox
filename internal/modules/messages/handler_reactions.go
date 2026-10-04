package messages

import (
	"net/http"
	"net/url"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// emojiParam decodes the `:emoji` route segment - the app runs with Fiber's UnescapePath
// off (the default), so a percent-encoded unicode emoji (e.g. "%F0%9F%91%8D") arrives at
// c.Params still escaped, not as the actual emoji bytes.
func emojiParam(c fiber.Ctx) (string, bool) {
	decoded, err := url.PathUnescape(c.Params("emoji"))
	if err != nil {
		return "", false
	}
	return decoded, true
}

// AddReaction handles PUT /rooms/:id/messages/:msg_id/reactions/:emoji - adds the caller's
// own reaction. `emoji` is either a raw unicode emoji or "custom:<id>".
func (h *Handler) AddReaction(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	emoji, ok := emojiParam(c)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": ErrInvalidReaction.Error()})
	}
	summary, err := h.svc.AddReaction(c.Context(), user.ID, roomID, msgID, emoji)
	if err != nil {
		return reactionError(c, err, roomID)
	}
	return c.JSON(fiber.Map{"reactions": summary})
}

// RemoveReaction handles DELETE /rooms/:id/messages/:msg_id/reactions/:emoji - removes the
// caller's own reaction (there is no route for removing someone else's).
func (h *Handler) RemoveReaction(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	emoji, ok := emojiParam(c)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": ErrInvalidReaction.Error()})
	}
	summary, err := h.svc.RemoveReaction(c.Context(), user.ID, roomID, msgID, emoji)
	if err != nil {
		return reactionError(c, err, roomID)
	}
	return c.JSON(fiber.Map{"reactions": summary})
}

// ListReactors handles GET /rooms/:id/messages/:msg_id/reactions/:emoji - who reacted with
// this specific emoji, for the hover tooltip.
func (h *Handler) ListReactors(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	emoji, ok := emojiParam(c)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": ErrInvalidReaction.Error()})
	}
	users, err := h.svc.ListReactors(c.Context(), user.ID, roomID, msgID, emoji)
	if err != nil {
		return reactionError(c, err, roomID)
	}
	out := make([]fiber.Map, 0, len(users))
	for _, u := range users {
		out = append(out, userSummary(u))
	}
	return c.JSON(out)
}

func reactionError(c fiber.Ctx, err error, roomID int64) error {
	if res, ok := originError(c, err); ok {
		return res
	}
	switch err {
	case ErrNotParticipant:
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
	case ErrForbidden:
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to react in this channel"})
	case ErrMessageNotFound:
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	case ErrInvalidReaction, ErrTooManyReactions:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	logger.Err("messages", err, map[string]any{"room_id": roomID})
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

func userSummary(u *auth.User) fiber.Map {
	if u == nil {
		return nil
	}
	return fiber.Map{
		"id":           id.Format(u.ID),
		"username":     u.Username,
		"display_name": u.DisplayName,
		"avatar":       u.Avatar,
		"bot":          u.Bot,
	}
}
