package auth

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// MFAChallenge is returned from Login instead of a session when the account has a second
// factor enabled. Token is opaque and short-lived (see pendingMFA in service_2fa.go);
// Methods lists which of the endpoints under /auth/2fa the client may call with it.
type MFAChallenge struct {
	Token   string   `json:"mfa_token"`
	Methods []string `json:"methods"`
}

// LoginResult is what a successful call to Login produces: either a session (Token set,
// MFA nil) or a challenge for the second factor (MFA set, Token empty, User only used
// internally by the service - never serialized before the challenge is cleared).
type LoginResult struct {
	User  *User
	Token string
	MFA   *MFAChallenge
}

// TwoFactorStatus is the response to GET /users/@me/2fa.
type TwoFactorStatus struct {
	TOTPEnabled            bool                     `json:"totp_enabled"`
	WebAuthnCredentials    []WebAuthnCredentialView `json:"webauthn_credentials"`
	RecoveryCodesRemaining int                      `json:"recovery_codes_remaining"`
}

// WebAuthnCredentialView is one row of the "your passkeys" list - deliberately not the raw
// stored Credential, which carries the public key and attestation data.
type WebAuthnCredentialView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

// webauthnUser adapts a User plus their loaded credentials to the go-webauthn User
// interface. It is built fresh for each ceremony, never persisted.
type webauthnUser struct {
	user        *User
	credentials []webauthn.Credential
}

// WebAuthnID is the opaque "user handle" exchanged with the authenticator. This app never
// does discoverable/usernameless login (a 2FA ceremony always already knows which user is
// signing in, from the pending-MFA token or the caller's own session), so unlike a
// passwordless deployment there is no need to resolve a user *from* this value - only for
// it to be stable and unique per user, which a plain hash of the account id already gives
// us without a dedicated stored-handle column.
func (w webauthnUser) WebAuthnID() []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("webauthn-user:%d", w.user.ID)))
	return sum[:]
}

func (w webauthnUser) WebAuthnName() string        { return w.user.Username }
func (w webauthnUser) WebAuthnDisplayName() string { return w.user.DisplayName }
func (w webauthnUser) WebAuthnCredentials() []webauthn.Credential { return w.credentials }
