package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
)

// twoFactorErrorStatus maps a TwoFactorService error to its HTTP status and wire code. Every
// case here is safe to reveal (never "which factor failed" beyond what the account owner
// already knows they have enabled).
func twoFactorErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ErrMFATokenInvalid):
		return http.StatusUnauthorized, "mfa_token_invalid"
	case errors.Is(err, ErrMFATooManyAttempts):
		return http.StatusTooManyRequests, "mfa_too_many_attempts"
	case errors.Is(err, ErrInvalidTOTPCode):
		return http.StatusUnauthorized, "invalid_code"
	case errors.Is(err, ErrInvalidRecoveryCode):
		return http.StatusUnauthorized, "invalid_recovery_code"
	case errors.Is(err, ErrInvalidWebAuthnResponse):
		return http.StatusUnauthorized, "invalid_passkey_response"
	case errors.Is(err, ErrTOTPAlreadyEnabled):
		return http.StatusConflict, "totp_already_enabled"
	case errors.Is(err, ErrTOTPNotEnabled):
		return http.StatusConflict, "totp_not_enabled"
	case errors.Is(err, ErrTOTPNotSetUp):
		return http.StatusConflict, "totp_not_set_up"
	case errors.Is(err, ErrWebAuthnNotConfigured):
		return http.StatusServiceUnavailable, "webauthn_not_configured"
	case errors.Is(err, ErrWebAuthnCredentialNotFound):
		return http.StatusNotFound, "passkey_not_found"
	case errors.Is(err, ErrInvalidCredentials):
		return http.StatusUnauthorized, "invalid_password"
	default:
		return 0, ""
	}
}

func (h *Handler) writeTwoFactorError(c fiber.Ctx, err error) error {
	if status, code := twoFactorErrorStatus(err); status != 0 {
		return c.Status(status).JSON(fiber.Map{"error": err.Error(), "code": code})
	}
	logger.Err("auth", err, map[string]any{"path": c.Path()})
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

// --- Login-time: resolving the mfa_token a challenged Login returned ---

type totpVerifyInput struct {
	MFAToken string `json:"mfa_token"`
	Code     string `json:"code"`
}

// VerifyTOTP handles POST /auth/2fa/totp.
func (h *Handler) VerifyTOTP(c fiber.Ctx) error {
	var in totpVerifyInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || in.MFAToken == "" || strings.TrimSpace(in.Code) == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "mfa_token and code are required"})
	}
	user, token, err := h.svc.VerifyTOTPLogin(c.Context(), in.MFAToken, in.Code, c.IP(), c.Get("User-Agent"))
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusOK).JSON(fiber.Map{"token": token, "user": loginUserJSON(user)})
}

type recoveryVerifyInput struct {
	MFAToken string `json:"mfa_token"`
	Code     string `json:"code"`
}

// VerifyRecoveryCode handles POST /auth/2fa/recovery.
func (h *Handler) VerifyRecoveryCode(c fiber.Ctx) error {
	var in recoveryVerifyInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || in.MFAToken == "" || strings.TrimSpace(in.Code) == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "mfa_token and code are required"})
	}
	user, token, err := h.svc.VerifyRecoveryCodeLogin(c.Context(), in.MFAToken, in.Code, c.IP(), c.Get("User-Agent"))
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusOK).JSON(fiber.Map{"token": token, "user": loginUserJSON(user)})
}

// BeginWebAuthnLogin handles POST /auth/2fa/webauthn/begin. The pending token travels as a
// header (not a query string, not the body) so it never lands in a proxy log and the body
// stays free for the credential payload on the matching /finish call.
func (h *Handler) BeginWebAuthnLogin(c fiber.Ctx) error {
	mfaToken := c.Get("X-MFA-Token")
	if mfaToken == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "X-MFA-Token header is required"})
	}
	assertion, err := h.svc.BeginWebAuthnLogin(c.Context(), mfaToken)
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusOK).JSON(assertion)
}

