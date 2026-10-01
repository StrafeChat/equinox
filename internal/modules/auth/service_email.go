package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/mail"
	"github.com/StrafeChat/equinox/internal/safego"
)

var (
	ErrEmailDisabled        = errors.New("email is not configured on this instance")
	ErrEmailAlreadyVerified = errors.New("email is already verified")
	ErrEmailCooldown        = errors.New("please wait a minute before requesting another email")
	ErrEmailTokenInvalid    = errors.New("that verification link is not valid or has expired")
	ErrResetTokenInvalid    = errors.New("that password reset link is not valid or has expired")
	ErrPasswordTooShort     = errors.New("password must be at least 8 characters")
)

// EmailUnverifiedError is Login's answer for an account that must confirm its address
// before it may sign in. Sent says whether a fresh link went out on this attempt (false
// when the per-account cooldown held it back, meaning one is already in the inbox).
// Compared with errors.As, never ==.
type EmailUnverifiedError struct{ Sent bool }

func (e *EmailUnverifiedError) Error() string { return "email not verified" }

const (
	// A verification link has a day; a reset link, which is a password in all but name,
	// an hour.
	emailVerifyTTL   = 24 * time.Hour
	passwordResetTTL = time.Hour
	// One email of a kind per minute per account or address: enough that a "resend"
	// works, little enough that a stolen session or a scripted /forgot cannot turn the
	// relay into someone's spam cannon.
	emailCooldown    = time.Minute
	emailTokenBytes  = 32
	minPasswordChars = 8
)

// EmailService is the address half of Service: confirming that an account's email reaches
// its owner, and the password reset that confirmation makes safe. Every link is a single-
// use random token kept hashed in Redis with a TTL, the same shape as a pending mfa_token
// (see service_2fa.go): a Redis dump alone must not redeem one, and nothing has to be
// swept because expiry is the store's job.
type EmailService interface {
	// EmailPolicy reports whether this instance can send email at all and whether a new
	// account must verify its address before signing in.
	EmailPolicy() (enabled, verificationRequired bool)
	// SendVerificationEmail issues a fresh link and mails it. ErrEmailCooldown when one
	// went out less than a minute ago.
	SendVerificationEmail(ctx context.Context, u *User) error
	// VerifyEmail redeems a link's token, marking the account verified.
	VerifyEmail(ctx context.Context, token string) (*User, error)
	// RequestPasswordReset mails a reset link if an account with that address exists.
	// It reports nil either way - the response must not say which addresses are
	// registered - and only errors on infrastructure failure.
	RequestPasswordReset(ctx context.Context, email string) error
	// ResetPassword redeems a reset token, sets the new password, marks the address
	// verified (the link proved it) and ends every existing session.
	ResetPassword(ctx context.Context, token, password string) (*User, error)
}

// MailSetter is implemented by the service NewService returns. Route setup uses it to hand
// in the mailer and the gateway hook without widening NewService's signature.
type MailSetter interface {
	SetMailer(m mail.Mailer)
	SetRevokeNotifier(fn func(ctx context.Context, userID int64))
}

func (s *service) SetMailer(m mail.Mailer) { s.mailer = m }

func (s *service) SetRevokeNotifier(fn func(ctx context.Context, userID int64)) {
	s.revokeNotify = fn
}

func (s *service) EmailPolicy() (bool, bool) {
	enabled := s.mailer != nil
	return enabled, enabled && s.cfg.Mail.VerificationRequired
}

// emailToken is what a verification or reset token resolves to. The address is recorded
// with the user so a link issued for one address can never confirm (or reset through) a
// different one the account has since moved to.
type emailToken struct {
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
}

func (s *service) emailVerifyKey(token string) string {
	return s.redisPrefix + "email:verify:" + mfaTokenHash(token)
}

func (s *service) passwordResetKey(token string) string {
	return s.redisPrefix + "email:reset:" + mfaTokenHash(token)
}

