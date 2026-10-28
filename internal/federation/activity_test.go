package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// Activities across instances: a user's home instance sends what they are playing with
// their presence (POST /users/presence), and that one path is the only one that may set
// or clear a remote user's activity on the receiving side. Snapshots, member pages and
// joins carry status only - they describe members of any instance, and member relays go
// through the durable outbox.

func useActivities(t *testing.T) *stargate.MemoryActivityStore {
	t.Helper()
	store := stargate.NewMemoryActivityStore()
	prev := stargate.CurrentActivityStore()
	stargate.SetActivityStore(store)
	t.Cleanup(func() { stargate.SetActivityStore(prev) })
	return store
}

func playingNow(name string) auth.Activity {
	return auth.Activity{Type: "playing", Name: name, StartedAt: time.Now().Add(-time.Hour).UnixMilli()}
}

func effectiveActivity(t *testing.T, userID int64) string {
	t.Helper()
	acts := stargate.ActivitiesOf(context.Background(), userID)
	if len(acts) == 0 {
		return ""
	}
	return acts[0].Name
}

// ---- outbound ------------------------------------------------------------------------------

func TestPresenceEventCarriesActivities(t *testing.T) {
	ctx := context.Background()
	store := useActivities(t)
	_, _ = store.SetConn(ctx, 7, 70, playingNow("Minecraft"), time.Now().UnixMilli())
	s := &Service{fcfg: config.FederationConfig{Domain: "a.example"}}

	u := &auth.User{ID: 7, Username: "gamer", Presence: auth.UserPresence{Online: true, Status: "dnd"}}
	raw, _ := json.Marshal(s.presenceEvent(ctx, u))
	var ev PresenceEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.User != "@7:a.example" || ev.Presence.Status != "dnd" || len(ev.Presence.Activities) != 1 || ev.Presence.Activities[0].Name != "Minecraft" {
		t.Errorf("online: %s", raw)
	}

	for _, p := range []auth.UserPresence{{Online: true, Status: "invisible"}, {Online: false, Status: "online"}} {
		u.Presence = p
		raw, _ := json.Marshal(s.presenceEvent(ctx, u))
		if strings.Contains(string(raw), "activities") || !strings.Contains(string(raw), `"status":"offline"`) {
			t.Errorf("%+v went out as %s", p, raw)
		}
	}
}

// The member wire never carries an activity, whatever the store holds.
func TestMemberWireCarriesNoActivities(t *testing.T) {
	ctx := context.Background()
	store := useActivities(t)
	origin := &Service{fcfg: config.FederationConfig{Domain: "a.example"}}
	local := &auth.User{ID: 4, Username: "local", Presence: auth.UserPresence{Online: true, Status: "online"}}
	shadow := shadowUser(5, 50, "b.example")
	shadow.Presence = auth.UserPresence{Online: true, Status: "online"}
	_, _ = store.SetConn(ctx, local.ID, 40, playingNow("Minecraft"), time.Now().UnixMilli())
	_, _ = store.SetFederated(ctx, shadow.ID, &auth.Activity{Type: "playing", Name: "Factorio", StartedAt: 1}, time.Now().UnixMilli())

	for _, u := range []*auth.User{local, shadow} {
		row := &spaces.SpaceMember{SpaceID: 100, UserID: u.ID, JoinedAt: time.Now()}
		raw, err := json.Marshal(origin.memberWire(row, u))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "activities") || !strings.Contains(string(raw), `"status":"online"`) {
			t.Errorf("member wire for %d: %s", u.ID, raw)
		}
	}
}

// ---- inbound: the space paths --------------------------------------------------------------

// presenceUsers is the slice of auth.UserRepository the presence paths read and write.
type presenceUsers struct {
	auth.UserRepository
	mu     sync.Mutex
	byID   map[int64]*auth.User
	writes int
}

func (u *presenceUsers) GetByID(_ context.Context, id int64) (*auth.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if x, ok := u.byID[id]; ok {
		c := *x
		return &c, nil
	}
	return nil, nil
}

func (u *presenceUsers) GetByRemote(_ context.Context, domain string, remoteID int64) (*auth.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, x := range u.byID {
		if x.IsRemote() && x.HomeDomain == domain && *x.RemoteID == remoteID {
			c := *x
			return &c, nil
		}
	}
	return nil, nil
}

func (u *presenceUsers) UpdateProfile(_ context.Context, id int64, upd *auth.ProfileUpdate) (*auth.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return nil, nil
	}
	u.writes++
	if p := upd.Presence; p != nil {
		if p.Online != nil {
			x.Presence.Online = *p.Online
		}
		if p.Status != nil {
			x.Presence.Status = *p.Status
		}
		if p.CustomStatus != nil {
			x.Presence.CustomStatus = *p.CustomStatus
		}
	}
	c := *x
	return &c, nil
}

