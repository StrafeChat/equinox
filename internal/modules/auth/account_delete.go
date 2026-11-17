package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gocql/gocql"
	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/safego"
)

// Account deletion (an instance administrator removing a local account or bot; the
// pipeline is internal/modules/instance/account_delete.go) and the peer side of it
// (a shadow whose home deleted the account becomes a tombstone, federation/account_delete.go).

// zombieSweepDelay is how long after DeleteAccount the row is looked for again. A writer
// that read the row just before the delete and writes just after it (the gateway's
// disconnect presence write, a relationship add holding a stale cached user) recreates a
// partial row - Cassandra's UPDATE is an upsert. Ids are never reused, so any row under a
// deleted id after this point is such a zombie and is removed again. A var for tests.
var zombieSweepDelay = 20 * time.Second

// cacheRedropDelay is how long after the first invalidation the cached repository drops a
// deleted account's entries again (a cache-aside read in flight can re-populate after the
// first pass, as in ChangeEmail). A var for tests.
var cacheRedropDelay = 2 * time.Second

// DeleteAccount removes a local account's identity in one logged batch: the users row and
// its users_by_username and users_by_email claims, so the name and address are free the
// moment it returns. The claims are read first and deleted only while they still point at
// this account - an address released by an earlier email change and claimed by someone
// else is never touched. Unconditional deletes are otherwise safe: while our index rows
// exist nobody else can claim the name or the address (their claim is IF NOT EXISTS).
//
// u is only used for its id; the username and email come from a fresh, uncached read of the
// row, so a stale cached copy cannot free the wrong name. Idempotent: a missing row with
// missing claims is a no-op. Schedules the zombie sweep (zombieSweepDelay).
func (r *scyllaUserRepo) DeleteAccount(ctx context.Context, u *User) error {
	if u == nil || u.ID == 0 {
		return errors.New("delete account: no user")
	}
	if err := r.deleteAccountRows(ctx, u.ID, u.Username, u.Email); err != nil {
		return err
	}
	userID, delay := u.ID, zombieSweepDelay
	safego.Go("auth", func() {
		time.Sleep(delay)
		if removed, err := r.sweepZombie(context.Background(), userID); err != nil {
			logger.Err("auth", err, map[string]any{"stage": "account_delete_sweep", "user_id": userID})
		} else if removed {
			logger.Warn("auth", "account_delete zombie row removed user_id=%d", userID)
		}
	})
	return nil
}

// deleteAccountRows is DeleteAccount without the sweep. fallbackUsername/fallbackEmail are
// what the caller knew, used only when the row is already gone (a rerun after a partial
// failure must still be able to release claims that point at this id).
func (r *scyllaUserRepo) deleteAccountRows(ctx context.Context, userID int64, fallbackUsername, fallbackEmail string) error {
	username, email := fallbackUsername, fallbackEmail
	var rowUsername, rowEmail string
	q := r.session.Session.Query("SELECT username, email FROM users WHERE id = ?", userID).WithContext(ctx)
	err := q.Scan(&rowUsername, &rowEmail)
	q.Release()
	switch {
	case err == nil:
		username, email = rowUsername, rowEmail
	case errors.Is(err, gocql.ErrNotFound):
	default:
		return err
	}

	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	if key := usernameKey(username); key != "" {
		owner, err := r.claimOwner(ctx, "SELECT user_id FROM users_by_username WHERE username = ?", key)
		if err != nil {
			return err
		}
		if owner == userID {
			b.Query("DELETE FROM users_by_username WHERE username = ?", key)
		}
	}
	if email != "" {
		owner, err := r.claimOwner(ctx, "SELECT user_id FROM users_by_email WHERE email = ?", email)
		if err != nil {
			return err
		}
		if owner == userID {
			b.Query("DELETE FROM users_by_email WHERE email = ?", email)
		}
	}
	b.Query("DELETE FROM users WHERE id = ?", userID)
	return r.session.ExecuteBatch(b)
}

// claimOwner reads which account holds a users_by_username/users_by_email row (0: nobody).
func (r *scyllaUserRepo) claimOwner(ctx context.Context, stmt string, key string) (int64, error) {
	var owner int64
	q := r.session.Session.Query(stmt, key).WithContext(ctx)
	defer q.Release()
	if err := q.Scan(&owner); err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return owner, nil
}

// sweepZombie removes a users row that reappeared under a deleted id (see zombieSweepDelay).
// It reports whether there was one.
func (r *scyllaUserRepo) sweepZombie(ctx context.Context, userID int64) (bool, error) {
	var got int64
	q := r.session.Session.Query("SELECT id FROM users WHERE id = ?", userID).WithContext(ctx)
	err := q.Scan(&got)
	q.Release()
	if errors.Is(err, gocql.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, r.deleteAccountRows(ctx, userID, "", "")
}

// TombstoneShadow blanks a shadow whose home instance deleted the account: every profile
// field, presence and the relationship/space/block sets, and sets deleted_at. The row and
// its users_by_remote mapping stay, so messages naming it still resolve and a later
// reference to the same federated id finds the tombstone instead of fetching a profile.
// Conditional on the row existing (IF EXISTS) so it can never create one. Refuses a local
// account: those are deleted, never tombstoned.
func (r *scyllaUserRepo) TombstoneShadow(ctx context.Context, userID int64) error {
	u, err := r.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return nil
	}
	if !u.IsRemote() {
		return errors.New("tombstone: not a shadow")
	}
	now := time.Now().UTC()
	q := r.session.Session.Query(
		`UPDATE users SET username = '', display_name = '', avatar = '', banner = '', bio = '',
		 about_me = '', pronouns = '', accent_color = '', presence = ?, relationships = null,
		 spaces = null, blocks = null, flags = 0, deleted_at = ?, updated_at = ?
		 WHERE id = ? IF EXISTS`,
		UserPresence{Status: "offline"}, now, now, userID,
	).WithContext(ctx)
	defer q.Release()
	_, err = q.MapScanCAS(map[string]interface{}{})
	return err
}

