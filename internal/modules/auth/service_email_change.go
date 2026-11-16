package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/mail"
	"github.com/StrafeChat/equinox/internal/safego"
)

// The self-service email change. In mail mode a change waits, as a pending record in
// Redis, for the link sent to the new address to be opened from the account's own signed-in
// session; in direct mode (no SMTP) it applies as soon as the person re-authenticates, and
// the new address is unverified. Either way the address moves only through
// ApplyEmailChange (email_change.go). No session is revoked, nothing is broadcast but the
// self-only USER_EMAIL_UPDATE, and no log line carries an address.

var (
	ErrSecondFactorRequired    = errors.New("a code from your authenticator app or a recovery code is required")
	ErrSessionRequired         = errors.New("only a signed-in person can change the account's email")
	ErrEmailChangeWrongAccount = errors.New("this link is for a different account")
	ErrNoPendingEmailChange    = errors.New("there is no email change waiting to be confirmed")
	ErrEmailChangeTokenInvalid = errors.New("that email change link is not valid or has expired")
	ErrEmailTargetLimited      = errors.New("too many emails have been sent to that address - try again later")
	ErrReauthLocked            = errors.New("too many failed attempts - try again in an hour")

	// Body errors, all answered 400 invalid_body.
	ErrEmailChangeFieldsRequired = errors.New("email and password are required")
	ErrStepUpBothCodes           = errors.New("send code or recovery_code, not both")
	ErrEmailChangeTokenRequired  = errors.New("token is required")
)

const (
	// emailChangeTTL is how long a pending change (and its link) lives.
	emailChangeTTL = time.Hour
	// emailChangeTargetMax bounds how many change mails one address can receive an hour,
	// across every account asking: the relay must not become a way to mail a stranger.
	emailChangeTargetMax = 3
	// emailChangeReauthMax failed step-ups an hour lock the account's change endpoint;
	// the IP limiter alone would allow hundreds of password guesses an hour.
	emailChangeReauthMax = 10
	// emailChangeResendMinTTL: a resend this close to expiry would mail a link that dies
	// before it is read.
	emailChangeResendMinTTL = 60 * time.Second
)

type EmailChangeInput struct{ Email, Password, Code, RecoveryCode string }

type PendingEmailChange struct {
	Email     string
	ExpiresAt time.Time
}

// EmailState is what every email endpoint answers: the address, whether it is verified, and
// the change waiting for its link (nil when none). Pending looks the same whether or not the
// requested address belongs to another account.
type EmailState struct {
	Email    string
	Verified bool
	Pending  *PendingEmailChange
}

func (s *service) changeDeps() EmailChangeDeps {
	return EmailChangeDeps{Users: s.repo, Redis: s.redis, Prefix: s.redisPrefix, Notify: s.emailUpdateNotify}
}

func (s *service) emailChangeReauthKey(userID int64) string {
	return s.redisPrefix + "email:change:reauth:" + strconv.FormatInt(userID, 10)
}

func (s *service) emailChangeTargetKey(email string) string {
	return s.redisPrefix + "email:change:target:" + mfaTokenHash(email)
}