func (u *presenceUsers) profileWrites() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.writes
}

// A snapshot, member page or join says nothing about what anyone is playing: activities in
// it are neither stored nor cleared, and do not count as a change.
func TestApplyRelayedPresenceIgnoresActivities(t *testing.T) {
	ctx := context.Background()
	store := useActivities(t)
	shadow := shadowUser(5, 50, "b.example")
	shadow.Presence = auth.UserPresence{Online: true, Status: "online"}
	users := &presenceUsers{byID: map[int64]*auth.User{5: shadow}}
	s := &Service{fcfg: config.FederationConfig{Domain: "a.example"}, users: users}
	home := auth.Activity{Type: "playing", Name: "Factorio", StartedAt: 1}
	_, _ = store.SetFederated(ctx, 5, &home, time.Now().UnixMilli())

	other := []auth.Activity{playingNow("Injected")}
	if s.applyRelayedPresence(ctx, shadow, auth.PublicPresence{Status: "online", Activities: other}) {
		t.Error("an activity alone counted as a change")
	}
	if s.applyRelayedPresence(ctx, shadow, auth.PublicPresence{Status: "online"}) {
		t.Error("no activities counted as a change")
	}
	if got := effectiveActivity(t, 5); got != "Factorio" {
		t.Errorf("after member relays with and without activities: %q", got)
	}
	if !s.applyRelayedPresence(ctx, shadow, auth.PublicPresence{Status: "dnd", Activities: other}) {
		t.Fatal("a status change was not applied")
	}
	if got := effectiveActivity(t, 5); got != "Factorio" || users.profileWrites() != 1 {
		t.Errorf("after a status change with activities: %q, %d writes", got, users.profileWrites())
	}
	// A user nobody relayed an activity for gets none from a member page either.
	bare := shadowUser(6, 60, "c.example")
	users.byID[6] = bare
	s.applyRelayedPresence(ctx, bare, auth.PublicPresence{Status: "online", Activities: other})
	if store.HasFederated(6) || effectiveActivity(t, 6) != "" {
		t.Error("a member relay stored an activity")
	}
}

// ---- inbound: POST /users/presence -----------------------------------------------------------

type relayRecorder struct {
	mu    sync.Mutex
	chans []string
}

func (r *relayRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *relayRecorder) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); cmd.Name() == "publish" && len(args) > 1 {
			r.mu.Lock()
			r.chans = append(r.chans, args[1].(string))
			r.mu.Unlock()
		}
		return nil
	}
}

func (r *relayRecorder) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return nil }
}

func (r *relayRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.chans)
}

func presenceApp(t *testing.T, users *presenceUsers) (*fiber.App, *relayRecorder) {
	t.Helper()
	rec := &relayRecorder{}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rdb.AddHook(rec)
	t.Cleanup(func() { _ = rdb.Close() })
	h := NewHandler(&Service{cfg: &config.Config{}, fcfg: config.FederationConfig{Domain: "a.example"}, users: users, redis: rdb})
	app := fiber.New()
	app.Post("/users/presence", func(c fiber.Ctx) error {
		c.Locals(localsKeyDomain, "b.example")
		return c.Next()
	}, h.Presence)
	return app, rec
}