// ---------------------------------------------------------------- cached wrapper ----

// DeleteAccount deletes the identity, then drops user:<id> and the user:name:<key> lookup
// entry - the one handle-keyed cache, which would otherwise answer by-name lookups (friend
// requests by handle, federation's /users/lookup) with the deleted account's id for up to
// the cache TTL after someone else registered the name. Dropped again 2 s later (a
// cache-aside read in flight can re-populate after the first pass, as in ChangeEmail) and
// once more after the zombie sweep.
func (r *CachedUserRepository) DeleteAccount(ctx context.Context, u *User) error {
	if err := r.repo.DeleteAccount(ctx, u); err != nil {
		return err
	}
	if u == nil {
		return nil
	}
	userID, username := u.ID, u.Username
	invalidate := func() { r.InvalidateUser(context.Background(), userID, username) }
	invalidate()
	redrop, sweep := cacheRedropDelay, zombieSweepDelay
	safego.Go("auth", func() {
		time.Sleep(redrop)
		invalidate()
		time.Sleep(sweep)
		invalidate()
	})
	return nil
}

func (r *CachedUserRepository) TombstoneShadow(ctx context.Context, userID int64) error {
	if err := r.repo.TombstoneShadow(ctx, userID); err != nil {
		return err
	}
	r.invalidateUser(context.Background(), userID)
	return nil
}

// ----------------------------------------------------------------------- sessions ----

// DeleteAllForUser hard-deletes every session row of a deleted account (both tables): they
// hold IP addresses and user agents. Revocation happened first (RevokeAllForUser); this
// also catches a session a login created after that, racing the deletion.
func (r *scyllaSessionRepo) DeleteAllForUser(ctx context.Context, userID int64) error {
	sessions, err := r.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.TokenHash == "" {
			continue
		}
		if err := r.session.Session.Query("DELETE FROM sessions_by_token WHERE token_hash = ?", s.TokenHash).WithContext(ctx).Exec(); err != nil {
			return err
		}
	}
	return r.session.Session.Query("DELETE FROM sessions_by_user WHERE user_id = ?", userID).WithContext(ctx).Exec()
}

func (r *CachedSessionRepository) DeleteAllForUser(ctx context.Context, userID int64) error {
	sessions, err := r.repo.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	if err := r.repo.DeleteAllForUser(ctx, userID); err != nil {
		return err
	}
	for _, s := range sessions {
		if s.TokenHash != "" {
			_ = r.redis.Del(ctx, r.prefix+s.TokenHash)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------- 2FA ----

// DeleteAllForUser drops a deleted account's recovery codes and passkeys (both partitions).
// The TOTP columns go with the users row.
func (r *scyllaTwoFactorRepo) DeleteAllForUser(ctx context.Context, userID int64) error {
	if err := r.session.Session.Query("DELETE FROM recovery_codes_by_user WHERE user_id = ?", userID).WithContext(ctx).Exec(); err != nil {
		return err
	}
	return r.session.Session.Query("DELETE FROM webauthn_credentials_by_user WHERE user_id = ?", userID).WithContext(ctx).Exec()
}

// --------------------------------------------------------------------------- Redis ----

// PurgeAccountRedisState drops a deleted account's per-user Redis state owned by this
// module: a pending email change (the record holds the new address) and the token it names,
// the email-change lock, re-auth counter and revival marker, and a half-finished passkey
// registration. prefix is RedisKeyPrefix(cfg). Token- and address-hash keyed entries
// (verify/reset links, cooldowns) are left to expire: redeeming one re-reads the account,
// which no longer exists.
func PurgeAccountRedisState(ctx context.Context, rdb *redis.Client, prefix string, userID int64) error {
	if rdb == nil {
		return nil
	}
	uid := strconv.FormatInt(userID, 10)
	keys := []string{
		emailChangePendingKey(prefix, userID),
		emailChangeLockKey(prefix, userID),
		emailChangedKey(prefix, userID),
		prefix + "email:change:reauth:" + uid,
		prefix + "webauthn:reg:" + uid,
	}
	if raw, err := rdb.Get(ctx, emailChangePendingKey(prefix, userID)).Bytes(); err == nil {
		var p pendingEmailChangeRecord
		if json.Unmarshal(raw, &p) == nil && p.TokenHash != "" {
			keys = append(keys, emailChangeTokenKey(prefix, p.TokenHash))
		}
	} else if !errors.Is(err, redis.Nil) {
		return err
	}
	return rdb.Del(ctx, keys...).Err()
}