// freshUser is the session user re-read uncached. The session's *User comes from the cached
// GetByID and may predate an address change; GetByEmail always reads the index and the
// row. If the session copy is stale, the row it now points at is tried once; anything else
// mid-change is ErrEmailChangeBusy rather than a decision on stale data.
func (s *service) freshUser(ctx context.Context, u *User) (*User, error) {
	if u == nil || u.Bot || u.System || u.IsRemote() || !strings.Contains(u.Email, "@") {
		return nil, ErrSessionRequired
	}
	real, err := s.repo.GetByEmail(ctx, strings.ToLower(u.Email))
	if err != nil {
		return nil, err
	}
	if real != nil && real.ID == u.ID && strings.EqualFold(real.Email, u.Email) {
		return real, nil
	}
	byID, err := s.repo.GetByID(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	if byID != nil && !strings.EqualFold(byID.Email, u.Email) && strings.Contains(byID.Email, "@") {
		real, err = s.repo.GetByEmail(ctx, strings.ToLower(byID.Email))
		if err != nil {
			return nil, err
		}
		if real != nil && real.ID == u.ID && strings.EqualFold(real.Email, byID.Email) {
			return real, nil
		}
	}
	return nil, ErrEmailChangeBusy
}

func (s *service) readPendingEmailChange(ctx context.Context, userID int64) (*pendingEmailChangeRecord, error) {
	if s.redis == nil {
		return nil, nil
	}
	raw, err := s.redis.Get(ctx, emailChangePendingKey(s.redisPrefix, userID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var p pendingEmailChangeRecord
	if json.Unmarshal(raw, &p) != nil || p.NewEmail == "" {
		return nil, nil
	}
	return &p, nil
}

// clearPendingEmailChange drops the pending record and the token key it names.
func (s *service) clearPendingEmailChange(ctx context.Context, userID int64) error {
	if s.redis == nil {
		return nil
	}
	p, err := s.readPendingEmailChange(ctx, userID)
	if err != nil {
		return err
	}
	keys := []string{emailChangePendingKey(s.redisPrefix, userID)}
	if p != nil && p.TokenHash != "" {
		keys = append(keys, emailChangeTokenKey(s.redisPrefix, p.TokenHash))
	}
	if err := s.redis.Del(ctx, keys...).Err(); err != nil {
		logger.Err("auth", err, map[string]any{"stage": "email_change_clear", "user_id": userID})
		return err
	}
	return nil
}

// emailStateFor renders the state of an account read uncached. A pending record made from
// an address the account no longer holds is moot and not shown.
func emailStateFor(u *User, p *pendingEmailChangeRecord) *EmailState {
	st := &EmailState{Email: u.Email, Verified: u.VerifiedEmail}
	if p != nil && strings.EqualFold(p.OldEmail, u.Email) {
		st.Pending = &PendingEmailChange{Email: p.NewEmail, ExpiresAt: time.UnixMilli(p.ExpiresAt).UTC()}
	}
	return st
}

func (s *service) GetEmailState(ctx context.Context, u *User) (*EmailState, error) {
	real, err := s.freshUser(ctx, u)
	if err != nil {
		return nil, err
	}
	p, err := s.readPendingEmailChange(ctx, real.ID)
	if err != nil {
		return nil, err
	}
	return emailStateFor(real, p), nil
}

// stepUpFactor is the second factor a step-up will check.
type stepUpFactor int

const (
	factorNone stepUpFactor = iota
	factorTOTP
	factorRecovery
)

// stepUpPlan is the step-up decision table, pure so it is testable without Redis or Scylla.
// totpEnabled means TOTP is on with a stored secret. A passkey cannot be used to step up in
// v1 (its ceremony is tied to a login mfa_token), so a passkey-only account gives one of its
// recovery codes, which always exist once any factor is on.
func stepUpPlan(has2FA, totpEnabled bool, code, recovery string) (stepUpFactor, error) {
	code, recovery = strings.TrimSpace(code), strings.TrimSpace(recovery)
	if code != "" && recovery != "" {
		return factorNone, ErrStepUpBothCodes
	}
	if !has2FA {
		// Any code sent for an account without a second factor is ignored.
		return factorNone, nil
	}
	switch {
	case code != "":
		if !totpEnabled {
			return factorNone, ErrInvalidTOTPCode
		}
		return factorTOTP, nil
	case recovery != "":
		return factorRecovery, nil
	default:
		return factorNone, ErrSecondFactorRequired
	}
}

// verifyStepUp re-authenticates the session's person: the password, and a TOTP code or a
// recovery code when the account has a second factor. It returns the uncached row. Wrong
// passwords and codes count against the account's hourly budget (emailChangeReauthMax),
// which the caller checks first; a missing code does not.
//
// Known weak link, not fixed here: DisableTOTP, RegenerateRecoveryCodes and
// DeleteWebAuthnCredential need only the password, so someone holding a session and the
// password can strip or replace the second factor first and then pass this check. Until
// those three demand the factor too, the factor here is defence in depth.
func (s *service) verifyStepUp(ctx context.Context, u *User, password, code, recovery string) (*User, error) {
	real, err := passwordMatches(ctx, s.repo, u, password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			s.countReauthFailure(ctx, u.ID)
		}
		return nil, err
	}
	// Uncached, like Login: the cached user carries no TOTP state.
	enabled, cipher, err := s.tfrepo.GetTOTPState(ctx, real.ID)
	if err != nil {
		return nil, err
	}
	creds, err := s.tfrepo.ListWebAuthnCredentials(ctx, real.ID)
	if err != nil {
		return nil, err
	}
	factor, err := stepUpPlan(enabled || len(creds) > 0, enabled && cipher != "", code, recovery)
	if err != nil {
		if errors.Is(err, ErrInvalidTOTPCode) {
			s.countReauthFailure(ctx, real.ID)
		}
		return nil, err
	}
	switch factor {
	case factorTOTP:
		secret, err := decryptTOTPSecret(s.cfg.TwoFactor.TOTPEncryptionKey, cipher)
		if err != nil {
			return nil, err
		}
		// The replay guard: a code accepted once (here or at login) is refused again.
		if err := s.verifyTOTPCode(ctx, real.ID, code, secret); err != nil {
			if errors.Is(err, ErrInvalidTOTPCode) {
				s.countReauthFailure(ctx, real.ID)
			}
			return nil, err
		}
	case factorRecovery:
		applied, err := s.tfrepo.ConsumeRecoveryCode(ctx, real.ID, hashRecoveryCode(strings.TrimSpace(recovery)))
		if err != nil {
			return nil, err
		}
		if !applied {
			s.countReauthFailure(ctx, real.ID)
			return nil, ErrInvalidRecoveryCode
		}
	}
	if s.redis != nil {
		_ = s.redis.Del(ctx, s.emailChangeReauthKey(real.ID)).Err()
	}
	return real, nil
}

func (s *service) countReauthFailure(ctx context.Context, userID int64) {
	if s.redis == nil {
		return
	}
	key := s.emailChangeReauthKey(userID)
	n, err := s.redis.Incr(ctx, key).Result()
	if err != nil {
		logger.Err("auth", err, map[string]any{"stage": "email_change_reauth_count", "user_id": userID})
		return
	}
	if n == 1 {
		_ = s.redis.Expire(ctx, key, time.Hour).Err()
	}
}

func (s *service) reauthLocked(ctx context.Context, userID int64) (bool, error) {
	if s.redis == nil {
		return false, nil
	}
	n, err := s.redis.Get(ctx, s.emailChangeReauthKey(userID)).Int()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, err
	}
	return n >= emailChangeReauthMax, nil
}

// checkChangeMailBudget peeks at the per-user cooldown and the per-target counter without
// taking either, so a refusal costs the person no TOTP step and no recovery code.
func (s *service) checkChangeMailBudget(ctx context.Context, userID int64, target string) error {
	held, err := s.redis.Exists(ctx, s.emailCooldownKey("change", strconv.FormatInt(userID, 10))).Result()
	if err != nil {
		return err
	}
	if held > 0 {
		return ErrEmailCooldown
	}
	n, err := s.redis.Get(ctx, s.emailChangeTargetKey(target)).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	if n >= emailChangeTargetMax {
		return ErrEmailTargetLimited
	}
	return nil
}

// takeChangeMailBudget claims the minute's cooldown and counts one mail to target. On a
// failure after the cooldown was taken it hands the minute back.
func (s *service) takeChangeMailBudget(ctx context.Context, userID int64, target string) error {
	id := strconv.FormatInt(userID, 10)
	fresh, err := s.takeCooldown(ctx, "change", id)
	if err != nil {
		return err
	}
	if !fresh {
		return ErrEmailCooldown
	}
	key := s.emailChangeTargetKey(target)
	n, err := s.redis.Incr(ctx, key).Result()
	if err != nil {
		s.releaseCooldown(ctx, "change", id)
		return err
	}
	if n == 1 {
		_ = s.redis.Expire(ctx, key, time.Hour).Err()
	}
	return nil
}

// queueMail renders and sends off the request, like the password reset: a synchronous SMTP
// round trip would make the response time say which branch ran.
func (s *service) queueMail(stage string, userID int64, to string, build func() (mail.Message, error)) {
	if s.mailer == nil || !strings.Contains(to, "@") {
		return
	}
	msg, err := build()
	if err != nil {
		logger.Err("auth", err, map[string]any{"stage": stage, "user_id": userID})
		return
	}
	msg.To = to
	mailer := s.mailer
	safego.Go("mail", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := mailer.Send(ctx, msg); err != nil {
			logger.Err("auth", err, map[string]any{"stage": stage, "user_id": userID})
		}
	})
}

