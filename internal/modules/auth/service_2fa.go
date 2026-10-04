package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

var (
	// Login-time (pending mfa_token) errors.
	ErrMFATokenInvalid         = errors.New("mfa session expired or invalid, please log in again")
	ErrMFATooManyAttempts      = errors.New("too many attempts, please log in again")
	ErrInvalidTOTPCode         = errors.New("invalid authenticator code")
	ErrInvalidRecoveryCode     = errors.New("invalid or already-used recovery code")
	ErrInvalidWebAuthnResponse = errors.New("passkey verification failed")

	// Settings-time errors.
	ErrTOTPAlreadyEnabled         = errors.New("authenticator app is already enabled")
	ErrTOTPNotEnabled             = errors.New("authenticator app is not enabled")
	ErrTOTPNotSetUp               = errors.New("call totp/setup before totp/enable")
	ErrWebAuthnNotConfigured      = errors.New("passkeys are not configured on this instance")
	ErrWebAuthnCredentialNotFound = errors.New("passkey not found")
)

const (
	mfaPendingTTL  = 5 * time.Minute
	mfaCeremonyTTL = 2 * time.Minute
	mfaMaxAttempts = 5

	// totpStepSeconds mirrors totpValidateOpts().Period.
	totpStepSeconds = 30
	// How long a "this time step was already used" marker lives: comfortably past the ±1
	// step of skew a code is accepted within, so the same code cannot be replayed for as
	// long as it would otherwise still validate.
	totpUsedTTL = 3 * totpStepSeconds * time.Second

	// A passkey label is rendered in the passkeys list; bound it rather than storing
	// whatever length a crafted query string carries.
	maxPasskeyNameRunes = 64
)

// TwoFactorService is the account-security half of Service: verifying a second factor
// during login (given the pending mfa_token Login returned) and managing a signed-in
// user's own TOTP/passkey/recovery-code setup. Split into its own interface (and this own
// file) per the module's convention of splitting by feature once a file grows, while still
// being one Service to callers - see the embed in service.go.
type TwoFactorService interface {
	VerifyTOTPLogin(ctx context.Context, mfaToken, code, ip, userAgent string) (*User, string, error)
	VerifyRecoveryCodeLogin(ctx context.Context, mfaToken, code, ip, userAgent string) (*User, string, error)
	BeginWebAuthnLogin(ctx context.Context, mfaToken string) (*protocol.CredentialAssertion, error)
	FinishWebAuthnLogin(ctx context.Context, mfaToken string, body []byte, ip, userAgent string) (*User, string, error)

	TwoFactorStatus(ctx context.Context, userID int64) (*TwoFactorStatus, error)
	SetupTOTP(ctx context.Context, userID int64) (secret string, otpauthURL string, err error)
	EnableTOTP(ctx context.Context, userID int64, code string) (recoveryCodes []string, err error)
	DisableTOTP(ctx context.Context, userID int64, password string) error
	BeginWebAuthnRegistration(ctx context.Context, userID int64) (*protocol.CredentialCreation, error)
	FinishWebAuthnRegistration(ctx context.Context, userID int64, name string, body []byte) (recoveryCodes []string, err error)
	DeleteWebAuthnCredential(ctx context.Context, userID int64, credentialID, password string) error
	RegenerateRecoveryCodes(ctx context.Context, userID int64, password string) ([]string, error)
}

type pendingMFA struct {
	UserID int64 `json:"user_id"`
}

func (s *service) mfaPendingKey(token string) string {
	return s.redisPrefix + "mfa:pending:" + mfaTokenHash(token)
}
func (s *service) mfaAttemptsKey(token string) string {
	return s.redisPrefix + "mfa:attempts:" + mfaTokenHash(token)
}
func (s *service) mfaCeremonyKey(token string) string {
	return s.redisPrefix + "mfa:ceremony:" + mfaTokenHash(token)
}
func (s *service) webauthnRegKey(userID int64) string {
	return s.redisPrefix + "webauthn:reg:" + fmt.Sprint(userID)
}