func (s *service) emailCooldownKey(kind, id string) string {
	return s.redisPrefix + "email:cooldown:" + kind + ":" + id
}

func newEmailToken() (string, error) {
	raw := make([]byte, emailTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// validEmailToken is the cheap shape check before Redis is asked: anything that is not
// what newEmailToken produces was never issued.
func validEmailToken(token string) bool {
	if len(token) != emailTokenBytes*2 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// takeCooldown claims this minute's slot for one kind of email to one target; false means
// one went out too recently. The claim is made before any work so that a failure can hand
// it back (releaseCooldown) and the person is not locked out of retrying.
func (s *service) takeCooldown(ctx context.Context, kind, id string) (bool, error) {
	return s.redis.SetNX(ctx, s.emailCooldownKey(kind, id), 1, emailCooldown).Result()
}

func (s *service) releaseCooldown(ctx context.Context, kind, id string) {
	_ = s.redis.Del(ctx, s.emailCooldownKey(kind, id)).Err()
}

// storeEmailToken issues a token resolving to u and keeps it under key(token) for ttl.
func (s *service) storeEmailToken(ctx context.Context, key func(string) string, u *User, ttl time.Duration) (string, error) {
	token, err := newEmailToken()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(emailToken{UserID: u.ID, Email: u.Email})
	if err != nil {
		return "", err
	}
	if err := s.redis.Set(ctx, key(token), payload, ttl).Err(); err != nil {
		return "", err
	}
	return token, nil
}

// redeemEmailToken resolves and burns a token in one step (GETDEL), so two clicks on the
// same link cannot both succeed. A missing token is "invalid", any other Redis failure is
// an error - the two must not be confused or an outage reads as "link expired".
func (s *service) redeemEmailToken(ctx context.Context, key string, invalid error) (*User, error) {
	raw, err := s.redis.GetDel(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, invalid
		}
		return nil, err
	}
	var p emailToken
	if err := json.Unmarshal(raw, &p); err != nil || p.UserID == 0 {
		return nil, invalid
	}
	// GetByID may answer from the Redis cache, which deliberately carries no password
	// hash (cached_user_repo.go) - so nothing here, or in the callers, may read
	// u.PasswordHash to decide anything.
	u, err := s.repo.GetByID(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if !canBeMailed(u) || !strings.EqualFold(u.Email, p.Email) {
		return nil, invalid
	}
	return u, nil
}

func (s *service) linkEmail(u *User, path, token, expiresIn string) mail.LinkEmail {
	return mail.LinkEmail{
		InstanceName: s.cfg.Mail.InstanceName,
		Username:     u.Username,
		Link:         s.cfg.Mail.LinkBase + path + "?token=" + token,
		ExpiresIn:    expiresIn,
	}
}

// canBeMailed says whether an account has an address of its own to write to: local
// accounts with a real email, not federation shadows (their home instance mails them) and
// not bots (their address is synthetic).
func canBeMailed(u *User) bool {
	return u != nil && !u.IsRemote() && !u.Bot && strings.Contains(u.Email, "@")
}

func (s *service) SendVerificationEmail(ctx context.Context, u *User) error {
	if s.mailer == nil || !canBeMailed(u) {
		return ErrEmailDisabled
	}
	if u.VerifiedEmail {
		return ErrEmailAlreadyVerified
	}
	id := fmt.Sprint(u.ID)
	fresh, err := s.takeCooldown(ctx, "verify", id)
	if err != nil {
		return err
	}
	if !fresh {
		return ErrEmailCooldown
	}
	token, err := s.storeEmailToken(ctx, s.emailVerifyKey, u, emailVerifyTTL)
	if err != nil {
		s.releaseCooldown(ctx, "verify", id)
		return err
	}
	msg, err := mail.Verification(s.linkEmail(u, "/verify-email", token, "24 hours"))
	if err != nil {
		s.releaseCooldown(ctx, "verify", id)
		return err
	}
	msg.To = u.Email
	if err := s.mailer.Send(ctx, msg); err != nil {
		// The token stays (harmless, it expires) but the minute is handed back so the
		// person can try again as soon as the relay is.
		s.releaseCooldown(ctx, "verify", id)
		return err
	}
	return nil
}

func (s *service) VerifyEmail(ctx context.Context, token string) (*User, error) {
	if !validEmailToken(token) {
		return nil, ErrEmailTokenInvalid
	}
	u, err := s.redeemEmailToken(ctx, s.emailVerifyKey(token), ErrEmailTokenInvalid)
	if err != nil {
		return nil, err
	}
	if !u.VerifiedEmail {
		if err := s.repo.SetEmailVerified(ctx, u.ID, true); err != nil {
			return nil, err
		}
		u.VerifiedEmail = true
	}
	return u, nil
}

func (s *service) RequestPasswordReset(ctx context.Context, email string) error {
	if s.mailer == nil {
		return ErrEmailDisabled
	}
	email = strings.ToLower(strings.TrimSpace(email))
	// The cooldown is keyed by the address and taken before the lookup, so a request for
	// an unknown address costs the same round trips as one for a known one.
	fresh, err := s.takeCooldown(ctx, "reset", mfaTokenHash(email))
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	u, err := s.repo.GetByEmail(ctx, email)
	if err != nil {
		return err
	}
	if !canBeMailed(u) || u.PasswordHash == "" {
		return nil
	}
	token, err := s.storeEmailToken(ctx, s.passwordResetKey, u, passwordResetTTL)
	if err != nil {
		return err
	}
	msg, err := mail.PasswordReset(s.linkEmail(u, "/reset-password", token, "1 hour"))
	if err != nil {
		return err
	}
	msg.To = u.Email
	// Delivered off the request, so the response takes as long whether or not an account
	// exists - a synchronous SMTP round trip here would be a reliable oracle for which
	// addresses are registered.
	userID := u.ID
	mailer := s.mailer
	safego.Go("mail", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := mailer.Send(ctx, msg); err != nil {
			logger.Err("auth", err, map[string]any{"stage": "password_reset_email", "user_id": userID})
		}
	})
	return nil
}

func (s *service) ResetPassword(ctx context.Context, token, password string) (*User, error) {
	if utf8.RuneCountInString(password) < minPasswordChars {
		return nil, ErrPasswordTooShort
	}
	if len(password) > bcryptMaxPasswordBytes {
		return nil, ErrPasswordTooLong
	}
	if !validEmailToken(token) {
		return nil, ErrResetTokenInvalid
	}
	// Whether the account has a password to reset was settled when the token was issued
	// (RequestPasswordReset looked the account up uncached); the token's existence is the
	// proof of that here.
	u, err := s.redeemEmailToken(ctx, s.passwordResetKey(token), ErrResetTokenInvalid)
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	if err := s.repo.SetPassword(ctx, u.ID, string(hash)); err != nil {
		return nil, err
	}
	u.PasswordHash = string(hash)
	// Opening the link proved the address, which is exactly what verification asks.
	if !u.VerifiedEmail {
		if err := s.repo.SetEmailVerified(ctx, u.ID, true); err != nil {
			logger.Err("auth", err, map[string]any{"stage": "reset_mark_verified", "user_id": u.ID})
		} else {
			u.VerifiedEmail = true
		}
	}
	// A reset is most often "someone else may know my password": every session the old
	// one opened ends now, and the gateway drops the sockets that held them. Best effort
	// after the password itself - that part already took.
	if err := s.srepo.RevokeAllForUser(ctx, u.ID); err != nil {
		logger.Err("auth", err, map[string]any{"stage": "reset_revoke_sessions", "user_id": u.ID})
	} else if s.revokeNotify != nil {
		s.revokeNotify(ctx, u.ID)
	}
	return u, nil
}