func (s *service) noticeEmail(u *User, newEmail, expiresIn string) mail.NoticeEmail {
	return mail.NoticeEmail{
		InstanceName: s.cfg.Mail.InstanceName,
		Username:     u.Username,
		MaskedEmail:  MaskEmail(newEmail),
		ExpiresIn:    expiresIn,
	}
}

// expiresInPhrase is the "expires in" of a link with ttl left: "1 hour" at issue, minutes
// on a resend.
func expiresInPhrase(ttl time.Duration) string {
	mins := int((ttl + time.Minute - 1) / time.Minute)
	if mins >= 60 {
		return "1 hour"
	}
	if mins <= 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", mins)
}

// queueConfirmMail sends the link (3.1) to a free address, or the "already registered" mail
// (3.2) to a taken one.
func (s *service) queueConfirmMail(u *User, rec *pendingEmailChangeRecord, token string, ttl time.Duration) {
	if rec.Taken {
		s.queueMail("email_change_taken", u.ID, rec.NewEmail, func() (mail.Message, error) {
			return mail.EmailChangeTaken(mail.NoticeEmail{InstanceName: s.cfg.Mail.InstanceName})
		})
		return
	}
	s.queueMail("email_change_confirm", u.ID, rec.NewEmail, func() (mail.Message, error) {
		return mail.EmailChangeConfirm(s.linkEmail(u, "/confirm-email", token, expiresInPhrase(ttl)))
	})
}