// mfaTokenHash mirrors how a session bearer token is looked up by its hash rather than its
// raw value (middleware/auth.go): the pending token is bearer-equivalent for its short
// window, so a Redis dump alone should not be enough to redeem one.
func mfaTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// beginPendingMFA issues the short-lived token Login hands back instead of a session when
// the account has a second factor. hasWebAuthn is passed in because listing credentials is
// the caller's problem (Login already loaded them to decide whether to get here at all).
func (s *service) beginPendingMFA(ctx context.Context, u *User, hasWebAuthn bool) (*MFAChallenge, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tok := hex.EncodeToString(raw)
	payload, err := json.Marshal(pendingMFA{UserID: u.ID})
	if err != nil {
		return nil, err
	}
	if err := s.redis.Set(ctx, s.mfaPendingKey(tok), payload, mfaPendingTTL).Err(); err != nil {
		return nil, err
	}
	methods := make([]string, 0, 2)
	if u.TOTPEnabled {
		methods = append(methods, "totp")
	}
	if hasWebAuthn {
		methods = append(methods, "webauthn")
	}
	return &MFAChallenge{Token: tok, Methods: methods}, nil
}

func (s *service) loadPendingMFA(ctx context.Context, mfaToken string) (int64, error) {
	raw, err := s.redis.Get(ctx, s.mfaPendingKey(mfaToken)).Bytes()
	if err != nil {
		return 0, ErrMFATokenInvalid
	}
	var p pendingMFA
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, ErrMFATokenInvalid
	}
	return p.UserID, nil
}

func (s *service) clearPendingMFA(ctx context.Context, mfaToken string) {
	_ = s.redis.Del(ctx, s.mfaPendingKey(mfaToken), s.mfaAttemptsKey(mfaToken), s.mfaCeremonyKey(mfaToken)).Err()
}

// checkAndIncrementAttempts bounds how many codes can be tried against one pending login,
// independent of (and in addition to) the per-IP limiter on the route: the limiter stops a
// single attacker hammering the endpoint, this stops the same pending token being
// brute-forced from many IPs at once. Exceeding it burns the token - the attacker (or a
// confused legitimate user) has to prove the password again to get a new one.
func (s *service) checkAndIncrementAttempts(ctx context.Context, mfaToken string) error {
	key := s.mfaAttemptsKey(mfaToken)
	n, err := s.redis.Incr(ctx, key).Result()
	if err != nil {
		return err
	}
	if n == 1 {
		_ = s.redis.Expire(ctx, key, mfaPendingTTL).Err()
	}
	if n > mfaMaxAttempts {
		s.clearPendingMFA(ctx, mfaToken)
		return ErrMFATooManyAttempts
	}
	return nil
}

