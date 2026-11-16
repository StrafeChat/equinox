package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/logger"
)

var (
	// ErrEmailChangeConflict: the account no longer holds the address the change was made
	// from - another change, or an administrator, moved it in the meantime.
	ErrEmailChangeConflict = errors.New("your email address changed after this link was sent")
	// ErrEmailChangeBusy: another change to the same account holds the per-user lock, or
	// the session's cached account disagrees with the stored one mid-change.
	ErrEmailChangeBusy = errors.New("another change to this account's email is in progress")
	// ErrEmailUnchanged: the "new" address is the current one (compared case-insensitively).
	ErrEmailUnchanged = errors.New("that is already your email address")
)

// EventUserEmailUpdate is the gateway event that tells an account's OWN sessions their
// address or its verification changed. It exists, under its own name, so that the address
// never has a reason to ride on USER_UPDATE: that event goes to friends and to every room
// the user is in, and broadcastUserProfile also feeds federation. Publish it only with
// stargate.PublishToUser.
const EventUserEmailUpdate = "USER_EMAIL_UPDATE"

// EmailUpdatePayload is USER_EMAIL_UPDATE's data, exactly {"email", "verified_email"}.
func EmailUpdatePayload(email string, verified bool) map[string]any {
	return map[string]any{"email": email, "verified_email": verified}
}

const (
	// emailChangeLockTTL bounds how long a crashed holder can block the account's next
	// change; the lock is normally released within milliseconds.
	emailChangeLockTTL = 15 * time.Second
	// emailChangeLockWait is how long ApplyEmailChange waits for the lock before
	// answering ErrEmailChangeBusy, polling every emailChangeLockPoll.
	emailChangeLockWait = 3 * time.Second
	emailChangeLockPoll = 100 * time.Millisecond
	// emailChangedMarkerTTL outlives the longest email token (verification, 24 h), so a
	// token issued before a change can never revive by the account moving back.
	emailChangedMarkerTTL = 25 * time.Hour
)

// RedisKeyPrefix is REDIS_CACHE_PREFIX with its trailing ':' - the prefix every auth key in
// Redis carries. Exported so the instance module's ApplyEmailChange calls write the same
// keys the auth service reads.
func RedisKeyPrefix(cfg *config.Config) string {
	prefix := cfg.Database.Redis.CachePrefix
	if prefix != "" && !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}
	return prefix
}

// EmailChangeDeps is what ApplyEmailChange needs. Redis is nil only in unit tests: then
// there is no lock, no revival marker and no pending change to clear.
type EmailChangeDeps struct {
	Users  UserRepository
	Redis  *redis.Client
	Prefix string // the Redis prefix with its trailing ':' (RedisKeyPrefix)
	// Notify publishes USER_EMAIL_UPDATE to the account's own channel. May be nil.
	Notify func(ctx context.Context, userID int64, email string, verified bool)
}

func emailChangeLockKey(prefix string, userID int64) string {
	return prefix + "email:change:lock:" + strconv.FormatInt(userID, 10)
}

func emailChangedKey(prefix string, userID int64) string {
	return prefix + "email:changed:" + strconv.FormatInt(userID, 10)
}

func emailChangePendingKey(prefix string, userID int64) string {
	return prefix + "email:change:pending:" + strconv.FormatInt(userID, 10)
}

// emailChangeTokenKey is keyed by the token's hash (H in the spec), never the token.
func emailChangeTokenKey(prefix, tokenHash string) string {
	return prefix + "email:change:token:" + tokenHash
}

func emailChangeDoneKey(prefix, tokenHash string) string {
	return prefix + "email:change:done:" + tokenHash
}

// pendingEmailChangeRecord is P email:change:pending:<uid>: the one change waiting for its
// link. It is the authority - a token key works only while TokenHash names it - so
// superseding or cancelling is a matter of rewriting or deleting this one record.
type pendingEmailChangeRecord struct {
	OldEmail  string `json:"old_email"`
	NewEmail  string `json:"new_email"`
	TokenHash string `json:"token_hash"` // "" when Taken: no link was ever issued
	Taken     bool   `json:"taken"`
	ExpiresAt int64  `json:"expires_at"` // unix ms
}

// emailChangeTokenPayload is P email:change:token:<H(token)>.
type emailChangeTokenPayload struct {
	UserID   int64  `json:"user_id"`
	OldEmail string `json:"old_email"`
	NewEmail string `json:"new_email"`
}