func (s *service) RequestEmailChange(ctx context.Context, u *User, in EmailChangeInput) (*EmailState, bool, error) {
	if strings.TrimSpace(in.Email) == "" || in.Password == "" {
		return nil, false, ErrEmailChangeFieldsRequired
	}
	code, recovery := strings.TrimSpace(in.Code), strings.TrimSpace(in.RecoveryCode)
	if code != "" && recovery != "" {
		return nil, false, ErrStepUpBothCodes
	}
	newEmail, err := NormalizeEmail(in.Email)
	if err != nil {
		return nil, false, err
	}
	cur, err := s.freshUser(ctx, u)
	if err != nil {
		return nil, false, err
	}
	if strings.EqualFold(newEmail, cur.Email) {
		return nil, false, ErrEmailUnchanged
	}
	if s.emailDomainRefused(newEmail) {
		return nil, false, ErrDisposableEmail
	}
	if locked, err := s.reauthLocked(ctx, cur.ID); err != nil {
		return nil, false, err
	} else if locked {
		return nil, false, ErrReauthLocked
	}
	mailMode := s.mailer != nil
	if mailMode {
		if err := s.checkChangeMailBudget(ctx, cur.ID, newEmail); err != nil {
			return nil, false, err
		}
	}
	real, err := s.verifyStepUp(ctx, cur, in.Password, code, recovery)
	if err != nil {
		return nil, false, err
	}

	if !mailMode {
		// Revealing that the address is taken (409) is accepted here: it sits behind
		// re-authentication and the limiters, and registration reveals it anyway.
		if err := ApplyEmailChange(ctx, s.changeDeps(), real.ID, real.Email, newEmail, false); err != nil {
			return nil, false, err
		}
		logger.Info("auth", "email changed without confirmation (no mail) user_id=%d", real.ID)
		return &EmailState{Email: newEmail, Verified: false}, true, nil
	}

	if err := s.takeChangeMailBudget(ctx, real.ID, newEmail); err != nil {
		return nil, false, err
	}
	// From here on both branches (address free or taken) make the same round trips: one
	// index read, one pending read and one transaction, so the answer and its timing say
	// nothing about whether the address is registered.
	taken, err := s.repo.EmailExists(ctx, newEmail)
	if err != nil {
		s.releaseCooldown(ctx, "change", strconv.FormatInt(real.ID, 10))
		return nil, false, err
	}
	rec := &pendingEmailChangeRecord{
		OldEmail:  real.Email,
		NewEmail:  newEmail,
		Taken:     taken,
		ExpiresAt: time.Now().Add(emailChangeTTL).UnixMilli(),
	}
	token := ""
	if !taken {
		if token, err = newEmailToken(); err != nil {
			s.releaseCooldown(ctx, "change", strconv.FormatInt(real.ID, 10))
			return nil, false, err
		}
		rec.TokenHash = mfaTokenHash(token)
	}
	if err := s.writePendingEmailChange(ctx, real.ID, rec, emailChangeTTL); err != nil {
		s.releaseCooldown(ctx, "change", strconv.FormatInt(real.ID, 10))
		return nil, false, err
	}
	s.queueConfirmMail(real, rec, token, emailChangeTTL)
	// The old address is told whether or not the new one is taken.
	s.queueMail("email_change_requested", real.ID, real.Email, func() (mail.Message, error) {
		return mail.EmailChangeRequested(s.noticeEmail(real, newEmail, "1 hour"))
	})
	logger.Info("auth", "email change requested user_id=%d taken=%t", real.ID, taken)
	return emailStateFor(real, rec), false, nil
}