func totpValidateOpts() totp.ValidateOpts {
	// Skew 1 tolerates the code either side of "now" by one 30s step, the same drift
	// Google/Microsoft/Discord authenticators are built to expect from the server side.
	return totp.ValidateOpts{Period: totpStepSeconds, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
}

func (s *service) totpUsedKey(userID int64, step int64) string {
	return fmt.Sprintf("%smfa:totp:used:%d:%d", s.redisPrefix, userID, step)
}

// verifyTOTPCode validates code against secret with the usual ±1-step skew and, on success,
// burns the exact time step it matched so the same code is refused if it is presented again
// (RFC 6238 §5.2). The per-token attempt budget cannot cover this case: a replayed code is
// *correct*, so without the marker a code read over someone's shoulder, or phished and
// relayed, stayed usable for up to 90 seconds. Each candidate step is checked on its own
// (skew 0 at now-1, now, now+1) so the marker names the step that actually matched; the
// marker is a SET NX, so two simultaneous submissions of one code cannot both pass.
func (s *service) verifyTOTPCode(ctx context.Context, userID int64, code, secret string) error {
	code = strings.TrimSpace(code)
	now := time.Now()
	opts := totpValidateOpts()
	opts.Skew = 0
	for _, k := range []int64{0, -1, 1} {
		at := now.Add(time.Duration(k) * totpStepSeconds * time.Second)
		ok, err := totp.ValidateCustom(code, secret, at, opts)
		if err != nil || !ok {
			continue
		}
		step := at.Unix() / totpStepSeconds
		fresh, err := s.redis.SetNX(ctx, s.totpUsedKey(userID, step), 1, totpUsedTTL).Result()
		if err != nil {
			return err
		}
		if !fresh {
			return ErrInvalidTOTPCode // this very code was already accepted; a replay
		}
		return nil
	}
	return ErrInvalidTOTPCode
}

func (s *service) VerifyTOTPLogin(ctx context.Context, mfaToken, code, ip, userAgent string) (*User, string, error) {
	userID, err := s.loadPendingMFA(ctx, mfaToken)
	if err != nil {
		return nil, "", err
	}
	if err := s.checkAndIncrementAttempts(ctx, mfaToken); err != nil {
		return nil, "", err
	}
	enabled, secretCipher, err := s.tfrepo.GetTOTPState(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if !enabled || secretCipher == "" {
		return nil, "", ErrInvalidTOTPCode
	}
	secret, err := decryptTOTPSecret(s.cfg.TwoFactor.TOTPEncryptionKey, secretCipher)
	if err != nil {
		return nil, "", err
	}
	if err := s.verifyTOTPCode(ctx, userID, code, secret); err != nil {
		return nil, "", err
	}
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if u == nil {
		return nil, "", ErrInvalidTOTPCode
	}
	s.clearPendingMFA(ctx, mfaToken)
	token, err := s.createSession(ctx, u.ID, ip, userAgent)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

func (s *service) VerifyRecoveryCodeLogin(ctx context.Context, mfaToken, code, ip, userAgent string) (*User, string, error) {
	userID, err := s.loadPendingMFA(ctx, mfaToken)
	if err != nil {
		return nil, "", err
	}
	if err := s.checkAndIncrementAttempts(ctx, mfaToken); err != nil {
		return nil, "", err
	}
	applied, err := s.tfrepo.ConsumeRecoveryCode(ctx, userID, hashRecoveryCode(code))
	if err != nil {
		return nil, "", err
	}
	if !applied {
		return nil, "", ErrInvalidRecoveryCode
	}
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if u == nil {
		return nil, "", ErrInvalidRecoveryCode
	}
	s.clearPendingMFA(ctx, mfaToken)
	token, err := s.createSession(ctx, u.ID, ip, userAgent)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

func (s *service) BeginWebAuthnLogin(ctx context.Context, mfaToken string) (*protocol.CredentialAssertion, error) {
	if s.webauthn == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	userID, err := s.loadPendingMFA(ctx, mfaToken)
	if err != nil {
		return nil, err
	}
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrMFATokenInvalid
	}
	creds, err := s.loadCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(creds) == 0 {
		return nil, ErrWebAuthnCredentialNotFound
	}
	assertion, sessionData, err := s.webauthn.BeginLogin(webauthnUser{user: u, credentials: creds})
	if err != nil {
		return nil, err
	}
	sessionBytes, err := json.Marshal(sessionData)
	if err != nil {
		return nil, err
	}
	if err := s.redis.Set(ctx, s.mfaCeremonyKey(mfaToken), sessionBytes, mfaCeremonyTTL).Err(); err != nil {
		return nil, err
	}
	return assertion, nil
}

func (s *service) FinishWebAuthnLogin(ctx context.Context, mfaToken string, body []byte, ip, userAgent string) (*User, string, error) {
	if s.webauthn == nil {
		return nil, "", ErrWebAuthnNotConfigured
	}
	userID, err := s.loadPendingMFA(ctx, mfaToken)
	if err != nil {
		return nil, "", err
	}
	if err := s.checkAndIncrementAttempts(ctx, mfaToken); err != nil {
		return nil, "", err
	}
	sessionRaw, err := s.redis.Get(ctx, s.mfaCeremonyKey(mfaToken)).Bytes()
	if err != nil {
		return nil, "", ErrMFATokenInvalid
	}
	var sessionData webauthn.SessionData
	if err := json.Unmarshal(sessionRaw, &sessionData); err != nil {
		return nil, "", ErrMFATokenInvalid
	}
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if u == nil {
		return nil, "", ErrMFATokenInvalid
	}
	creds, err := s.loadCredentials(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(body))
	if err != nil {
		return nil, "", ErrInvalidWebAuthnResponse
	}
	updated, err := s.webauthn.ValidateLogin(webauthnUser{user: u, credentials: creds}, sessionData, parsed)
	if err != nil {
		return nil, "", ErrInvalidWebAuthnResponse
	}
	s.persistCredential(ctx, userID, updated)

	s.clearPendingMFA(ctx, mfaToken)
	token, err := s.createSession(ctx, u.ID, ip, userAgent)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

func (s *service) TwoFactorStatus(ctx context.Context, userID int64) (*TwoFactorStatus, error) {
	enabled, _, err := s.tfrepo.GetTOTPState(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.tfrepo.ListWebAuthnCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	views := make([]WebAuthnCredentialView, 0, len(rows))
	for _, row := range rows {
		views = append(views, WebAuthnCredentialView{ID: row.CredentialID, Name: row.Name, CreatedAt: row.CreatedAt, LastUsedAt: row.LastUsedAt})
	}
	remaining, err := s.tfrepo.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &TwoFactorStatus{TOTPEnabled: enabled, WebAuthnCredentials: views, RecoveryCodesRemaining: remaining}, nil
}

func (s *service) SetupTOTP(ctx context.Context, userID int64) (string, string, error) {
	enabled, _, err := s.tfrepo.GetTOTPState(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if enabled {
		return "", "", ErrTOTPAlreadyEnabled
	}
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if u == nil {
		return "", "", ErrInvalidCredentials
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "StrafeChat",
		AccountName: u.Username,
	})
	if err != nil {
		return "", "", err
	}
	enc, err := encryptTOTPSecret(s.cfg.TwoFactor.TOTPEncryptionKey, key.Secret())
	if err != nil {
		return "", "", err
	}
	// Not enabled yet - EnableTOTP flips the flag once the user proves they can generate a
	// matching code. Calling setup again before that simply replaces this pending secret.
	if err := s.repo.SetTOTP(ctx, userID, enc, false); err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

func (s *service) EnableTOTP(ctx context.Context, userID int64, code string) ([]string, error) {
	enabled, secretCipher, err := s.tfrepo.GetTOTPState(ctx, userID)
	if err != nil {
		return nil, err
	}
	if enabled {
		return nil, ErrTOTPAlreadyEnabled
	}
	if secretCipher == "" {
		return nil, ErrTOTPNotSetUp
	}
	secret, err := decryptTOTPSecret(s.cfg.TwoFactor.TOTPEncryptionKey, secretCipher)
	if err != nil {
		return nil, err
	}
	if err := s.verifyTOTPCode(ctx, userID, code, secret); err != nil {
		return nil, err
	}
	if err := s.repo.SetTOTP(ctx, userID, secretCipher, true); err != nil {
		return nil, err
	}
	return s.ensureRecoveryCodes(ctx, userID)
}

func (s *service) DisableTOTP(ctx context.Context, userID int64, password string) error {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrInvalidCredentials
	}
	if err := s.verifyPassword(ctx, u, password); err != nil {
		return err
	}
	enabled, _, err := s.tfrepo.GetTOTPState(ctx, userID)
	if err != nil {
		return err
	}
	if !enabled {
		return ErrTOTPNotEnabled
	}
	return s.repo.SetTOTP(ctx, userID, "", false)
}

func (s *service) BeginWebAuthnRegistration(ctx context.Context, userID int64) (*protocol.CredentialCreation, error) {
	if s.webauthn == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalidCredentials
	}
	creds, err := s.loadCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(creds))
	for _, c := range creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, sessionData, err := s.webauthn.BeginRegistration(webauthnUser{user: u, credentials: creds}, webauthn.WithExclusions(exclude))
	if err != nil {
		return nil, err
	}
	sessionBytes, err := json.Marshal(sessionData)
	if err != nil {
		return nil, err
	}
	if err := s.redis.Set(ctx, s.webauthnRegKey(userID), sessionBytes, mfaCeremonyTTL).Err(); err != nil {
		return nil, err
	}
	return creation, nil
}

func (s *service) FinishWebAuthnRegistration(ctx context.Context, userID int64, name string, body []byte) ([]string, error) {
	if s.webauthn == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalidCredentials
	}
	sessionRaw, err := s.redis.Get(ctx, s.webauthnRegKey(userID)).Bytes()
	if err != nil {
		return nil, ErrMFATokenInvalid
	}
	var sessionData webauthn.SessionData
	if err := json.Unmarshal(sessionRaw, &sessionData); err != nil {
		return nil, ErrMFATokenInvalid
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(body))
	if err != nil {
		return nil, ErrInvalidWebAuthnResponse
	}
	cred, err := s.webauthn.CreateCredential(webauthnUser{user: u}, sessionData, parsed)
	if err != nil {
		return nil, ErrInvalidWebAuthnResponse
	}
	_ = s.redis.Del(ctx, s.webauthnRegKey(userID)).Err()

	credJSON, err := json.Marshal(cred)
	if err != nil {
		return nil, err
	}
	displayName := strings.TrimSpace(name)
	if displayName == "" {
		displayName = "Passkey"
	}
	if r := []rune(displayName); len(r) > maxPasskeyNameRunes {
		displayName = string(r[:maxPasskeyNameRunes])
	}
	row := &WebAuthnCredentialRow{
		UserID:       userID,
		CredentialID: base64.RawURLEncoding.EncodeToString(cred.ID),
		Credential:   credJSON,
		Name:         displayName,
	}
	if err := s.tfrepo.CreateWebAuthnCredential(ctx, row); err != nil {
		return nil, err
	}
	return s.ensureRecoveryCodes(ctx, userID)
}

func (s *service) DeleteWebAuthnCredential(ctx context.Context, userID int64, credentialID, password string) error {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrInvalidCredentials
	}
	if err := s.verifyPassword(ctx, u, password); err != nil {
		return err
	}
	row, err := s.tfrepo.GetWebAuthnCredential(ctx, userID, credentialID)
	if err != nil {
		return err
	}
	if row == nil {
		return ErrWebAuthnCredentialNotFound
	}
	return s.tfrepo.DeleteWebAuthnCredential(ctx, userID, credentialID)
}

// AdminRegenerateRecoveryCodes issues a fresh batch of recovery codes for a user, replaces the
// stored set with their hashes, and returns the plaintext codes. Unlike the self-service
// RegenerateRecoveryCodes it takes NO password - it is for an instance admin acting on behalf
// of a user locked out of their account; the caller must have already checked admin authority.
// The codes only help someone who also has the account password (they bypass the 2FA step, not
// the password), so delivering them to the user by email is safe.
func AdminRegenerateRecoveryCodes(ctx context.Context, tfrepo TwoFactorRepository, userID int64) ([]string, error) {
	codes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = hashRecoveryCode(c)
	}
	if err := tfrepo.CreateRecoveryCodes(ctx, userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

func (s *service) RegenerateRecoveryCodes(ctx context.Context, userID int64, password string) ([]string, error) {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalidCredentials
	}
	if err := s.verifyPassword(ctx, u, password); err != nil {
		return nil, err
	}
	codes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = hashRecoveryCode(c)
	}
	if err := s.tfrepo.CreateRecoveryCodes(ctx, userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// ensureRecoveryCodes issues a fresh batch only the first time a user enables any second
// factor (TOTP or a passkey) - a later one reuses whatever set is already live, exactly like
// GitHub/Discord: recovery codes are the fallback for the *whole* account, not per-method.
func (s *service) ensureRecoveryCodes(ctx context.Context, userID int64) ([]string, error) {
	remaining, err := s.tfrepo.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return nil, err
	}
	if remaining > 0 {
		return nil, nil
	}
	codes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = hashRecoveryCode(c)
	}
	if err := s.tfrepo.CreateRecoveryCodes(ctx, userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// verifyPassword re-checks the account password for a security-sensitive settings change.
// It deliberately re-fetches by email rather than trusting a *User already in hand: GetByID
// may be cache-served, and the cache never carries PasswordHash (see cached_user_repo.go),
// so a cached user's hash reads as empty and would make every re-auth fail. GetByEmail is
// never cached, so it always sees the real column.
func (s *service) verifyPassword(ctx context.Context, u *User, password string) error {
	real, err := s.repo.GetByEmail(ctx, u.Email)
	if err != nil {
		return err
	}
	if real == nil || bcrypt.CompareHashAndPassword([]byte(real.PasswordHash), []byte(password)) != nil {
		return ErrInvalidCredentials
	}
	return nil
}

func (s *service) loadCredentials(ctx context.Context, userID int64) ([]webauthn.Credential, error) {
	rows, err := s.tfrepo.ListWebAuthnCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]webauthn.Credential, 0, len(rows))
	for _, row := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(row.Credential, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// persistCredential writes back the fields a login ceremony updates (sign count, clone
// warning, the UV/BS flags) - required after every successful login per the library's own
// storage guidance, not merely nice to have: skipping it means the next ceremony can't tell
// a cloned authenticator from a legitimate one.
func (s *service) persistCredential(ctx context.Context, userID int64, cred *webauthn.Credential) {
	credJSON, err := json.Marshal(cred)
	if err != nil {
		return
	}
	credIDStr := base64.RawURLEncoding.EncodeToString(cred.ID)
	_ = s.tfrepo.UpdateWebAuthnCredential(ctx, userID, credIDStr, credJSON, time.Now().UTC())
}
