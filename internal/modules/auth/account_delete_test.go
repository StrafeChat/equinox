package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/redistest"
)

// The cached repository's side of account deletion, over an in-memory Redis
// (internal/redistest) and the in-memory users fake.

func newDeleteCache(t *testing.T) (*CachedUserRepository, *memUsers, *redistest.Redis) {
	t.Helper()
	rdb, mem := redistest.New()
	cfg := &config.Config{}
	cfg.Database.Redis.CacheEnabled = true
	cfg.Database.Redis.CachePrefix = "t"
	users := newMemUsers()
	repo, ok := NewCachedUserRepository(users, rdb, cfg).(*CachedUserRepository)
	if !ok {
		t.Fatal("cache not enabled")
	}
	return repo, users, mem
}

func TestCachedDeleteAccountDropsIDAndNameEntries(t *testing.T) {
	oldRedrop, oldSweep := cacheRedropDelay, zombieSweepDelay
	cacheRedropDelay, zombieSweepDelay = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { cacheRedropDelay, zombieSweepDelay = oldRedrop, oldSweep })

	repo, users, mem := newDeleteCache(t)
	ctx := context.Background()
	users.add(&User{ID: 7, Username: "Alice", Email: "alice@x.test"})
	// Fill both cache entries the way reads do.
	if u, err := repo.GetByID(ctx, 7); err != nil || u == nil {
		t.Fatalf("GetByID: %v %v", u, err)
	}
	if u, err := repo.GetByUsername(ctx, "alice"); err != nil || u == nil {
		t.Fatalf("GetByUsername: %v %v", u, err)
	}
	if !mem.HasString("t:user:7") || !mem.HasString("t:user:name:alice") {
		t.Fatalf("cache not filled: %v", mem.Strings())
	}

	if err := repo.DeleteAccount(ctx, &User{ID: 7, Username: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if mem.HasString("t:user:7") || mem.HasString("t:user:name:alice") {
		t.Fatalf("entries survived the delete: %v", mem.Strings())
	}
	if users.row(7) != nil {
		t.Fatal("row survived")
	}

	// A read in flight re-populates the entries after the first pass; the second pass
	// drops them again.
	stale, _ := json.Marshal(userToCached(&User{ID: 7, Username: "Alice"}))
	mem.SetString("t:user:7", string(stale))
	mem.SetString("t:user:name:alice", string(stale))
	deadline := time.Now().Add(2 * time.Second)
	for mem.HasString("t:user:7") || mem.HasString("t:user:name:alice") {
		if time.Now().After(deadline) {
			t.Fatalf("the delayed pass never dropped the entries: %v", mem.Strings())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A new account that claims a freed name never sees the old holder's cached lookup.
func TestCachedCreateDropsStaleNameEntry(t *testing.T) {
	repo, _, mem := newDeleteCache(t)
	ctx := context.Background()
	old, _ := json.Marshal(userToCached(&User{ID: 7, Username: "alice"}))
	mem.SetString("t:user:name:alice", string(old))

	if err := repo.Create(ctx, &User{ID: 8, Username: "ALICE", Email: "new@x.test"}); err != nil {
		t.Fatal(err)
	}
	if mem.HasString("t:user:name:alice") {
		t.Fatal("stale user:name: entry survived the claim")
	}
	u, err := repo.GetByUsername(ctx, "alice")
	if err != nil || u == nil || u.ID != 8 {
		t.Fatalf("lookup after the claim = %+v, %v; want the new account", u, err)
	}
}

// A user:name: hit naming someone else (a stale entry) is not trusted.
func TestCachedGetByUsernameIgnoresMismatchedEntry(t *testing.T) {
	repo, users, mem := newDeleteCache(t)
	ctx := context.Background()
	users.add(&User{ID: 9, Username: "bob", Email: "bob@x.test"})
	wrong, _ := json.Marshal(userToCached(&User{ID: 7, Username: "carol"}))
	mem.SetString("t:user:name:bob", string(wrong))

	u, err := repo.GetByUsername(ctx, "bob")
	if err != nil || u == nil || u.ID != 9 {
		t.Fatalf("GetByUsername = %+v, %v; want bob from the repository", u, err)
	}
}

func TestCachedUserRoundTripsDeletedAt(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	rid := int64(55)
	in := &User{ID: 3, HomeDomain: "b.test", RemoteID: &rid, DeletedAt: &at}
	raw, err := json.Marshal(userToCached(in))
	if err != nil {
		t.Fatal(err)
	}
	var c cachedUser
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	out := cachedToUser(&c, "")
	if out.DeletedAt == nil || !out.DeletedAt.Equal(at) || !out.IsDeleted() {
		t.Fatalf("DeletedAt lost: %+v", out.DeletedAt)
	}
	live := cachedToUser(userToCached(&User{ID: 4}), "")
	if live.DeletedAt != nil || live.IsDeleted() {
		t.Fatal("a live account came back deleted")
	}
	var nilUser *User
	if nilUser.IsDeleted() {
		t.Fatal("nil user reported deleted")
	}
}

func TestPurgeAccountRedisState(t *testing.T) {
	rdb, mem := redistest.New()
	ctx := context.Background()
	prefix := "t:"
	rec, _ := json.Marshal(pendingEmailChangeRecord{OldEmail: "a@x.test", NewEmail: "b@x.test", TokenHash: "abc"})
	mem.SetString(emailChangePendingKey(prefix, 7), string(rec))
	mem.SetString(emailChangeTokenKey(prefix, "abc"), "7")
	mem.SetString(emailChangeLockKey(prefix, 7), "1")
	mem.SetString(emailChangedKey(prefix, 7), "1")
	mem.SetString(prefix+"email:change:reauth:7", "2")
	mem.SetString(prefix+"webauthn:reg:7", "{}")
	// Someone else's state stays.
	mem.SetString(emailChangePendingKey(prefix, 8), "{}")
	mem.SetString(prefix+"webauthn:reg:8", "{}")

	if err := PurgeAccountRedisState(ctx, rdb, prefix, 7); err != nil {
		t.Fatal(err)
	}
	got := mem.Strings()
	want := []string{emailChangePendingKey(prefix, 8), prefix + "webauthn:reg:8"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("keys left %v, want %v", got, want)
	}
	// Nothing pending: still fine, and idempotent.
	if err := PurgeAccountRedisState(ctx, rdb, prefix, 7); err != nil {
		t.Fatal(err)
	}
	if err := PurgeAccountRedisState(ctx, nil, prefix, 7); err != nil {
		t.Fatal(err)
	}
}