// writePendingEmailChange stores rec as the account's one pending change, superseding any
// earlier one: the earlier link's token key is deleted in the same transaction, so it stops
// working the moment this one exists.
func (s *service) writePendingEmailChange(ctx context.Context, userID int64, rec *pendingEmailChangeRecord, ttl time.Duration) error {
	old, err := s.readPendingEmailChange(ctx, userID)
	if err != nil {
		return err
	}
	recRaw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	var tokRaw []byte
	if rec.TokenHash != "" {
		if tokRaw, err = json.Marshal(emailChangeTokenPayload{UserID: userID, OldEmail: rec.OldEmail, NewEmail: rec.NewEmail}); err != nil {
			return err
		}
	}
	_, err = s.redis.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, emailChangePendingKey(s.redisPrefix, userID), recRaw, ttl)
		if tokRaw != nil {
			pipe.Set(ctx, emailChangeTokenKey(s.redisPrefix, rec.TokenHash), tokRaw, ttl)
		}
		if old != nil && old.TokenHash != "" && old.TokenHash != rec.TokenHash {
			pipe.Del(ctx, emailChangeTokenKey(s.redisPrefix, old.TokenHash))
		}
		return nil
	})
	return err
}

func (s *service) ResendEmailChange(ctx context.Context, u *User) (*EmailState, error) {
	if s.mailer == nil {
		return nil, ErrEmailDisabled
	}
	cur, err := s.freshUser(ctx, u)
	if err != nil {
		return nil, err
	}
	rec, err := s.readPendingEmailChange(ctx, cur.ID)
	if err != nil {
		return nil, err
	}
	if rec == nil || !strings.EqualFold(rec.OldEmail, cur.Email) {
		return nil, ErrNoPendingEmailChange
	}
	ttl, err := s.redis.PTTL(ctx, emailChangePendingKey(s.redisPrefix, cur.ID)).Result()
	if err != nil {
		return nil, err
	}
	if ttl < emailChangeResendMinTTL {
		return nil, ErrNoPendingEmailChange
	}
	if err := s.checkChangeMailBudget(ctx, cur.ID, rec.NewEmail); err != nil {
		return nil, err
	}
	if err := s.takeChangeMailBudget(ctx, cur.ID, rec.NewEmail); err != nil {
		return nil, err
	}
	if testHookResendBeforeWrite != nil {
		testHookResendBeforeWrite()
	}
	token, ttl, err := s.reissuePendingToken(ctx, cur.ID, rec)
	if err != nil {
		s.releaseCooldown(ctx, "change", strconv.FormatInt(cur.ID, 10))
		if errors.Is(err, redis.TxFailedErr) {
			// The record moved under us (a cancel, a confirm, a reset or a new request):
			// whatever was resent would be for a change that no longer waits.
			return nil, ErrNoPendingEmailChange
		}
		return nil, err
	}
	// The old-address notice is not sent again: it already went with the request.
	s.queueConfirmMail(cur, rec, token, ttl)
	logger.Info("auth", "email change resent user_id=%d", cur.ID)
	return emailStateFor(cur, rec), nil
}