func postPresence(t *testing.T, app *fiber.App, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/users/presence", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestPresenceRelayActivities(t *testing.T) {
	store := useActivities(t)
	shadow := shadowUser(5, 50, "b.example")
	shadow.Presence = auth.UserPresence{Online: true, Status: "online"}
	shadow.Relationships = []int64{1}
	users := &presenceUsers{byID: map[int64]*auth.User{5: shadow}}
	app, rec := presenceApp(t, users)
	now := time.Now().UnixMilli()
	relay := func(presence string) int {
		return postPresence(t, app, `{"user":"@50:b.example","presence":`+presence+`}`)
	}

	// Re-sanitised and clamped on the way in, stored without touching the shadow row.
	if code := relay(`{"status":"online","activities":[{"type":"playing","name":"  Mine‮craft  ","started_at":` + jsonNum(now+3_600_000) + `}]}`); code != http.StatusNoContent {
		t.Fatalf("relay: %d", code)
	}
	acts := stargate.ActivitiesOf(context.Background(), 5)
	if len(acts) != 1 || acts[0].Name != "Minecraft" || acts[0].StartedAt > time.Now().UnixMilli() {
		t.Fatalf("stored %+v", acts)
	}
	if users.profileWrites() != 0 {
		t.Errorf("an activity-only relay wrote the shadow row %d times", users.profileWrites())
	}
	if rec.count() == 0 {
		t.Error("an activity change was not published")
	}

	// The same relay again changes nothing and publishes nothing.
	published := rec.count()
	started := acts[0].StartedAt
	if code := relay(`{"status":"online","activities":[{"type":"playing","name":"Minecraft","started_at":` + jsonNum(started) + `}]}`); code != http.StatusNoContent {
		t.Fatalf("repeat: %d", code)
	}
	if rec.count() != published {
		t.Errorf("a repeated relay published %d frames", rec.count()-published)
	}

	// More than one activity is refused outright, and leaves the stored one alone.
	two := `{"status":"online","activities":[{"type":"playing","name":"A"},{"type":"playing","name":"B"}]}`
	if code := relay(two); code != http.StatusBadRequest {
		t.Errorf("two activities: %d", code)
	}
	if got := effectiveActivity(t, 5); got != "Minecraft" {
		t.Errorf("after a refused relay: %q", got)
	}

	// A peer speaks for its own users only.
	if code := postPresence(t, app, `{"user":"@50:c.example","presence":{"status":"online","activities":[{"type":"playing","name":"X"}]}}`); code != http.StatusForbidden {
		t.Errorf("another instance's user: %d", code)
	}

	// A type this instance does not know costs the activity, not the status.
	if code := relay(`{"status":"dnd","activities":[{"type":"listening","name":"Radio"}]}`); code != http.StatusNoContent {
		t.Fatalf("unknown type: %d", code)
	}
	if got := effectiveActivity(t, 5); got != "" || users.byID[5].Presence.Status != "dnd" {
		t.Errorf("unknown type: activity %q, status %q", got, users.byID[5].Presence.Status)
	}

	// Absent means none - which is also what a peer from before activities sends.
	_ = relay(`{"status":"dnd","activities":[{"type":"playing","name":"Minecraft"}]}`)
	if got := effectiveActivity(t, 5); got != "Minecraft" {
		t.Fatalf("set again: %q", got)
	}
	if code := relay(`{"status":"dnd"}`); code != http.StatusNoContent {
		t.Fatalf("clear: %d", code)
	}
	if store.HasFederated(5) {
		t.Error("a relay without activities left the activity in place")
	}

	// Offline is doing nothing, whatever the body says.
	_ = relay(`{"status":"online","activities":[{"type":"playing","name":"Minecraft"}]}`)
	if code := relay(`{"status":"offline","activities":[{"type":"playing","name":"Minecraft"}]}`); code != http.StatusNoContent {
		t.Fatalf("offline: %d", code)
	}
	if store.HasFederated(5) {
		t.Error("an offline user's activity was kept")
	}

	// Nobody here knows the user: nothing is stored.
	if code := postPresence(t, app, `{"user":"@51:b.example","presence":{"status":"online","activities":[{"type":"playing","name":"X"}]}}`); code != http.StatusNoContent {
		t.Errorf("unknown user: %d", code)
	}
	if store.HasFederated(51) {
		t.Error("an activity was stored for an unknown user")
	}
}

// "Playing nothing" that changes nothing here may follow an activity that lapsed here (the
// 12 h expiry) while local clients still show it, so it is published all the same. A relay
// that changes nothing for someone who reads offline stays quiet.
func TestPresenceRelayClearAfterLapse(t *testing.T) {
	ctx := context.Background()
	store := useActivities(t)
	shadow := shadowUser(5, 50, "b.example")
	shadow.Presence = auth.UserPresence{Online: true, Status: "online"}
	shadow.Relationships = []int64{1}
	users := &presenceUsers{byID: map[int64]*auth.User{5: shadow}}
	app, rec := presenceApp(t, users)
	relay := func(presence string) {
		t.Helper()
		if code := postPresence(t, app, `{"user":"@50:b.example","presence":`+presence+`}`); code != http.StatusNoContent {
			t.Fatalf("relay %s: %d", presence, code)
		}
	}

	relay(`{"status":"online","activities":[{"type":"playing","name":"Minecraft"}]}`)
	_, _ = store.SetFederated(ctx, 5, nil, 0) // the entry expires here
	published := rec.count()
	relay(`{"status":"online"}`)
	if rec.count() == published {
		t.Error("the end of a lapsed activity was not published")
	}

	relay(`{"status":"offline"}`)
	published = rec.count()
	relay(`{"status":"offline"}`)
	if rec.count() != published {
		t.Errorf("a relay that changed nothing for an offline user published %d frames", rec.count()-published)
	}
	if users.profileWrites() != 1 {
		t.Errorf("%d shadow-row writes, want the one for going offline", users.profileWrites())
	}
}

func jsonNum(v int64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
