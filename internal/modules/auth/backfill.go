package auth

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
)

// Data migration that retires the username#discriminator tag (claimed in data_migrations).
const dataMigrationUniqueUsernames = "043_unique_usernames"

// UsernameMigrator is the one-off move from username#discriminator tags to usernames that
// are unique on their own. Implemented by the Scylla user repository; run once at startup.
type UsernameMigrator interface {
	// BackfillUniqueUsernames fills users_by_username (migration 043 creates it), renames the
	// accounts that shared a name and drops the discriminator column. onRenamed is told about
	// every rename so caches can forget the old row.
	BackfillUniqueUsernames(ctx context.Context, onRenamed func(userID int64, oldUsername string)) error
}

type migrationUser struct {
	id            int64
	username      string
	discriminator int
	remote        bool
}

// BackfillUniqueUsernames makes every local username unique (case-insensitively) and claims
// it in users_by_username. Where several accounts shared a name the oldest keeps it and the
// others are renamed to the name followed by their old discriminator - "bob#4821" becomes
// "bob4821", the tag they always typed, so nothing about them is new to their friends. Then
// users_by_username_discriminator and the discriminator column are dropped. Exactly once per
// keyspace (claimed in data_migrations); on any error the claim is released so the next start
// retries, and every step is idempotent: a username row already held by the same user counts
// as claimed, an account already renamed is simply no longer a duplicate.
func (r *scyllaUserRepo) BackfillUniqueUsernames(ctx context.Context, onRenamed func(userID int64, oldUsername string)) error {
	claimed, err := r.ClaimDataMigration(ctx, dataMigrationUniqueUsernames)
	if err != nil || !claimed {
		return err
	}
	renamed, err := r.backfillUniqueUsernames(ctx, onRenamed)
	if err != nil {
		if relErr := r.ReleaseDataMigration(ctx, dataMigrationUniqueUsernames); relErr != nil {
			logger.Err("auth", relErr, map[string]any{"data_migration": dataMigrationUniqueUsernames})
		}
		return err
	}
	logger.Info("auth", "usernames made unique: %d account(s) renamed, discriminators dropped", renamed)
	return nil
}

func (r *scyllaUserRepo) backfillUniqueUsernames(ctx context.Context, onRenamed func(userID int64, oldUsername string)) (int, error) {
	hasDisc, err := r.hasDiscriminatorColumn(ctx)
	if err != nil {
		return 0, err
	}
	users, err := r.listUsersForMigration(ctx, hasDisc)
	if err != nil {
		return 0, err
	}
	// Oldest first: snowflakes are time-ordered, so the first account with a name keeps it.
	sort.Slice(users, func(i, j int) bool { return users[i].id < users[j].id })
	taken := make(map[string]int64, len(users))
	renamed := 0
	now := time.Now().UTC()
	for _, u := range users {
		if u.remote || u.username == "" {
			continue // shadows of users on other instances are never in the lookup
		}
		name := u.username
		for attempt := 0; ; attempt++ {
			if owner, dup := taken[usernameKey(name)]; dup && owner != u.id {
				name = pickRename(u, taken)
			}
			ok, err := r.claimUsernameFor(ctx, name, u.id)
			if err != nil {
				return renamed, err
			}
			if ok {
				break
			}
			// Held in the table by an account this pass has not reached (a row left by an
			// earlier, interrupted run): treat the name as taken and pick again.
			taken[usernameKey(name)] = -1
			if attempt > 100 {
				return renamed, fmt.Errorf("no free username found for user %d", u.id)
			}
		}
		taken[usernameKey(name)] = u.id
		if name != u.username {
			if err := r.session.Session.Query("UPDATE users SET username = ?, updated_at = ? WHERE id = ?", name, now, u.id).WithContext(ctx).Exec(); err != nil {
				return renamed, err
			}
			renamed++
			if onRenamed != nil {
				onRenamed(u.id, u.username)
			}
		}
	}
	if err := r.session.Session.Query("DROP TABLE IF EXISTS users_by_username_discriminator").WithContext(ctx).Exec(); err != nil {
		return renamed, err
	}
	if hasDisc {
		if err := r.session.Session.Query("ALTER TABLE users DROP discriminator").WithContext(ctx).Exec(); err != nil {
			return renamed, err
		}
	}
	return renamed, nil
}