// testHookResendBeforeWrite runs between the resend's read of the pending record and its
// write; tests use it to land a cancel in that gap. Always nil outside tests.
var testHookResendBeforeWrite func()

// reissuePendingToken gives the pending change rec a new token (when its address was free)
// with the record's remaining life, and returns it with that TTL. It runs under WATCH on the
// pending record and writes only if the record is still exactly rec: a cancel, a confirm, a
// password reset or a newer request in between makes it fail with redis.TxFailedErr (or
// ErrNoPendingEmailChange when the record is already gone), instead of writing the
// record back - which, with KEEPTTL on a key that no longer exists, would revive a cancelled
// change with no expiry at all.
func (s *service) reissuePendingToken(ctx context.Context, userID int64, rec *pendingEmailChangeRecord) (string, time.Duration, error) {
	pendingKey := emailChangePendingKey(s.redisPrefix, userID)
	want, err := json.Marshal(rec)
	if err != nil {
		return "", 0, err
	}
	var token string
	var ttl time.Duration
	err = s.redis.Watch(ctx, func(tx *redis.Tx) error {
		raw, err := tx.Get(ctx, pendingKey).Bytes()
		if errors.Is(err, redis.Nil) {
			return ErrNoPendingEmailChange
		}
		if err != nil {
			return err
		}
		var now pendingEmailChangeRecord
		if json.Unmarshal(raw, &now) != nil {
			return ErrNoPendingEmailChange
		}
		if again, _ := json.Marshal(now); string(again) != string(want) {
			return ErrNoPendingEmailChange
		}
		if ttl, err = tx.PTTL(ctx, pendingKey).Result(); err != nil {
			return err
		}
		if ttl < emailChangeResendMinTTL {
			return ErrNoPendingEmailChange
		}
		if rec.Taken {
			// No link to reissue; the "already registered" mail is just sent again.
			return nil
		}
		// A new token with the pending change's remaining life; the old one dies with it.
		if token, err = newEmailToken(); err != nil {
			return err
		}
		next := *rec
		next.TokenHash = mfaTokenHash(token)
		recRaw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		tokRaw, err := json.Marshal(emailChangeTokenPayload{UserID: userID, OldEmail: rec.OldEmail, NewEmail: rec.NewEmail})
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, emailChangeTokenKey(s.redisPrefix, next.TokenHash), tokRaw, ttl)
			if rec.TokenHash != "" {
				pipe.Del(ctx, emailChangeTokenKey(s.redisPrefix, rec.TokenHash))
			}
			pipe.Set(ctx, pendingKey, recRaw, redis.KeepTTL)
			return nil
		})
		if err == nil {
			rec.TokenHash = next.TokenHash
		}
		return err
	}, pendingKey)
	if err != nil {
		return "", 0, err
	}
	return token, ttl, nil
}

