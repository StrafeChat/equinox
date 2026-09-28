package auth

import (
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/captcha"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
)

type Handler struct {
	svc Service
	// captcha is nil unless the instance enabled a registration challenge.
	captcha captcha.Verifier
	// challenger is set when the provider issues its own challenges (ALTCHA); the hosted
	// providers hand the browser a site key instead and leave this nil.
	challenger captcha.Challenger
}

// NewHandlerWithCaptcha wires an optional registration challenge. Pass nil to disable.
func NewHandlerWithCaptcha(svc Service, v captcha.Verifier) *Handler {
	h := NewHandler(svc)
	h.captcha = v
	if ch, ok := v.(captcha.Challenger); ok {
		h.challenger = ch
	}
	return h
}

// CaptchaChallenge issues a fresh challenge for a self-hosted provider.
// GET /auth/captcha/challenge
func (h *Handler) CaptchaChallenge(c fiber.Ctx) error {
	if h.challenger == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "this instance's captcha does not issue challenges"})
	}
	ch, err := h.challenger.Challenge(c.Context())
	if err != nil {
		logger.Err("auth", err, map[string]any{"stage": "captcha-challenge"})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	// A challenge is single-use and short-lived; nothing between here and the browser may
	// hand a cached one to a second visitor.
	c.Set("Cache-Control", "no-store")
	return c.JSON(ch)
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

	// Before anything is written: a bot that can't answer the challenge should cost this
	// instance one HTTP round trip, not a user row and a discriminator.
	if h.captcha != nil {
		switch err := h.captcha.Verify(c.Context(), in.CaptchaToken, c.IP()); err {
		case nil:
		case captcha.ErrMissingToken:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "captcha is required"})
		case captcha.ErrFailed:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "captcha verification failed"})
		default:
			logger.Err("auth", err, map[string]any{"stage": "captcha"})
			return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "captcha provider unavailable, try again"})
		}
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
		case ErrWeakPassword, ErrPasswordTooLong, ErrInvalidUsername:
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
