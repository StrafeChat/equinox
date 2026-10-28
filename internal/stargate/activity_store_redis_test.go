package stargate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// The Redis store - its Lua script, expiries, index and prefix - needs a real Redis. These
// run when STARGATE_TEST_REDIS names one, e.g. redis://localhost:6379/15: a database the
// test may treat as its own (it refuses one holding anything but its own keys).
const activityTestPrefix = "sgtest:"

func activityTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("STARGATE_TEST_REDIS")
	if raw == "" {
		t.Skip("set STARGATE_TEST_REDIS=redis://host:port/db to run the Redis-backed activity tests")
	}
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "*").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, activityTestPrefix) {
			t.Skipf("database holds %q - use an empty one", k)
		}
	}
	clean := func() {
		if ks, _ := rdb.Keys(ctx, activityTestPrefix+"*").Result(); len(ks) > 0 {
			rdb.Del(ctx, ks...)
		}
	}
	clean()
	t.Cleanup(func() { clean(); _ = rdb.Close() })
	return rdb
}

func TestRedisActivityStore(t *testing.T) {
	rdb := activityTestRedis(t)
	ctx := context.Background()
	s := NewActivityStore(rdb, activityTestPrefix)
	mc := auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: 100}
	fac := auth.Activity{Type: "playing", Name: "Factorio", StartedAt: 200}

	mustChange := func(what string, want bool, changed bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if changed != want {
			t.Fatalf("%s: changed = %v, want %v", what, changed, want)
		}
	}
	eff := func(userID int64) string {
		t.Helper()
		got, err := s.Effective(ctx, []int64{userID})
		if err != nil {
			t.Fatal(err)
		}
		if len(got[userID]) == 0 {
			return ""
		}
		return got[userID][0].Name
	}

	c, err := s.SetConn(ctx, 1, 11, mc, 1000)
	mustChange("first report", true, c, err)
	c, err = s.SetConn(ctx, 1, 11, mc, 2000)
	mustChange("same report later", false, c, err)
	c, err = s.SetConn(ctx, 1, 12, fac, 3000)
	mustChange("a newer connection's game", true, c, err)
	if eff(1) != "Factorio" {
		t.Fatalf("effective %q, want the latest report", eff(1))
	}
	c, err = s.ClearConn(ctx, 1, 12)
	mustChange("clearing the latest", true, c, err)
	if eff(1) != "Minecraft" {
		t.Fatalf("effective %q after the clear", eff(1))
	}

	// Keys carry the prefix; the hash expires; the index names the user.
	if ttl := rdb.TTL(ctx, activityTestPrefix+"presence:activity:1").Val(); ttl <= 0 || ttl > activityTTL {
		t.Errorf("connection hash TTL %v", ttl)
	}
	if !rdb.SIsMember(ctx, activityTestPrefix+"presence:activity:users", "1").Val() {
		t.Error("user 1 missing from the index")
	}

	// A keepalive writes the connection's entry again as it was: no change, and the
	// expiry renewed - a game can outlast it.
	connKey := activityTestPrefix + "presence:activity:1"
	rdb.Expire(ctx, connKey, time.Minute)
	c, err = s.SetConn(ctx, 1, 11, mc, 2000)
	mustChange("a keepalive", false, c, err)
	if ttl := rdb.TTL(ctx, connKey).Val(); ttl <= time.Hour {
		t.Errorf("after a keepalive the hash expires in %v", ttl)
	}
	// If Redis lost the entry, the keepalive puts it back, and that is a change.
	rdb.Del(ctx, connKey)
	c, err = s.SetConn(ctx, 1, 11, mc, 2000)
	mustChange("a keepalive after a loss", true, c, err)
	if eff(1) != "Minecraft" {
		t.Fatalf("effective %q after the keepalive", eff(1))
	}

	// A remote user's relayed activity: its own key, with an expiry, newer than the rest.
	c, err = s.SetFederated(ctx, 1, &fac, 4000)
	mustChange("a newer federated entry", true, c, err)
	if ttl := rdb.TTL(ctx, activityTestPrefix+"presence:activity:fed:1").Val(); ttl <= 0 || ttl > activityTTL {
		t.Errorf("federated key TTL %v", ttl)
	}
	_, _ = s.SetConn(ctx, 2, 21, mc, 1000)
	got, err := s.Effective(ctx, []int64{1, 2, 3, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1][0].Name != "Factorio" || got[2][0].Name != "Minecraft" {
		t.Fatalf("batch read %+v", got)
	}

	// A gateway start empties the connections, not the federated entries.
	if err := s.PurgeConnections(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"presence:activity:1", "presence:activity:2", "presence:activity:users"} {
		if rdb.Exists(ctx, activityTestPrefix+k).Val() != 0 {
			t.Errorf("%s survived the purge", k)
		}
	}
	if eff(1) != "Factorio" || eff(2) != "" {
		t.Errorf("after the purge: user 1 %q, user 2 %q", eff(1), eff(2))
	}
	c, err = s.SetFederated(ctx, 1, nil, 0)
	mustChange("clearing the federated entry", true, c, err)
	if eff(1) != "" {
		t.Errorf("after every clear %q is shown", eff(1))
	}

	// The last connection's clear drops the user from the index.
	_, _ = s.SetConn(ctx, 3, 31, mc, time.Now().UnixMilli())
	_, _ = s.ClearConn(ctx, 3, 31)
	if rdb.SIsMember(ctx, activityTestPrefix+"presence:activity:users", "3").Val() {
		t.Error("an empty hash left its user in the index")
	}
}