func (s *service) CancelEmailChange(ctx context.Context, u *User) (*EmailState, error) {
	cur, err := s.freshUser(ctx, u)
	if err != nil {
		return nil, err
	}
	if err := s.clearPendingEmailChange(ctx, cur.ID); err != nil {
		return nil, err
	}
	logger.Info("auth", "email change cancelled user_id=%d", cur.ID)
	return emailStateFor(cur, nil), nil
}

// emailChangeDoneFor reports whether the token with this hash already confirmed a change
// for userID within the hour - the idempotent path for a double click or a refresh.
func (s *service) emailChangeDoneFor(ctx context.Context, tokenHash string, userID int64) (bool, error) {
	v, err := s.redis.Get(ctx, emailChangeDoneKey(s.redisPrefix, tokenHash)).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, err
	}
	return v == userID, nil
}

func (s *service) ConfirmEmailChange(ctx context.Context, u *User, token string) (*EmailState, error) {
	token = strings.ToLower(strings.TrimSpace(token))
	if token == "" {
		return nil, ErrEmailChangeTokenRequired
	}
	if u == nil || u.Bot || u.System || u.IsRemote() {
		return nil, ErrSessionRequired
	}
	if !validEmailToken(token) || s.redis == nil {
		return nil, ErrEmailChangeTokenInvalid
	}
	hash := mfaTokenHash(token)
	alreadyDone := func() (*EmailState, error) {
		done, err := s.emailChangeDoneFor(ctx, hash, u.ID)
		if err != nil {
			return nil, err
		}
		if !done {
			return nil, ErrEmailChangeTokenInvalid
		}
		return s.GetEmailState(ctx, u)
	}

	raw, err := s.redis.Get(ctx, emailChangeTokenKey(s.redisPrefix, hash)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return alreadyDone()
		}
		return nil, err
	}
	var p emailChangeTokenPayload
	if json.Unmarshal(raw, &p) != nil || p.UserID == 0 {
		return nil, ErrEmailChangeTokenInvalid
	}
	if p.UserID != u.ID {
		// Left untouched: the right account may still open it.
		return nil, ErrEmailChangeWrongAccount
	}
	rec, err := s.readPendingEmailChange(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.TokenHash != hash {
		// Superseded, cancelled or expired - or a concurrent confirm of this very token
		// just finished, which the done key (written with the pending clear) says.
		return alreadyDone()
	}
	// The operator's rules may have changed in the hour the link was open.
	newEmail, err := NormalizeEmail(rec.NewEmail)
	if err != nil {
		return nil, err
	}
	if s.emailDomainRefused(newEmail) {
		return nil, ErrDisposableEmail
	}
	applied, err := applyEmailChange(ctx, s.changeDeps(), u.ID, rec.OldEmail, newEmail, true, func(pipe redis.Pipeliner) {
		pipe.Set(ctx, emailChangeDoneKey(s.redisPrefix, hash), u.ID, emailChangeTTL)
	})
	switch {
	case errors.Is(err, ErrEmailInUse), errors.Is(err, ErrEmailChangeConflict):
		_ = s.clearPendingEmailChange(ctx, u.ID)
		return nil, err
	case err != nil:
		return nil, err
	}
	if !applied {
		// A concurrent confirm of the same change made it; this one only reports.
		return &EmailState{Email: newEmail, Verified: true}, nil
	}
	s.queueMail("email_change_done", u.ID, rec.OldEmail, func() (mail.Message, error) {
		return mail.EmailChangeDone(s.noticeEmail(u, newEmail, ""))
	})
	logger.Info("auth", "email change confirmed user_id=%d", u.ID)
	return &EmailState{Email: newEmail, Verified: true}, nil
}
