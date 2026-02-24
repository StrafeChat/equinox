package auth

import (
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
)

type Handler struct {
	svc Service
}

func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Login(c fiber.Ctx) error {
	var in LoginInput
	if errs := ParseLoginBody(c.Body(), &in); errs != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": formatValidationErrors(errs)})
	}

	ip := c.IP()
	userAgent := c.Get("User-Agent")

	user, token, err := h.svc.Login(c.Context(), in.Email, in.Password, ip, userAgent)
	if err != nil {
		if err == ErrInvalidCredentials {
			logger.Info("auth", "login failed: invalid credentials")
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid email or password"})
		}
		logger.Err("auth", err, map[string]any{"email": in.Email})
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

	if err := h.svc.Logout(c.Context(), user.ID, session.SessionID); err != nil {
		logger.Err("auth", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"ok": true})
}

func (h *Handler) LogoutAll(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	if err := h.svc.LogoutAll(c.Context(), user.ID); err != nil {
		logger.Err("auth", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"ok": true})
}

func (h *Handler) Register(c fiber.Ctx) error {
	var in RegisterInput
	if errs := ParseRegisterBody(c.Body(), &in); errs != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": formatValidationErrors(errs)})
	}

	user, err := h.svc.Register(c.Context(), in)
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
			logger.Err("auth", err, map[string]any{"email": in.Email})
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