// unlockScript deletes the lock only if it still holds our value, so a holder whose lock
// expired cannot free the next holder's.
var unlockScript = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

func takeEmailChangeLock(ctx context.Context, rdb *redis.Client, key string) (func(), error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	val := hex.EncodeToString(raw)
	deadline := time.Now().Add(emailChangeLockWait)
	for {
		ok, err := rdb.SetNX(ctx, key, val, emailChangeLockTTL).Result()
		if err != nil {
			return nil, err
		}
		if ok {
			released := false
			return func() {
				if released {
					return
				}
				released = true
				_ = unlockScript.Run(context.Background(), rdb, []string{key}, val).Err()
			}, nil
		}
		if time.Now().Add(emailChangeLockPoll).After(deadline) {
			return nil, ErrEmailChangeBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(emailChangeLockPoll):
		}
	}
}

// ApplyEmailChange is the only way an account's address moves: the confirmed self-service
// change, the direct one on an instance without mail, and the administrator's tool all come
// through here, and nothing calls UserRepository.ChangeEmail directly. Under a per-user lock
// it checks the account still holds oldEmail (uncached), moves it, records when it moved
// (so verify and reset tokens issued before can never redeem again, even if the account
// moves back), drops any pending change and, once the lock is released, tells the account's
// own sessions through Notify.
//
// Returns nil without changing anything when the account already holds newEmail (a second,
// concurrent confirm of the same change). Errors from ChangeEmail are returned unchanged.
func ApplyEmailChange(ctx context.Context, d EmailChangeDeps, userID int64, oldEmail, newEmail string, verified bool) error {
	_, err := applyEmailChange(ctx, d, userID, oldEmail, newEmail, verified, nil)
	return err
}

// applyEmailChange reports whether this call made the change (false on the idempotent
// path). extra runs in the same transaction that clears the pending change, so a reader can
// never see the pending record gone without what extra wrote (the confirm's done key).
func applyEmailChange(ctx context.Context, d EmailChangeDeps, userID int64, oldEmail, newEmail string, verified bool, extra func(pipe redis.Pipeliner)) (bool, error) {
	release := func() {}
	if d.Redis != nil {
		var err error
		release, err = takeEmailChangeLock(ctx, d.Redis, emailChangeLockKey(d.Prefix, userID))
		if err != nil {
			return false, err
		}
	}
	defer release()

	if oldEmail != "" {
		cur, err := d.Users.GetByEmail(ctx, strings.ToLower(oldEmail))
		if err != nil {
			return false, err
		}
		if cur == nil || cur.ID != userID || !strings.EqualFold(cur.Email, oldEmail) {
			now, err := d.Users.GetByEmail(ctx, strings.ToLower(newEmail))
			if err != nil {
				return false, err
			}
			if now != nil && now.ID == userID && strings.EqualFold(now.Email, newEmail) {
				return false, nil
			}
			return false, ErrEmailChangeConflict
		}
	}

	if err := d.Users.ChangeEmail(ctx, userID, oldEmail, newEmail, verified); err != nil {
		return false, err
	}

	if d.Redis != nil {
		pendingKey := emailChangePendingKey(d.Prefix, userID)
		var tokenKey string
		if raw, err := d.Redis.Get(ctx, pendingKey).Bytes(); err == nil {
			var p pendingEmailChangeRecord
			if json.Unmarshal(raw, &p) == nil && p.TokenHash != "" {
				tokenKey = emailChangeTokenKey(d.Prefix, p.TokenHash)
			}
		}
		_, err := d.Redis.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, emailChangedKey(d.Prefix, userID), time.Now().UnixMilli(), emailChangedMarkerTTL)
			pipe.Del(ctx, pendingKey)
			if tokenKey != "" {
				pipe.Del(ctx, tokenKey)
			}
			if extra != nil {
				extra(pipe)
			}
			return nil
		})
		if err != nil {
			// The address already moved. Without the marker an old reset link could revive
			// if the account moves back within the hour - log it, the change still stands.
			logger.Err("auth", err, map[string]any{"stage": "email_change_marker", "user_id": userID})
		}
	}

	release()
	if d.Notify != nil {
		d.Notify(ctx, userID, newEmail, verified)
	}
	return true, nil
}
