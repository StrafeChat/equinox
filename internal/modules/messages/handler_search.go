package messages

import (
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// parseSearchQuery reads the shared `q`/`from`/`mentions`/`has`/`before`/`limit` operators
// off the query string. Returns nil and writes a 400 when one of the id operators is
// malformed.
func parseSearchQuery(c fiber.Ctx) (*SearchOptions, error) {
	opts := &SearchOptions{Query: c.Query("q"), Has: c.Query("has")}
	if from := c.Query("from"); from != "" {
		fid, err := id.Parse(from)
		if err != nil {
			return nil, c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid from"})
		}
		opts.FromUserID = &fid
	}
	if mentions := c.Query("mentions"); mentions != "" {
		mid, err := id.Parse(mentions)
		if err != nil {
			return nil, c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid mentions"})
		}
		opts.MentionsUserID = &mid
	}
	if before := c.Query("before"); before != "" {
		bid, err := id.Parse(before)
		if err != nil {
			return nil, c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid before"})
		}
		opts.BeforeID = &bid
	}
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			opts.Limit = n
		}
	}
	return opts, nil
}

// searchError maps the shared authorization failures onto responses.
func searchError(c fiber.Ctx, err error, fields map[string]any) error {
	if err == ErrNotParticipant {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
	}
	if err == ErrForbidden {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to read this channel"})
	}
	logger.Err("messages", err, fields)
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

// Search handles GET /rooms/:id/messages/search?q=&from=&mentions=&has=&before=&limit=
// Discord-style search operators, scoped to one room. E2EE rooms report
// {"searchable": false} instead of a match list - the server never sees their content.
func (h *Handler) Search(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	opts, bad := parseSearchQuery(c)
	if opts == nil {
		return bad
	}
	res, err := h.svc.SearchMessages(c.Context(), user.ID, roomID, opts)
	if err != nil {
		return searchError(c, err, map[string]any{"room_id": roomID})
	}
	if !res.Searchable {
		return c.JSON(fiber.Map{"searchable": false, "messages": []fiber.Map{}})
	}
	body := fiber.Map{"searchable": true, "messages": h.svc.MessagesJSON(c.Context(), res.Messages, nil)}
	if res.NextBeforeID != nil {
		body["next_before_id"] = id.Format(*res.NextBeforeID)
	}
	return c.JSON(body)
}

// SearchSpace handles GET /spaces/:id/messages/search - the same operators as Search plus
// `in=<room id>` (Discord's `in:` filter), merged across every text channel of the space
// the caller can read.
func (h *Handler) SearchSpace(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	base, bad := parseSearchQuery(c)
	if base == nil {
		return bad
	}
	opts := &SpaceSearchOptions{SearchOptions: *base}
	if in := c.Query("in"); in != "" {
		rid, err := id.Parse(in)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid in"})
		}
		opts.RoomID = &rid
	}
	res, err := h.svc.SearchSpaceMessages(c.Context(), user.ID, spaceID, opts)
	if err != nil {
		return searchError(c, err, map[string]any{"space_id": spaceID})
	}
	body := fiber.Map{
		"messages":        h.svc.MessagesJSON(c.Context(), res.Messages, nil),
		"rooms_searched":  res.RoomsSearched,
		"encrypted_rooms": res.EncryptedRooms,
	}
	if res.NextBeforeID != nil {
		body["next_before_id"] = id.Format(*res.NextBeforeID)
	}
	return c.JSON(body)
}
