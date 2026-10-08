package threads

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func unauthorized(c fiber.Ctx) error {
	return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
}

func badID(c fiber.Ctx, what string) error {
	return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid " + what + " id"})
}

func fail(c fiber.Ctx, err error, fields map[string]any) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrMessageNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrMissingPerm), errors.Is(err, ErrNotMember), errors.Is(err, ErrNotInvitable), errors.Is(err, ErrLocked):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrNotThread), errors.Is(err, ErrNotTextChannel), errors.Is(err, ErrInvalidName),
		errors.Is(err, ErrInvalidAutoArchive), errors.Is(err, ErrAlreadyThread), errors.Is(err, ErrTooManyThreads),
		errors.Is(err, ErrArchived), errors.Is(err, ErrRemoteSpace), errors.Is(err, ErrCannotViewParent),
		errors.Is(err, ErrInvalidSlowmode), errors.Is(err, ErrSystemMessageThread):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	logger.Err("threads", err, fields)
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

func (h *Handler) threadResponse(c fiber.Ctx, status int, t *Thread) error {
	return c.Status(status).JSON(h.svc.ThreadJSON(t))
}

// CreateFromMessage POST /rooms/:id/messages/:msg_id/threads
func (h *Handler) CreateFromMessage(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "room")
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return badID(c, "message")
	}
	var in CreateInput
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &in); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
		}
	}
	t, err := h.svc.CreateFromMessage(c.Context(), user.ID, roomID, msgID, in)
	if err != nil {
		return fail(c, err, map[string]any{"room_id": roomID, "message_id": msgID})
	}
	return h.threadResponse(c, http.StatusCreated, t)
}

// Create POST /rooms/:id/threads
func (h *Handler) Create(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "room")
	}
	var in CreateInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	t, err := h.svc.Create(c.Context(), user.ID, roomID, in)
	if err != nil {
		return fail(c, err, map[string]any{"room_id": roomID})
	}
	return h.threadResponse(c, http.StatusCreated, t)
}

// Get GET /rooms/:id/thread
func (h *Handler) Get(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	threadID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "thread")
	}
	t, err := h.svc.Get(c.Context(), user.ID, threadID)
	if err != nil {
		return fail(c, err, map[string]any{"thread_id": threadID})
	}
	return h.threadResponse(c, http.StatusOK, t)
}

// Update PATCH /rooms/:id/thread
func (h *Handler) Update(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	threadID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "thread")
	}
	var in UpdateInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	t, err := h.svc.Update(c.Context(), user.ID, threadID, in)
	if err != nil {
		return fail(c, err, map[string]any{"thread_id": threadID})
	}
	return h.threadResponse(c, http.StatusOK, t)
}

// Delete DELETE /rooms/:id/thread
func (h *Handler) Delete(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	threadID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "thread")
	}
	if err := h.svc.Delete(c.Context(), user.ID, threadID); err != nil {
		return fail(c, err, map[string]any{"thread_id": threadID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListMembers GET /rooms/:id/thread-members
func (h *Handler) ListMembers(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	threadID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "thread")
	}
	members, err := h.svc.ListMembers(c.Context(), user.ID, threadID)
	if err != nil {
		return fail(c, err, map[string]any{"thread_id": threadID})
	}
	out := make([]fiber.Map, 0, len(members))
	for _, m := range members {
		item := fiber.Map{"user_id": id.Format(m.UserID), "joined_at": m.JoinedAt.UTC()}
		if m.User != nil {
			item["user"] = fiber.Map{
				"id": id.Format(m.User.ID), "username": m.User.Username, "display_name": m.User.DisplayName,
				"avatar": m.User.Avatar, "bot": m.User.Bot, "public_flags": auth.PublicFlags(m.User),
			}
		}
		out = append(out, item)
	}
	return c.JSON(out)
}

// Join PUT /rooms/:id/thread-members/@me
func (h *Handler) Join(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	threadID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "thread")
	}
	if err := h.svc.Join(c.Context(), user.ID, threadID); err != nil {
		return fail(c, err, map[string]any{"thread_id": threadID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// Leave DELETE /rooms/:id/thread-members/@me
func (h *Handler) Leave(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	threadID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "thread")
	}
	if err := h.svc.Leave(c.Context(), user.ID, threadID); err != nil {
		return fail(c, err, map[string]any{"thread_id": threadID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// AddMember PUT /rooms/:id/thread-members/:user_id
func (h *Handler) AddMember(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	threadID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "thread")
	}
	targetID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return badID(c, "user")
	}
	if err := h.svc.AddMember(c.Context(), user.ID, threadID, targetID); err != nil {
		return fail(c, err, map[string]any{"thread_id": threadID, "user_id": targetID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// RemoveMember DELETE /rooms/:id/thread-members/:user_id
func (h *Handler) RemoveMember(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	threadID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "thread")
	}
	targetID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return badID(c, "user")
	}
	if err := h.svc.RemoveMember(c.Context(), user.ID, threadID, targetID); err != nil {
		return fail(c, err, map[string]any{"thread_id": threadID, "user_id": targetID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListActive GET /spaces/:id/threads/active
func (h *Handler) ListActive(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "space")
	}
	list, err := h.svc.ListActive(c.Context(), user.ID, spaceID)
	if err != nil {
		return fail(c, err, map[string]any{"space_id": spaceID})
	}
	out := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		out = append(out, h.svc.ThreadJSON(&list[i]))
	}
	return c.JSON(fiber.Map{"threads": out})
}

// ListArchived GET /rooms/:id/threads/archived?before=<RFC3339>&limit=
func (h *Handler) ListArchived(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return unauthorized(c)
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return badID(c, "room")
	}
	var before *time.Time
	if raw := c.Query("before"); raw != "" {
		ts, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid before"})
		}
		before = &ts
	}
	limit := 25
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	list, hasMore, err := h.svc.ListArchived(c.Context(), user.ID, roomID, before, limit)
	if err != nil {
		return fail(c, err, map[string]any{"room_id": roomID})
	}
	out := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		out = append(out, h.svc.ThreadJSON(&list[i]))
	}
	return c.JSON(fiber.Map{"threads": out, "has_more": hasMore})
}
