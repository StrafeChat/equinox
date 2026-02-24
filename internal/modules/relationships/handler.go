package relationships

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
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
// Body: { "username": "alice", "discriminator": "1234" } (both required; discriminator is string)
func (h *Handler) Post(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var in SendRequestInput
	if errs := ParseSendRequestBody(c.Body(), &in); errs != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": formatValidationErrors(errs)})
	}

	if err := h.svc.SendRequest(c.Context(), user.ID, in); err != nil {
		switch err {
		case ErrInvalidDiscriminator:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid discriminator"})
		case ErrUserNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
		case ErrSelfRequest:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "cannot send request to yourself"})
		case ErrAlreadyFriends:
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "already friends"})
		case ErrRequestExists:
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "request already sent"})
		default:
			logger.Err("relationships", err, map[string]any{"actor_id": user.ID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
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
		switch err {
		case ErrUserNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
		case ErrSelfRequest:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "cannot send request to yourself"})
		case ErrAlreadyFriends:
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "already friends"})
		case ErrRequestExists:
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "request already sent"})
		default:
			logger.Err("relationships", err, map[string]any{"actor_id": user.ID})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}

	return c.SendStatus(http.StatusNoContent)
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

// PutIgnore ignores a user.
func (h *Handler) PutIgnore(c fiber.Ctx) error {
	_ = auth.GetUser(c)
	_ = c.Params("user_id")
	return c.SendStatus(http.StatusNoContent)
}

// DeleteIgnore unignores a user.
func (h *Handler) DeleteIgnore(c fiber.Ctx) error {
	_ = auth.GetUser(c)
	_ = c.Params("user_id")
	return c.SendStatus(http.StatusNoContent)
}
