package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
)

// emailChangeErrorStatus maps the email-change errors to status and wire code, then falls
// back to the shared email rows (email_cooldown, email_disabled, email_change_busy) and the
// step-up rows (invalid_password, invalid_code, invalid_recovery_code). 0 means 500.
func emailChangeErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ErrEmailChangeFieldsRequired), errors.Is(err, ErrStepUpBothCodes), errors.Is(err, ErrEmailChangeTokenRequired):
		return http.StatusBadRequest, "invalid_body"
	case errors.Is(err, ErrInvalidEmail):
		return http.StatusBadRequest, "invalid_email"
	case errors.Is(err, ErrEmailUnchanged):
		return http.StatusBadRequest, "email_unchanged"
	case errors.Is(err, ErrDisposableEmail):
		return http.StatusBadRequest, "disposable_email"
	case errors.Is(err, ErrEmailChangeTokenInvalid):
		return http.StatusBadRequest, "email_change_token_invalid"
	case errors.Is(err, ErrSecondFactorRequired):
		return http.StatusForbidden, "mfa_required"
	case errors.Is(err, ErrSessionRequired):
		return http.StatusForbidden, "session_required"
	case errors.Is(err, ErrEmailChangeWrongAccount):
		return http.StatusForbidden, "email_change_wrong_account"
	case errors.Is(err, ErrNoPendingEmailChange):
		return http.StatusNotFound, "no_pending_email_change"
	case errors.Is(err, ErrEmailInUse):
		return http.StatusConflict, "email_in_use"
	case errors.Is(err, ErrEmailChangeConflict):
		return http.StatusConflict, "email_change_stale"
	case errors.Is(err, ErrEmailTargetLimited):
		return http.StatusTooManyRequests, "email_target_limited"
	case errors.Is(err, ErrReauthLocked):
		return http.StatusTooManyRequests, "reauth_locked"
	}
	if status, code := emailErrorStatus(err); status != 0 {
		return status, code
	}
	return twoFactorErrorStatus(err)
}

func (h *Handler) writeEmailChangeError(c fiber.Ctx, userID int64, err error) error {
	c.Set("Cache-Control", "no-store")
	if status, code := emailChangeErrorStatus(err); status != 0 {
		return c.Status(status).JSON(fiber.Map{"error": err.Error(), "code": code})
	}
	logger.Err("auth", err, map[string]any{"path": c.Path(), "user_id": userID})
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

// emailStateJSON is the EmailState wire shape; pending is null when nothing waits.
func emailStateJSON(st *EmailState) fiber.Map {
	var pending any
	if st.Pending != nil {
		pending = fiber.Map{
			"email":      st.Pending.Email,
			"expires_at": st.Pending.ExpiresAt.UTC().Format(time.RFC3339),
		}
	}
	return fiber.Map{"email": st.Email, "verified_email": st.Verified, "pending": pending}
}

// emailChangeCaller is the session user, or nil after writing the 403: only a person's own
// session may read or change the address. requireAuth already refuses OAuth2 tokens; a bot
// token has no session, and system accounts and federation shadows never sign in, but are
// refused here too rather than relied on.
func (h *Handler) emailChangeCaller(c fiber.Ctx) (*User, error) {
	user := GetUser(c)
	if user == nil {
		c.Set("Cache-Control", "no-store")
		return nil, c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if GetSession(c) == nil || user.Bot || user.System || user.IsRemote() {
		c.Set("Cache-Control", "no-store")
		return nil, c.Status(http.StatusForbidden).JSON(fiber.Map{"error": ErrSessionRequired.Error(), "code": "session_required"})
	}
	return user, nil
}

// GetEmail handles GET /users/@me/email.
func (h *Handler) GetEmail(c fiber.Ctx) error {
	user, done := h.emailChangeCaller(c)
	if user == nil {
		return done
	}
	st, err := h.svc.GetEmailState(c.Context(), user)
	if err != nil {
		return h.writeEmailChangeError(c, user.ID, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(emailStateJSON(st))
}

type emailChangeBody struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

// RequestEmailChange handles POST /users/@me/email: 202 with the pending change when the
// instance sends mail (the same answer whether or not the address is free), 200 with the
// change already applied when it does not.
func (h *Handler) RequestEmailChange(c fiber.Ctx) error {
	user, done := h.emailChangeCaller(c)
	if user == nil {
		return done
	}
	var in emailChangeBody
	if err := json.Unmarshal(c.Body(), &in); err != nil || strings.TrimSpace(in.Email) == "" || in.Password == "" {
		return h.writeEmailChangeError(c, user.ID, ErrEmailChangeFieldsRequired)
	}
	if strings.TrimSpace(in.Code) != "" && strings.TrimSpace(in.RecoveryCode) != "" {
		return h.writeEmailChangeError(c, user.ID, ErrStepUpBothCodes)
	}
	st, applied, err := h.svc.RequestEmailChange(c.Context(), user, EmailChangeInput{
		Email: in.Email, Password: in.Password, Code: in.Code, RecoveryCode: in.RecoveryCode,
	})
	if err != nil {
		return h.writeEmailChangeError(c, user.ID, err)
	}
	c.Set("Cache-Control", "no-store")
	status := http.StatusAccepted
	if applied {
		status = http.StatusOK
	}
	return c.Status(status).JSON(emailStateJSON(st))
}

// ResendEmailChange handles POST /users/@me/email/pending/resend: the pending change's
// mail again, without another step-up (so without spending another code).
func (h *Handler) ResendEmailChange(c fiber.Ctx) error {
	user, done := h.emailChangeCaller(c)
	if user == nil {
		return done
	}
	st, err := h.svc.ResendEmailChange(c.Context(), user)
	if err != nil {
		return h.writeEmailChangeError(c, user.ID, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusAccepted).JSON(emailStateJSON(st))
}

// CancelEmailChange handles DELETE /users/@me/email/pending. Idempotent.
func (h *Handler) CancelEmailChange(c fiber.Ctx) error {
	user, done := h.emailChangeCaller(c)
	if user == nil {
		return done
	}
	st, err := h.svc.CancelEmailChange(c.Context(), user)
	if err != nil {
		return h.writeEmailChangeError(c, user.ID, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(emailStateJSON(st))
}

// ConfirmEmailChange handles POST /users/@me/email/confirm {token}. A session is required -
// a link that worked anonymously would let a stranger (or their mail scanner) confirm a
// typo'd address and then reset the password through it.
func (h *Handler) ConfirmEmailChange(c fiber.Ctx) error {
	user, done := h.emailChangeCaller(c)
	if user == nil {
		return done
	}
	var in emailTokenInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || strings.TrimSpace(in.Token) == "" {
		return h.writeEmailChangeError(c, user.ID, ErrEmailChangeTokenRequired)
	}
	st, err := h.svc.ConfirmEmailChange(c.Context(), user, in.Token)
	if err != nil {
		return h.writeEmailChangeError(c, user.ID, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(emailStateJSON(st))
}
