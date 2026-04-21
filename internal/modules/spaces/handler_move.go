package spaces

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// MoveChannel POST /spaces/:id/rooms/move — body: MoveChannelInput (registered before /rooms/:roomId).
func (h *Handler) MoveChannel(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var body MoveChannelInput
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	channelID, err := id.Parse(strings.TrimSpace(body.ChannelID))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid channel_id"})
	}
	var parentSectionID *int64
	if body.ParentSectionID != nil && strings.TrimSpace(*body.ParentSectionID) != "" {
		pid, err := id.Parse(strings.TrimSpace(*body.ParentSectionID))
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid parent_section_id"})
		}
		parentSectionID = &pid
	}
	var beforeRoomID *int64
	if body.BeforeRoomID != nil && strings.TrimSpace(*body.BeforeRoomID) != "" {
		bid, err := id.Parse(strings.TrimSpace(*body.BeforeRoomID))
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid before_room_id"})
		}
		beforeRoomID = &bid
	}
	if err := h.svc.MoveSpaceChannel(c.Context(), user.ID, spaceID, channelID, parentSectionID, beforeRoomID); err != nil {
		if errors.Is(err, ErrMissingPerm) || errors.Is(err, ErrNotMember) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if errors.Is(err, ErrSpaceNotFound) {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "space not found"})
		}
		if errors.Is(err, ErrInvalidRoom) || errors.Is(err, ErrInvalidReorder) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.SendStatus(http.StatusNoContent)
}
