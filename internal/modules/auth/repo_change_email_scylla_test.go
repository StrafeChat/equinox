package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
)

// ChangeEmail's LWT sequence against a real Scylla. Runs when AUTH_TEST_SCYLLA names a node,
// e.g. AUTH_TEST_SCYLLA=127.0.0.1:9042. The test creates its own keyspace
// (equinox_test_email_<unixnano>) holding only the two tables ChangeEmail touches, drops it
// afterwards, and refuses to run if that keyspace already exists or is SCYLLA_KEYSPACE. It
// never touches an application keyspace and never runs migrations.

func scyllaChangeEmailRepo(t *testing.T) (*scyllaUserRepo, *gocql.Session) {
	t.Helper()
	addr := os.Getenv("AUTH_TEST_SCYLLA")
	if addr == "" {
		t.Skip("set AUTH_TEST_SCYLLA=host:port to run the Scylla-backed ChangeEmail tests")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("AUTH_TEST_SCYLLA: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("AUTH_TEST_SCYLLA port: %v", err)
	}
	keyspace := fmt.Sprintf("equinox_test_email_%d", time.Now().UnixNano())
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
	// LWTs need vnodes on Scylla versions that default new keyspaces to tablets; older
	// versions reject the tablets option, so fall back to the plain statement.
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
		"CREATE TABLE " + keyspace + ".users (id bigint PRIMARY KEY, email text, verified_email boolean, updated_at timestamp, username text)",
		"CREATE TABLE " + keyspace + ".users_by_email (email text PRIMARY KEY, user_id bigint)",
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

func seedEmailUser(t *testing.T, s *gocql.Session, id int64, email string) {
	t.Helper()
	if err := s.Query("INSERT INTO users (id, email, verified_email, username) VALUES (?, ?, false, ?)", id, email, "u"+strconv.FormatInt(id, 10)).Exec(); err != nil {
		t.Fatal(err)
	}
	if email != "" {
		if err := s.Query("INSERT INTO users_by_email (email, user_id) VALUES (?, ?)", email, id).Exec(); err != nil {
			t.Fatal(err)
		}
	}
}

func rowEmail(t *testing.T, s *gocql.Session, id int64) (string, bool) {
	t.Helper()
	var email string
	var verified bool
	if err := s.Query("SELECT email, verified_email FROM users WHERE id = ?", id).Scan(&email, &verified); err != nil {
		t.Fatal(err)
	}
	return email, verified
}

// indexOwner is who users_by_email says holds email, 0 when nobody.
func indexOwner(t *testing.T, s *gocql.Session, email string) int64 {
	t.Helper()
	var id int64
	err := s.Query("SELECT user_id FROM users_by_email WHERE email = ?", email).Scan(&id)
	if errors.Is(err, gocql.ErrNotFound) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestChangeEmailScyllaRace(t *testing.T) {
	repo, s := scyllaChangeEmailRepo(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		uid := int64(1000 + i)
		a, b, c := fmt.Sprintf("a%d@x.test", i), fmt.Sprintf("b%d@x.test", i), fmt.Sprintf("c%d@x.test", i)
		seedEmailUser(t, s, uid, a)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, target := range []string{b, c} {
			wg.Add(1)
			go func(j int, target string) {
				defer wg.Done()
				errs[j] = repo.ChangeEmail(ctx, uid, a, target, true)
			}(j, target)
		}
		wg.Wait()
		winner, loser := "", ""
		switch {
		case errs[0] == nil && errs[1] != nil:
			winner, loser = b, c
		case errs[1] == nil && errs[0] != nil:
			winner, loser = c, b
		default:
			t.Fatalf("iteration %d: errors %v / %v, want exactly one nil", i, errs[0], errs[1])
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, ErrEmailChangeConflict) && !errors.Is(err, ErrEmailInUse) {
				t.Fatalf("iteration %d: loser error %v", i, err)
			}
		}
		if got, _ := rowEmail(t, s, uid); got != winner {
			t.Fatalf("iteration %d: row holds %q, want the winner %q", i, got, winner)
		}
		if indexOwner(t, s, winner) != uid {
			t.Fatalf("iteration %d: the winner's index row is missing", i)
		}
		if owner := indexOwner(t, s, loser); owner != 0 {
			t.Fatalf("iteration %d: the loser's claim was left behind (orphan -> %d)", i, owner)
		}
		if owner := indexOwner(t, s, a); owner != 0 {
			t.Fatalf("iteration %d: the old address was not released", i)
		}
	}
}

func TestChangeEmailScyllaConditionalRelease(t *testing.T) {
	repo, s := scyllaChangeEmailRepo(t)
	ctx := context.Background()
	seedEmailUser(t, s, 1, "a@x.test")
	seedEmailUser(t, s, 2, "two@x.test")
	// The old address's index row now belongs to user 2 (a hijack, or a stale read).
	if err := s.Query("INSERT INTO users_by_email (email, user_id) VALUES (?, ?)", "a@x.test", int64(2)).Exec(); err != nil {
		t.Fatal(err)
	}
	if err := repo.ChangeEmail(ctx, 1, "a@x.test", "c@x.test", false); err != nil {
		t.Fatal(err)
	}
	if owner := indexOwner(t, s, "a@x.test"); owner != 2 {
		t.Errorf("a@x.test -> %d; the release must not delete another account's row", owner)
	}
	if indexOwner(t, s, "c@x.test") != 1 {
		t.Error("c@x.test must be claimed by user 1")
	}
}

func TestChangeEmailScyllaStaleOld(t *testing.T) {
	repo, s := scyllaChangeEmailRepo(t)
	ctx := context.Background()
	seedEmailUser(t, s, 1, "a@x.test")
	if err := repo.ChangeEmail(ctx, 1, "a@x.test", "b@x.test", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Query("INSERT INTO users_by_email (email, user_id) VALUES (?, ?) IF NOT EXISTS", "a@x.test", int64(2)).Exec(); err != nil {
		t.Fatal(err)
	}
	if err := repo.ChangeEmail(ctx, 1, "a@x.test", "c@x.test", true); !errors.Is(err, ErrEmailChangeConflict) {
		t.Fatalf("stale old address: %v, want ErrEmailChangeConflict", err)
	}
	if indexOwner(t, s, "a@x.test") != 2 {
		t.Error("a@x.test must still point at user 2")
	}
	if owner := indexOwner(t, s, "c@x.test"); owner != 0 {
		t.Errorf("c@x.test must have been released, points at %d", owner)
	}
	if got, _ := rowEmail(t, s, 1); got != "b@x.test" {
		t.Errorf("row holds %q, want b@x.test", got)
	}
}

func TestChangeEmailScyllaTakenAndTogether(t *testing.T) {
	repo, s := scyllaChangeEmailRepo(t)
	ctx := context.Background()
	seedEmailUser(t, s, 1, "a@x.test")
	seedEmailUser(t, s, 3, "x@x.test")

	if err := repo.ChangeEmail(ctx, 1, "a@x.test", "x@x.test", true); !errors.Is(err, ErrEmailInUse) {
		t.Fatalf("taken: %v, want ErrEmailInUse", err)
	}
	if got, _ := rowEmail(t, s, 1); got != "a@x.test" || indexOwner(t, s, "a@x.test") != 1 || indexOwner(t, s, "x@x.test") != 3 {
		t.Error("a refused change must change nothing")
	}
	if err := repo.ChangeEmail(ctx, 1, "a@x.test", "A@X.test", true); !errors.Is(err, ErrEmailUnchanged) {
		t.Errorf("case-only difference: %v, want ErrEmailUnchanged", err)
	}

	// verified_email is written in the same statement as the address, both ways.
	if err := repo.ChangeEmail(ctx, 1, "a@x.test", "v@x.test", true); err != nil {
		t.Fatal(err)
	}
	if got, verified := rowEmail(t, s, 1); got != "v@x.test" || !verified {
		t.Errorf("after a verified change: %q verified=%v", got, verified)
	}
	if err := repo.ChangeEmail(ctx, 1, "v@x.test", "u@x.test", false); err != nil {
		t.Fatal(err)
	}
	if got, verified := rowEmail(t, s, 1); got != "u@x.test" || verified {
		t.Errorf("after an unverified change: %q verified=%v", got, verified)
	}
	if indexOwner(t, s, "v@x.test") != 0 {
		t.Error("the intermediate address must be released")
	}
}