// FinishWebAuthnLogin handles POST /auth/2fa/webauthn/finish. The body is exactly the
// PublicKeyCredential the browser produced, unmodified, so it parses as-is.
func (h *Handler) FinishWebAuthnLogin(c fiber.Ctx) error {
	mfaToken := c.Get("X-MFA-Token")
	if mfaToken == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "X-MFA-Token header is required"})
	}
	user, token, err := h.svc.FinishWebAuthnLogin(c.Context(), mfaToken, c.Body(), c.IP(), c.Get("User-Agent"))
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusOK).JSON(fiber.Map{"token": token, "user": loginUserJSON(user)})
}

// --- Settings: managing a signed-in account's own 2FA ---

// TwoFactorStatus handles GET /users/@me/2fa.
func (h *Handler) TwoFactorStatus(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	status, err := h.svc.TwoFactorStatus(c.Context(), user.ID)
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	return c.Status(http.StatusOK).JSON(status)
}

// SetupTOTP handles POST /users/@me/2fa/totp/setup.
func (h *Handler) SetupTOTP(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	secret, otpauthURL, err := h.svc.SetupTOTP(c.Context(), user.ID)
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusOK).JSON(fiber.Map{"secret": secret, "otpauth_url": otpauthURL})
}

type totpEnableInput struct {
	Code string `json:"code"`
}

// EnableTOTP handles POST /users/@me/2fa/totp/enable.
func (h *Handler) EnableTOTP(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in totpEnableInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || strings.TrimSpace(in.Code) == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "code is required"})
	}
	codes, err := h.svc.EnableTOTP(c.Context(), user.ID, in.Code)
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusOK).JSON(fiber.Map{"recovery_codes": codes})
}

type passwordInput struct {
	Password string `json:"password"`
}

// DisableTOTP handles POST /users/@me/2fa/totp/disable.
func (h *Handler) DisableTOTP(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in passwordInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || in.Password == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "password is required"})
	}
	if err := h.svc.DisableTOTP(c.Context(), user.ID, in.Password); err != nil {
		return h.writeTwoFactorError(c, err)
	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"ok": true})
}

// BeginWebAuthnRegistration handles POST /users/@me/2fa/webauthn/register/begin.
func (h *Handler) BeginWebAuthnRegistration(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	creation, err := h.svc.BeginWebAuthnRegistration(c.Context(), user.ID)
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusOK).JSON(creation)
}

// FinishWebAuthnRegistration handles POST /users/@me/2fa/webauthn/register/finish?name=....
// The body is exactly the PublicKeyCredential the browser produced; the user-chosen label
// for the key travels as a query parameter instead of a body field so the body needs no
// unwrapping before it reaches the WebAuthn parser.
func (h *Handler) FinishWebAuthnRegistration(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	name := c.Query("name")
	codes, err := h.svc.FinishWebAuthnRegistration(c.Context(), user.ID, name, c.Body())
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusCreated).JSON(fiber.Map{"recovery_codes": codes})
}

// DeleteWebAuthnCredential handles DELETE /users/@me/2fa/webauthn/:credential_id.
func (h *Handler) DeleteWebAuthnCredential(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	// credential_id is base64url (RFC 4648 §5: A-Za-z0-9-_ only), which needs no
	// percent-decoding - unlike a route param that can carry arbitrary text.
	credentialID := c.Params("credential_id")
	if credentialID == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid credential id"})
	}
	var in passwordInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || in.Password == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "password is required"})
	}
	if err := h.svc.DeleteWebAuthnCredential(c.Context(), user.ID, credentialID, in.Password); err != nil {
		return h.writeTwoFactorError(c, err)
	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"ok": true})
}

// RegenerateRecoveryCodes handles POST /users/@me/2fa/recovery_codes/regenerate.
func (h *Handler) RegenerateRecoveryCodes(c fiber.Ctx) error {
	user := GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in passwordInput
	if err := json.Unmarshal(c.Body(), &in); err != nil || in.Password == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "password is required"})
	}
	codes, err := h.svc.RegenerateRecoveryCodes(c.Context(), user.ID, in.Password)
	if err != nil {
		return h.writeTwoFactorError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.Status(http.StatusOK).JSON(fiber.Map{"recovery_codes": codes})
}