// pickRename finds a free spelling for a duplicate: the name with the old discriminator
// ("bob4821"), else a counter ("bob2", "bob3", ...), always within the 32-character limit.
func pickRename(u migrationUser, taken map[string]int64) string {
	try := func(suffix string) (string, bool) {
		base := u.username
		if n := 32 - len(suffix); len(base) > n {
			base = base[:n]
		}
		cand := base + suffix
		_, dup := taken[usernameKey(cand)]
		return cand, !dup
	}
	if u.discriminator > 0 {
		if cand, ok := try(fmt.Sprintf("%04d", u.discriminator)); ok {
			return cand
		}
	}
	for i := 2; ; i++ {
		if cand, ok := try(fmt.Sprint(i)); ok {
			return cand
		}
	}
}

// hasDiscriminatorColumn reports whether users still has the column, by asking for it: the
// repository does not know its keyspace name, so system_schema cannot be consulted directly.
func (r *scyllaUserRepo) hasDiscriminatorColumn(ctx context.Context) (bool, error) {
	err := r.session.Session.Query("SELECT discriminator FROM users LIMIT 1").WithContext(ctx).Exec()
	if err == nil {
		return true, nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "undefined") || strings.Contains(msg, "unknown") {
		return false, nil
	}
	return false, err
}

func (r *scyllaUserRepo) listUsersForMigration(ctx context.Context, withDisc bool) ([]migrationUser, error) {
	cols := "id, username, home_domain"
	if withDisc {
		cols += ", discriminator"
	}
	iter := r.session.Session.Query("SELECT " + cols + " FROM users").WithContext(ctx).Iter()
	var out []migrationUser
	for {
		var u migrationUser
		var home string
		var disc int
		dest := []interface{}{&u.id, &u.username, &home}
		if withDisc {
			dest = append(dest, &disc)
		}
		if !iter.Scan(dest...) {
			break
		}
		u.discriminator = disc
		u.remote = home != ""
		out = append(out, u)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// claimUsernameFor claims name for userID in users_by_username. A row already held by the
// same user counts as claimed (re-runs); one held by another user does not.
func (r *scyllaUserRepo) claimUsernameFor(ctx context.Context, name string, userID int64) (bool, error) {
	prev := map[string]interface{}{}
	q := r.session.Session.Query("INSERT INTO users_by_username (username, user_id) VALUES (?, ?) IF NOT EXISTS", usernameKey(name), userID).WithContext(ctx)
	defer q.Release()
	applied, err := q.MapScanCAS(prev)
	if err != nil {
		return false, err
	}
	if applied {
		return true, nil
	}
	owner, _ := prev["user_id"].(int64)
	return owner == userID, nil
}

// ClaimDataMigration records name in data_migrations if nobody has; false means another
// replica (or an earlier start) already ran it.
func (r *scyllaUserRepo) ClaimDataMigration(ctx context.Context, name string) (bool, error) {
	q := r.session.Session.Query("INSERT INTO data_migrations (name, applied_at) VALUES (?, ?) IF NOT EXISTS", name, time.Now().UTC()).WithContext(ctx)
	defer q.Release()
	return q.MapScanCAS(map[string]interface{}{})
}

// ReleaseDataMigration forgets a claim so the migration runs again on the next start.
func (r *scyllaUserRepo) ReleaseDataMigration(ctx context.Context, name string) error {
	return r.session.Session.Query("DELETE FROM data_migrations WHERE name = ?", name).WithContext(ctx).Exec()
}
