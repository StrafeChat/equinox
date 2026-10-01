package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	netmail "net/mail"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
)

// emailErrorStatus maps an EmailService error to its HTTP status and wire code. Nothing here
// says whether an address is registered: the forgot route answers 200 either way, and the
// token errors only ever describe a token the caller already holds.
func emailErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ErrEmailDisabled):
		return http.StatusServiceUnavailable, "email_disabled"
	case errors.Is(err, ErrEmailAlreadyVerified):
		return http.StatusConflict, "email_already_verified"
	case errors.Is(err, ErrEmailCooldown):
		return http.StatusTooManyRequests, "email_cooldown"
	case errors.Is(err, ErrEmailTokenInvalid):
		return http.StatusBadRequest, "email_token_invalid"
	case errors.Is(err, ErrResetTokenInvalid):
		return http.StatusBadRequest, "reset_token_invalid"
	case errors.Is(err, ErrPasswordTooShort), errors.Is(err, ErrPasswordTooLong), errors.Is(err, ErrWeakPassword):
		return http.StatusBadRequest, "weak_password"
	default:
		return 0, ""
	}
}

func (h *Handler) writeEmailError(c fiber.Ctx, err error) error {
	if status, code := emailErrorStatus(err); status != 0 {
		return c.Status(status).JSON(fiber.Map{"error": err.Error(), "code": code})
	}
	logger.Err("auth", err, map[string]any{"path": c.Path()})
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

type emailTokenInput struct {
	Token string `json:"token"`
}

// VerifyEmail handles POST /auth/email/verify: the link in a verification email lands on
// the web client, which posts the token here. No session - the person may well be in a
// different browser than the one they registered in.
func (h *Handler) VerifyEmail(c fiber.Ctx) error {
	var in emailTokenInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || strings.TrimSpace(in.Token) == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "token is required"})
	}
	if _, err := h.svc.VerifyEmail(c.Context(), strings.TrimSpace(in.Token)); err != nil {
		return h.writeEmailError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(fiber.Map{"ok": true})
}

type forgotPasswordInput struct {
	Email string `json:"email"`
}

// ForgotPassword handles POST /auth/password/forgot. A well-formed address is always
// answered 200: the mail goes out only if an account has that address, and the response
// must not reveal which that is.
func (h *Handler) ForgotPassword(c fiber.Ctx) error {
	var in forgotPasswordInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "email is required"})
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if addr, err := netmail.ParseAddress(email); err != nil || addr.Address != email {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "email must be valid"})
	}
	if err := h.svc.RequestPasswordReset(c.Context(), email); err != nil {
		return h.writeEmailError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(fiber.Map{"ok": true})
}

type resetPasswordInput struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// ResetPassword handles POST /auth/password/reset: the reset link's token plus the new
// password. On success every session of the account has been ended; the client sends the
// person to sign in afresh.
func (h *Handler) ResetPassword(c fiber.Ctx) error {
	var in resetPasswordInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || strings.TrimSpace(in.Token) == "" || in.Password == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "token and password are required"})
	}
	if _, err := h.svc.ResetPassword(c.Context(), strings.TrimSpace(in.Token), in.Password); err != nil {
		return h.writeEmailError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(fiber.Map{"ok": true})
}

// SendVerificationEmail handles POST /users/@me/email/verification: a signed-in account
// asking for its link again (or for the first time, on an instance that does not require
// verification but offers it).
func (h *Handler) SendVerificationEmail(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if err := h.svc.SendVerificationEmail(c.Context(), user); err != nil {
		return h.writeEmailError(c, err)
	}
	return c.JSON(fiber.Map{"ok": true})
}
