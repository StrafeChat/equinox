package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
)

// DeleteAccount, the zombie sweep and TombstoneShadow against a real Scylla. Runs when
// AUTH_TEST_SCYLLA names a node, e.g. AUTH_TEST_SCYLLA=127.0.0.1:9042. Like
// repo_change_email_scylla_test.go, it creates its own keyspace
// (equinox_test_delete_<unixnano>) holding only the tables these touch, drops it afterwards,
// and refuses to run if that keyspace already exists or is SCYLLA_KEYSPACE. It never touches
// an application keyspace and never runs migrations.

func scyllaDeleteAccountRepo(t *testing.T) (*scyllaUserRepo, *gocql.Session) {
	t.Helper()
	addr := os.Getenv("AUTH_TEST_SCYLLA")
	if addr == "" {
		t.Skip("set AUTH_TEST_SCYLLA=host:port to run the Scylla-backed account deletion tests")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("AUTH_TEST_SCYLLA: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("AUTH_TEST_SCYLLA port: %v", err)
	}
	keyspace := fmt.Sprintf("equinox_test_delete_%d", time.Now().UnixNano())
	if keyspace == os.Getenv("SCYLLA_KEYSPACE") || keyspace == "strafechattest" {
		t.Fatalf("refusing to use keyspace %q", keyspace)
	}

	cluster := gocql.NewCluster(host)
	cluster.Port = port
	cluster.Timeout = 10 * time.Second
	cluster.ConnectTimeout = 10 * time.Second
	cluster.Consistency = gocql.Quorum
	admin, err := cluster.CreateSession()
	if err != nil {
		t.Fatalf("scylla: %v", err)
	}
	var existing string
	if err := admin.Query("SELECT keyspace_name FROM system_schema.keyspaces WHERE keyspace_name = ?", keyspace).Scan(&existing); err == nil {
		admin.Close()
		t.Fatalf("keyspace %q already exists; refusing to touch it", keyspace)
	} else if !errors.Is(err, gocql.ErrNotFound) {
		admin.Close()
		t.Fatal(err)
	}
	create := "CREATE KEYSPACE " + keyspace + " WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}"
	if err := admin.Query(create + " AND tablets = {'enabled': false}").Exec(); err != nil {
		if err2 := admin.Query(create).Exec(); err2 != nil {
			admin.Close()
			t.Fatalf("create keyspace: %v / %v", err, err2)
		}
	}
	t.Cleanup(func() {
		if err := admin.Query("DROP KEYSPACE IF EXISTS " + keyspace).Exec(); err != nil {
			t.Errorf("drop keyspace %s: %v", keyspace, err)
		}
		admin.Close()
	})
	for _, stmt := range []string{
		"CREATE TYPE " + keyspace + ".user_presence (online boolean, status text, custom_status text)",
		"CREATE TABLE " + keyspace + `.users (
			id bigint PRIMARY KEY, email text, password text, username text, display_name text,
			avatar text, banner text, bot boolean, bots set<text>, system boolean, bio text,
			pronouns text, birthday_opt_in boolean, flags int, relationships set<bigint>,
			spaces set<bigint>, blocks set<bigint>, date_of_birth timestamp, verified_email boolean,
			about_me text, accent_color text, locale text, presence frozen<user_presence>,
			created_at timestamp, updated_at timestamp, home_domain text, remote_id bigint,
			totp_enabled boolean, totp_secret text, deleted_at timestamp)`,
		"CREATE TABLE " + keyspace + ".users_by_email (email text PRIMARY KEY, user_id bigint)",
		"CREATE TABLE " + keyspace + ".users_by_username (username text PRIMARY KEY, user_id bigint)",
		"CREATE TABLE " + keyspace + ".users_by_remote (home_domain text, remote_id bigint, user_id bigint, PRIMARY KEY (home_domain, remote_id))",
	} {
		if err := admin.Query(stmt).Exec(); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	kc := gocql.NewCluster(host)
	kc.Port = port
	kc.Keyspace = keyspace
	kc.Timeout = 10 * time.Second
	kc.ConnectTimeout = 10 * time.Second
	kc.Consistency = gocql.Quorum
	sess, err := gocqlx.WrapSession(kc.CreateSession())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	return &scyllaUserRepo{session: sess}, sess.Session
}

func claimHolder(t *testing.T, s *gocql.Session, table, key string) int64 {
	t.Helper()
	var id int64
	err := s.Query("SELECT user_id FROM "+table+" WHERE "+map[string]string{"users_by_email": "email", "users_by_username": "username"}[table]+" = ?", key).Scan(&id)
	if errors.Is(err, gocql.ErrNotFound) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func rowExists(t *testing.T, s *gocql.Session, id int64) bool {
	t.Helper()
	var got int64
	err := s.Query("SELECT id FROM users WHERE id = ?", id).Scan(&got)
	if errors.Is(err, gocql.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func TestDeleteAccountScyllaFreesNameAndAddress(t *testing.T) {
	repo, s := scyllaDeleteAccountRepo(t)
	ctx := context.Background()
	// The sweep DeleteAccount schedules must not fire into a dropped keyspace mid-test.
	old := zombieSweepDelay
	zombieSweepDelay = time.Hour
	t.Cleanup(func() { zombieSweepDelay = old })

	alice := &User{ID: 1001, Username: "Alice", Email: "alice@x.test", Presence: UserPresence{Status: "online"}}
	if err := repo.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	// Delete through a stale copy with the wrong name: the row's own name is what is freed.
	if err := repo.DeleteAccount(ctx, &User{ID: 1001, Username: "someone-else", Email: "x@x.test"}); err != nil {
		t.Fatal(err)
	}
	if rowExists(t, s, 1001) {
		t.Fatal("users row survived")
	}
	if h := claimHolder(t, s, "users_by_username", "alice"); h != 0 {
		t.Fatalf("username claim still held by %d", h)
	}
	if h := claimHolder(t, s, "users_by_email", "alice@x.test"); h != 0 {
		t.Fatalf("email claim still held by %d", h)
	}
	// Idempotent.
	if err := repo.deleteAccountRows(ctx, 1001, "Alice", "alice@x.test"); err != nil {
		t.Fatalf("rerun: %v", err)
	}

	// The name and the address are free at once.
	again := &User{ID: 1002, Username: "alice", Email: "alice@x.test", Presence: UserPresence{Status: "online"}}
	if err := repo.Create(ctx, again); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if u, err := repo.GetByUsername(ctx, "ALICE"); err != nil || u == nil || u.ID != 1002 {
		t.Fatalf("GetByUsername = %+v, %v; want 1002", u, err)
	}

	// A rerun for the old id never releases claims that are someone else's now.
	if err := repo.deleteAccountRows(ctx, 1001, "Alice", "alice@x.test"); err != nil {
		t.Fatal(err)
	}
	if claimHolder(t, s, "users_by_username", "alice") != 1002 || claimHolder(t, s, "users_by_email", "alice@x.test") != 1002 {
		t.Fatal("a rerun for the deleted id released the new account's claims")
	}

	// The zombie hazard: an UPDATE on a deleted id recreates a partial row (Cassandra
	// upserts); the sweep removes it without touching the new holder's claims.
	if err := s.Query("UPDATE users SET relationships = relationships + ? WHERE id = ?", []int64{5}, int64(1001)).Exec(); err != nil {
		t.Fatal(err)
	}
	if !rowExists(t, s, 1001) {
		t.Fatal("expected the update to recreate a row (the hazard this guards against)")
	}
	removed, err := repo.sweepZombie(ctx, 1001)
	if err != nil || !removed {
		t.Fatalf("sweep = %v, %v", removed, err)
	}
	if rowExists(t, s, 1001) {
		t.Fatal("zombie row survived the sweep")
	}
	if claimHolder(t, s, "users_by_username", "alice") != 1002 || claimHolder(t, s, "users_by_email", "alice@x.test") != 1002 {
		t.Fatal("the sweep released the new account's claims")
	}
	if removed, err := repo.sweepZombie(ctx, 1001); err != nil || removed {
		t.Fatalf("second sweep = %v, %v; want nothing to do", removed, err)
	}
}

func TestTombstoneShadowScylla(t *testing.T) {
	repo, s := scyllaDeleteAccountRepo(t)
	ctx := context.Background()
	rid := int64(77)
	shadow := &User{
		ID: 2001, Username: "far", DisplayName: "Far Away", Avatar: "https://b.test/a.png", Bio: "hi",
		HomeDomain: "b.test", RemoteID: &rid, Relationships: []int64{1002}, Spaces: []int64{9},
		Presence: UserPresence{Status: "online"},
	}
	if err := repo.UpsertShadow(ctx, shadow); err != nil {
		t.Fatal(err)
	}
	if err := repo.TombstoneShadow(ctx, 2001); err != nil {
		t.Fatal(err)
	}
	u, err := repo.GetByID(ctx, 2001)
	if err != nil || u == nil {
		t.Fatalf("tombstone row: %+v %v", u, err)
	}
	if !u.IsDeleted() || u.Username != "" || u.DisplayName != "" || u.Avatar != "" || u.Bio != "" ||
		len(u.Relationships) != 0 || len(u.Spaces) != 0 || u.Presence.Status != "offline" {
		t.Fatalf("tombstone not blanked: %+v", u)
	}
	if !u.IsRemote() || u.OriginID() != 77 {
		t.Fatalf("tombstone lost its federated identity: %+v", u)
	}
	if got, err := repo.GetByRemote(ctx, "b.test", 77); err != nil || got == nil || got.ID != 2001 {
		t.Fatalf("users_by_remote = %+v, %v; want 2001", got, err)
	}

	// Never a local account, never a row that is not there.
	local := &User{ID: 2002, Username: "here", Email: "here@x.test", Presence: UserPresence{Status: "online"}}
	if err := repo.Create(ctx, local); err != nil {
		t.Fatal(err)
	}
	if err := repo.TombstoneShadow(ctx, 2002); err == nil {
		t.Fatal("a local account was tombstoned")
	}
	if err := repo.TombstoneShadow(ctx, 2003); err != nil {
		t.Fatal(err)
	}
	if rowExists(t, s, 2003) {
		t.Fatal("tombstoning a missing id created a row")
	}
}
