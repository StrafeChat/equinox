package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestEmailChangeErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		code int
		wire string
	}{
		{ErrEmailChangeFieldsRequired, http.StatusBadRequest, "invalid_body"},
		{ErrStepUpBothCodes, http.StatusBadRequest, "invalid_body"},
		{ErrEmailChangeTokenRequired, http.StatusBadRequest, "invalid_body"},
		{ErrInvalidEmail, http.StatusBadRequest, "invalid_email"},
		{ErrEmailUnchanged, http.StatusBadRequest, "email_unchanged"},
		{ErrDisposableEmail, http.StatusBadRequest, "disposable_email"},
		{ErrEmailChangeTokenInvalid, http.StatusBadRequest, "email_change_token_invalid"},
		{ErrSecondFactorRequired, http.StatusForbidden, "mfa_required"},
		{ErrSessionRequired, http.StatusForbidden, "session_required"},
		{ErrEmailChangeWrongAccount, http.StatusForbidden, "email_change_wrong_account"},
		{ErrNoPendingEmailChange, http.StatusNotFound, "no_pending_email_change"},
		{ErrEmailInUse, http.StatusConflict, "email_in_use"},
		{ErrEmailChangeConflict, http.StatusConflict, "email_change_stale"},
		{ErrEmailChangeBusy, http.StatusConflict, "email_change_busy"},
		{ErrEmailTargetLimited, http.StatusTooManyRequests, "email_target_limited"},
		{ErrReauthLocked, http.StatusTooManyRequests, "reauth_locked"},
		// Fallbacks to the shared email rows and the step-up rows.
		{ErrEmailCooldown, http.StatusTooManyRequests, "email_cooldown"},
		{ErrEmailDisabled, http.StatusServiceUnavailable, "email_disabled"},
		{ErrInvalidCredentials, http.StatusUnauthorized, "invalid_password"},
		{ErrInvalidTOTPCode, http.StatusUnauthorized, "invalid_code"},
		{ErrInvalidRecoveryCode, http.StatusUnauthorized, "invalid_recovery_code"},
	}
	for _, tc := range cases {
		code, wire := emailChangeErrorStatus(tc.err)
		if code != tc.code || wire != tc.wire {
			t.Errorf("%v: got %d %q, want %d %q", tc.err, code, wire, tc.code, tc.wire)
		}
	}
	// The verification route answers the busy row too (SendVerificationEmail mid-change).
	if code, wire := emailErrorStatus(ErrEmailChangeBusy); code != http.StatusConflict || wire != "email_change_busy" {
		t.Errorf("emailErrorStatus(busy) = %d %q", code, wire)
	}
	if code, _ := emailChangeErrorStatus(ErrUsernameTaken); code != 0 {
		t.Errorf("unrelated errors must fall through to the 500 path, got %d", code)
	}
}

// The wire messages are exact: the web client maps them in SERVER_MESSAGE_TO_KEY.
func TestEmailChangeMessagesExact(t *testing.T) {
	want := map[error]string{
		ErrEmailChangeFieldsRequired: "email and password are required",
		ErrStepUpBothCodes:           "send code or recovery_code, not both",
		ErrEmailChangeTokenRequired:  "token is required",
		ErrInvalidEmail:              "enter a valid email address",
		ErrEmailUnchanged:            "that is already your email address",
		ErrSecondFactorRequired:      "a code from your authenticator app or a recovery code is required",
		ErrSessionRequired:           "only a signed-in person can change the account's email",
		ErrEmailChangeWrongAccount:   "this link is for a different account",
		ErrNoPendingEmailChange:      "there is no email change waiting to be confirmed",
		ErrEmailChangeConflict:       "your email address changed after this link was sent",
		ErrEmailChangeBusy:           "another change to this account's email is in progress",
		ErrEmailChangeTokenInvalid:   "that email change link is not valid or has expired",
		ErrEmailTargetLimited:        "too many emails have been sent to that address - try again later",
		ErrReauthLocked:              "too many failed attempts - try again in an hour",
	}
	for err, msg := range want {
		if err.Error() != msg {
			t.Errorf("message %q, want %q", err.Error(), msg)
		}
	}
}

func TestStepUpPlan(t *testing.T) {
	cases := []struct {
		name          string
		has2FA, totp  bool
		code, recover string
		want          stepUpFactor
		err           error
	}{
		{"no 2FA, no code", false, false, "", "", factorNone, nil},
		{"no 2FA, a code is ignored", false, false, "123456", "", factorNone, nil},
		{"no 2FA, a recovery code is ignored", false, false, "", "ABCDE-12345", factorNone, nil},
		{"TOTP, no code", true, true, "", "", factorNone, ErrSecondFactorRequired},
		{"TOTP, code", true, true, "123456", "", factorTOTP, nil},
		{"TOTP, recovery", true, true, "", "ABCDE-12345", factorRecovery, nil},
		{"passkey only, code", true, false, "123456", "", factorNone, ErrInvalidTOTPCode},
		{"passkey only, recovery", true, false, "", "ABCDE-12345", factorRecovery, nil},
		{"passkey only, nothing", true, false, "  ", "", factorNone, ErrSecondFactorRequired},
		{"both codes", true, true, "123456", "ABCDE-12345", factorNone, ErrStepUpBothCodes},
		{"both codes, no 2FA", false, false, "123456", "ABCDE-12345", factorNone, ErrStepUpBothCodes},
	}
	for _, tc := range cases {
		got, err := stepUpPlan(tc.has2FA, tc.totp, tc.code, tc.recover)
		if got != tc.want || err != tc.err {
			t.Errorf("%s: got %v, %v; want %v, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestEmailUpdatePayloadShape(t *testing.T) {
	p := EmailUpdatePayload("a@b.co", true)
	if len(p) != 2 || p["email"] != "a@b.co" || p["verified_email"] != true {
		t.Errorf("payload = %v, want exactly {email, verified_email}", p)
	}
}

func TestExpiresInPhrase(t *testing.T) {
	cases := map[int]string{3600: "1 hour", 3599: "1 hour", 2700: "45 minutes", 61: "2 minutes", 60: "1 minute"}
	for secs, want := range cases {
		if got := expiresInPhrase(time.Duration(secs) * time.Second); got != want {
			t.Errorf("%ds: %q, want %q", secs, got, want)
		}
	}
}
