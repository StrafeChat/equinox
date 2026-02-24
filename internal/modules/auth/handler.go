package auth

import (
	"context"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
)

type Handler struct {
	svc Service
}

func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Login(c fiber.Ctx) error {
	var in LoginInput
	if err := c.Bind().Body(&in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	ip := c.IP()
	userAgent := c.Get("User-Agent")

	user, token, err := h.svc.Login(context.Background(), in.Email, in.Password, ip, userAgent)
	if err != nil {
		if err == ErrInvalidCredentials {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid email or password"})
		}
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}

	return c.Status(http.StatusOK).JSON(fiber.Map{
		"token": token,
		"user": fiber.Map{
			"id":            id.Format(user.ID),
			"email":         user.Email,
			"username":      user.Username,
			"discriminator": user.Discriminator,
			"display_name":  user.DisplayName,
		},
	})
}

func (h *Handler) Logout(c fiber.Ctx) error {
	user := GetUser(c)
	session := GetSession(c)
	if user == nil || session == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	if err := h.svc.Logout(context.Background(), user.ID, session.SessionID); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"ok": true})
}

func (h *Handler) LogoutAll(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	if err := h.svc.LogoutAll(context.Background(), user.ID); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"ok": true})
}

func (h *Handler) Register(c fiber.Ctx) error {
	var in RegisterInput
	if err := c.Bind().Body(&in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid request body",
		})
	}

	user, err := h.svc.Register(context.Background(), in)
	if err != nil {
		switch err {
		case ErrInviteOnly:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "invite-only mode enabled"})
		case ErrEmailInUse:
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "email already in use"})
		case ErrDiscriminatorInUse:
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "discriminator already in use for this username"})
		case ErrWeakPassword, ErrInvalidUsername:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		default:
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}

	return c.Status(http.StatusCreated).JSON(fiber.Map{
		"id":            id.Format(user.ID),
		"email":         user.Email,
		"username":      user.Username,
		"discriminator": user.Discriminator,
		"display_name":  user.DisplayName,
		"created_at":    user.CreatedAt,
	})
}

// Locals keys used by auth middleware.
const (
	LocalsKeyUser    = "user"
	LocalsKeySession = "session"
)

func GetUser(c fiber.Ctx) *User {
	v := c.Locals(LocalsKeyUser)
	if u, ok := v.(*User); ok {
		return u
	}
	return nil
}

func GetSession(c fiber.Ctx) *Session {
	v := c.Locals(LocalsKeySession)
	if s, ok := v.(*Session); ok {
		return s
	}
	return nil
}