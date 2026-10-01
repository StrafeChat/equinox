package relationships

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// HandleResolver turns "name#0001@domain" into a local user row (a shadow for remote
// users). Provided by internal/federation; without it only local handles resolve.
type HandleResolver interface {
	ResolveHandle(ctx context.Context, handle string) (*auth.User, error)
}

// ResolveError is a handle lookup failure carrying the status it should be reported with,
// so a resolver can distinguish "no such user" from "their instance is unreachable"
// without this package knowing the federation error values.
type ResolveError struct {
	Status  int
	Message string
}

func (e *ResolveError) Error() string { return e.Message }

type Handler struct {
	svc      *Service
	resolver HandleResolver
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) SetHandleResolver(r HandleResolver) {
	h.resolver = r
}

// resolveHandle finds the user behind a handle. Handles with a domain need federation.
func (h *Handler) resolveHandle(c fiber.Ctx, handle string) (*auth.User, int, string) {
	handle = strings.TrimSpace(handle)
	if h.resolver != nil {
		u, err := h.resolver.ResolveHandle(c.Context(), handle)
		if err != nil {
			var re *ResolveError
			if errors.As(err, &re) {
				return nil, re.Status, re.Message
			}
			return nil, http.StatusNotFound, err.Error()
		}
		return u, 0, ""
	}
	if strings.Contains(handle, "@") {
		return nil, http.StatusBadRequest, "this instance does not federate; use a local username#0001"
	}
	name, disc, ok := strings.Cut(strings.TrimPrefix(handle, "@"), "#")
	d, err := parseDiscriminator(strings.TrimSpace(disc))
	if !ok || err != nil || strings.TrimSpace(name) == "" {
		return nil, http.StatusBadRequest, "expected username#0001"
	}
	u, err := h.svc.FindLocalUser(c.Context(), strings.TrimSpace(name), d)
	if err != nil {
		return nil, http.StatusInternalServerError, "internal error"
	}
	if u == nil {
		return nil, http.StatusNotFound, "user not found"
	}
	return u, 0, ""
}

// Get returns all relationships.
func (h *Handler) Get(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	data, err := h.svc.ListRelationships(c.Context(), user.ID)
	if err != nil {
		logger.Err("relationships", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}

	return c.JSON(data)
}

// Post sends a friend request.
// Body: { "handle": "alice#1234" } or { "handle": "alice#1234@other.instance" };
// { "username": "alice", "discriminator": "1234" } is still accepted.
func (h *Handler) Post(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var in SendRequestInput
	if errs := ParseSendRequestBody(c.Body(), &in); errs != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": formatValidationErrors(errs)})
	}
	handle := in.Handle
	if handle == "" {
		if in.Username == "" || in.Discriminator == "" {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "handle is required"})
		}
		handle = in.Username + "#" + strings.TrimPrefix(in.Discriminator, "#")
	}
	target, status, msg := h.resolveHandle(c, handle)
	if target == nil {
		return c.Status(status).JSON(fiber.Map{"error": msg})
	}

	if err := h.svc.SendRequestTo(c.Context(), user.ID, target); err != nil {
		return sendRequestError(c, err, user.ID)
	}
	return c.SendStatus(http.StatusNoContent)
}

// PutByID creates a relationship by user id.
func (h *Handler) PutByID(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	targetID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}

	if err := h.svc.SendRequestByID(c.Context(), user.ID, targetID); err != nil {
		return sendRequestError(c, err, user.ID)
	}
	return c.SendStatus(http.StatusNoContent)
}

func sendRequestError(c fiber.Ctx, err error, actorID int64) error {
	switch err {
	case ErrUserNotFound:
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	case ErrSelfRequest:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "cannot send request to yourself"})
	case ErrBotTarget:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": ErrBotTarget.Error()})
	case ErrAlreadyFriends:
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "already friends"})
	case ErrRequestExists:
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "request already sent"})
	case ErrBlocked:
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "you cannot add this user"})
	default:
		logger.Err("relationships", err, map[string]any{"actor_id": actorID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
}

// Delete removes a relationship.
func (h *Handler) Delete(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	targetID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}

	if err := h.svc.Delete(c.Context(), user.ID, targetID); err != nil {
		switch err {
		case ErrRequestNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "relationship not found"})
		default:
			logger.Err("relationships", err, nil)
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}

	return c.SendStatus(http.StatusNoContent)
}

// PutBlock blocks a user, removing any friendship or pending request between them.
func (h *Handler) PutBlock(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	targetID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	if err := h.svc.Block(c.Context(), user.ID, targetID); err != nil {
		switch err {
		case ErrSelfRequest:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "cannot block yourself"})
		case ErrUserNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
		default:
			logger.Err("relationships", err, map[string]any{"actor_id": user.ID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}
	return c.SendStatus(http.StatusNoContent)
}

// DeleteBlock unblocks a user.
func (h *Handler) DeleteBlock(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	targetID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	if err := h.svc.Unblock(c.Context(), user.ID, targetID); err != nil {
		logger.Err("relationships", err, map[string]any{"actor_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.SendStatus(http.StatusNoContent)
}

// Patch modifies relationship metadata (nickname).
func (h *Handler) Patch(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	targetID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}

	var nickname *string
	if body := c.Body(); len(body) > 0 {
		var in struct {
			Nickname *string `json:"nickname"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
		}
		if in.Nickname != nil {
			s := *in.Nickname
			if len(s) > 32 {
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "nickname must be 0-32 characters"})
			}
			nickname = in.Nickname
		}
	}

	if err := h.svc.Patch(c.Context(), user.ID, targetID, nickname); err != nil {
		switch err {
		case ErrRequestNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not friends"})
		default:
			logger.Err("relationships", err, nil)
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}

	return c.SendStatus(http.StatusNoContent)
}

// BulkDelete removes multiple relationships.
func (h *Handler) BulkDelete(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	relType := TypeIncomingRequest
	if s := c.Query("relationship_type", "3"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			relType = n
		}
	}
	if err := h.svc.BulkDelete(c.Context(), user.ID, relType); err != nil {
		if err == ErrRequestNotFound {
			return c.SendStatus(http.StatusNoContent)
		}
		logger.Err("relationships", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.SendStatus(http.StatusNoContent)
}

// PutIgnore ignores a user. Not implemented yet: answer honestly instead of a 204 that
// would make a client believe the user is now ignored.
func (h *Handler) PutIgnore(c fiber.Ctx) error {
	return c.Status(http.StatusNotImplemented).JSON(fiber.Map{"error": "ignoring users is not implemented yet"})
}

// DeleteIgnore unignores a user. See PutIgnore.
func (h *Handler) DeleteIgnore(c fiber.Ctx) error {
	return c.Status(http.StatusNotImplemented).JSON(fiber.Map{"error": "ignoring users is not implemented yet"})
}
