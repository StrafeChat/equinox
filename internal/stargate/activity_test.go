package stargate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// ---- fakes ---------------------------------------------------------------------------

// pubRecorder is a go-redis hook that answers every command itself, so a client built on
// it never dials anything; it keeps each PUBLISH for the test to look at.
type pubRecorder struct {
	mu   sync.Mutex
	pubs []recordedPub
}

type recordedPub struct {
	channel  string
	event    string
	presence map[string]any // data.presence of a PRESENCE_UPDATE
}

func (r *pubRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *pubRecorder) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() != "publish" || len(args) != 3 {
			return nil
		}
		ch, _ := args[1].(string)
		raw, _ := args[2].([]byte)
		var env struct {
			Type string `json:"t"`
			Data struct {
				Presence map[string]any `json:"presence"`
			} `json:"d"`
		}
		_ = json.Unmarshal(raw, &env)
		r.mu.Lock()
		r.pubs = append(r.pubs, recordedPub{channel: ch, event: env.Type, presence: env.Data.Presence})
		r.mu.Unlock()
		return nil
	}
}

func (r *pubRecorder) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return nil }
}

func (r *pubRecorder) all() []recordedPub {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedPub(nil), r.pubs...)
}

func (r *pubRecorder) on(channel string) []recordedPub {
	var out []recordedPub
	for _, p := range r.all() {
		if p.channel == channel {
			out = append(out, p)
		}
	}
	return out
}

func recordingRedis(t *testing.T) (*redis.Client, *pubRecorder) {
	t.Helper()
	rec := &pubRecorder{}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rdb.AddHook(rec)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, rec
}

// activityNames lists the names in a recorded presence's activities.
func activityNames(p map[string]any) []string {
	list, _ := p["activities"].([]any)
	var out []string
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m["name"].(string))
		}
	}
	return out
}

type fakeUsers struct {
	auth.UserRepository
	mu       sync.Mutex
	byID     map[int64]*auth.User
	onUpdate func()
}

func (f *fakeUsers) GetByID(_ context.Context, id int64) (*auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.byID[id]; ok {
		c := *u
		return &c, nil
	}
	return nil, nil
}

// UpdateProfile applies the online flag, which is all the gateway's connect and
// disconnect write; onUpdate, when set, runs first (the test's look at the moment of the
// write).
func (f *fakeUsers) UpdateProfile(_ context.Context, id int64, upd *auth.ProfileUpdate) (*auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onUpdate != nil {
		f.onUpdate()
	}
	u, ok := f.byID[id]
	if !ok {
		return nil, nil
	}
	if upd.Presence != nil && upd.Presence.Online != nil {
		u.Presence.Online = *upd.Presence.Online
	}
	c := *u
	return &c, nil
}

// fakeFederator records how each relayed user appeared to others.
type fakeFederator struct {
	mu       sync.Mutex
	calls    int
	statuses []string
}

func (f *fakeFederator) AfterPresenceChanged(_ context.Context, u *auth.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.statuses = append(f.statuses, auth.ToPublicPresence(u.Presence, true).Status)
}

func (f *fakeFederator) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeFederator) relayed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statuses...)
}

// recNotifier is the hub's PresenceNotifier in tests: connects are ignored, disconnects,
// activity changes and keepalive relays counted per user.
type recNotifier struct {
	mu       sync.Mutex
	changes  map[int64]int
	relays   map[int64]int
	offlines map[int64]int
}

func (n *recNotifier) bump(m *map[int64]int, userID int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if *m == nil {
		*m = map[int64]int{}
	}
	(*m)[userID]++
}

func (n *recNotifier) read(m *map[int64]int, userID int64) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return (*m)[userID]
}

func (n *recNotifier) OnConnect(context.Context, int64)                {}
func (n *recNotifier) OnDisconnect(_ context.Context, userID int64)    { n.bump(&n.offlines, userID) }
func (n *recNotifier) ActivityChanged(_ context.Context, userID int64) { n.bump(&n.changes, userID) }
func (n *recNotifier) RelayActivity(_ context.Context, userID int64)   { n.bump(&n.relays, userID) }

func (n *recNotifier) count(userID int64) int    { return n.read(&n.changes, userID) }
func (n *recNotifier) relayed(userID int64) int  { return n.read(&n.relays, userID) }
func (n *recNotifier) offlined(userID int64) int { return n.read(&n.offlines, userID) }

// countingStore counts writes to an in-memory store; with gate set, SetConn waits for it,
// and while fail is above zero each write fails (and counts it down).
type countingStore struct {
	*MemoryActivityStore
	mu     sync.Mutex
	writes int
	fail   int
	gate   chan struct{}
}

var errTestRedisDown = errors.New("redis is down")

// write counts one write and reports whether it should fail.
func (s *countingStore) write() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.fail > 0 {
		s.fail--
		return errTestRedisDown
	}
	return nil
}

func (s *countingStore) SetConn(ctx context.Context, userID, connID int64, a auth.Activity, at int64) (bool, error) {
	if s.gate != nil {
		<-s.gate
	}
	if err := s.write(); err != nil {
		return false, err
	}
	return s.MemoryActivityStore.SetConn(ctx, userID, connID, a, at)
}

func (s *countingStore) ClearConn(ctx context.Context, userID, connID int64) (bool, error) {
	if err := s.write(); err != nil {
		return false, err
	}
	return s.MemoryActivityStore.ClearConn(ctx, userID, connID)
}

func (s *countingStore) failNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = n
}

func (s *countingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func useActivityStore(t *testing.T, s ActivityStore) {
	t.Helper()
	prev := CurrentActivityStore()
	SetActivityStore(s)
	t.Cleanup(func() { SetActivityStore(prev) })
}

func newActivityHub(t *testing.T, n PresenceNotifier) *Hub {
	t.Helper()
	h := NewHubWithConfig(HubConfig{PresenceNotifier: n})
	h.grace = 30 * time.Millisecond
	h.activityRetry = 10 * time.Millisecond
	return h
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settle waits until the connection's worker is idle with nothing pending.
func settle(t *testing.T, c *Client) {
	t.Helper()
	waitFor(t, "the activity worker to go idle", func() bool {
		c.act.mu.Lock()
		defer c.act.mu.Unlock()
		return !c.act.running && c.act.pending == nil && c.act.timer == nil && !c.act.keepaliveDue
	})
}

func effectiveName(t *testing.T, userID int64) string {
	t.Helper()
	acts := ActivitiesOf(context.Background(), userID)
	if len(acts) == 0 {
		return ""
	}
	return acts[0].Name
}

func frame(acts string) []byte {
	if acts == "" {
		return []byte(`{"op":9,"d":null}`)
	}
	return []byte(`{"op":9,"d":{"activities":[` + acts + `]}}`)
}

func playing(name string, startedAt int64) string {
	raw, _ := json.Marshal(auth.Activity{Type: "playing", Name: name, StartedAt: startedAt})
	return string(raw)
}

// errorFrames drains the client's outgoing buffer and returns the error codes in it.
func errorFrames(c *Client) []int {
	var codes []int
	for {
		select {
		case raw := <-c.send:
			var f struct {
				Op int          `json:"op"`
				D  ErrorPayload `json:"d"`
			}
			if json.Unmarshal(raw, &f) == nil && f.Op == OpError {
				codes = append(codes, f.D.Code)
			}
		default:
			return codes
		}
	}
}

// ---- parsing and the bucket --------------------------------------------------------------

func TestParseActivityFrame(t *testing.T) {
	now := time.UnixMilli(1_761_000_000_000)
	start := now.UnixMilli() - 60_000
	cases := []struct {
		name       string
		raw        string
		want       *auth.Activity
		startedNow bool
		invalid    bool
	}{
		{name: "one game", raw: `{"op":9,"d":{"activities":[{"type":"playing","name":" Minecraft ","started_at":` + jsonInt(start) + `}]}}`, want: &auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: start}},
		{name: "no start", raw: `{"op":9,"d":{"activities":[{"type":"playing","name":"Minecraft"}]}}`, want: &auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: now.UnixMilli()}, startedNow: true},
		{name: "a start in the future", raw: `{"op":9,"d":{"activities":[{"type":"playing","name":"Minecraft","started_at":` + jsonInt(now.UnixMilli()+90_000) + `}]}}`, want: &auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: now.UnixMilli()}, startedNow: true},
		{name: "null d clears", raw: `{"op":9,"d":null}`},
		{name: "no d clears", raw: `{"op":9}`},
		{name: "empty list clears", raw: `{"op":9,"d":{"activities":[]}}`},
		{name: "null list clears", raw: `{"op":9,"d":{"activities":null}}`},
		{name: "two activities", raw: `{"op":9,"d":{"activities":[{"type":"playing","name":"A"},{"type":"playing","name":"B"}]}}`, invalid: true},
		{name: "unknown type", raw: `{"op":9,"d":{"activities":[{"type":"streaming","name":"A"}]}}`, invalid: true},
		{name: "blank name", raw: `{"op":9,"d":{"activities":[{"type":"playing","name":"⠀"}]}}`, invalid: true},
		{name: "name not a string", raw: `{"op":9,"d":{"activities":[{"type":"playing","name":5}]}}`, invalid: true},
		{name: "start not a number", raw: `{"op":9,"d":{"activities":[{"type":"playing","name":"A","started_at":"soon"}]}}`, invalid: true},
		{name: "d is a list", raw: `{"op":9,"d":[{"type":"playing","name":"A"}]}`, invalid: true},
		{name: "d is a string", raw: `{"op":9,"d":"Minecraft"}`, invalid: true},
	}
	for _, c := range cases {
		act, startedNow, err := parseActivityFrame([]byte(c.raw), now)
		if c.invalid {
			if err == nil {
				t.Errorf("%s: accepted as %+v", c.name, act)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !auth.SameActivity(act, c.want) || startedNow != c.startedNow {
			t.Errorf("%s: got %+v (startedNow %v), want %+v (%v)", c.name, act, startedNow, c.want, c.startedNow)
		}
	}
}

func jsonInt(v int64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Op 9's bucket: a burst of five, then one report every 12 s, never more than the burst.
func TestActivityTokenBucket(t *testing.T) {
	var a clientActivity
	now := time.Now()
	for i := 0; i < activityBurst; i++ {
		if !a.takeTokenLocked(now, activityRefillInterval) {
			t.Fatalf("report %d of the burst refused", i+1)
		}
	}
	if a.takeTokenLocked(now, activityRefillInterval) {
		t.Fatal("a report past the burst was allowed")
	}
	if d := a.untilTokenLocked(now, activityRefillInterval); d != activityRefillInterval {
		t.Errorf("next token in %v, want %v", d, activityRefillInterval)
	}
	now = now.Add(activityRefillInterval - time.Second)
	if a.takeTokenLocked(now, activityRefillInterval) {
		t.Fatal("a token before 12 s")
	}
	now = now.Add(time.Second)
	if !a.takeTokenLocked(now, activityRefillInterval) {
		t.Fatal("no token after 12 s")
	}
	if a.takeTokenLocked(now, activityRefillInterval) {
		t.Fatal("two tokens after 12 s")
	}
	now = now.Add(time.Hour)
	n := 0
	for a.takeTokenLocked(now, activityRefillInterval) {
		n++
	}
	if n != activityBurst {
		t.Fatalf("after an idle hour %d reports allowed, want %d", n, activityBurst)
	}
}

// ---- the per-connection worker ---------------------------------------------------------

// Reports land in the order they were sent: a set followed by a clear ends cleared.
func TestActivityReportsApplyInOrder(t *testing.T) {
	store := &countingStore{MemoryActivityStore: NewMemoryActivityStore()}
	useActivityStore(t, store)
	n := &recNotifier{}
	h := newActivityHub(t, n)
	c := newClient(h, nil, 1, 0)

	c.handleActivity(frame(playing("Minecraft", 0)))
	c.handleActivity(frame(""))
	c.handleActivity(frame(playing("Factorio", 0)))
	settle(t, c)
	if got := effectiveName(t, 1); got != "Factorio" {
		t.Fatalf("after set, clear, set: %q", got)
	}
	c.handleActivity(frame(playing("Minecraft", 0)))
	c.handleActivity(frame(""))
	settle(t, c)
	if got := effectiveName(t, 1); got != "" {
		t.Fatalf("after set, clear: %q is still shown", got)
	}
	if codes := errorFrames(c); len(codes) != 0 {
		t.Errorf("valid reports were answered with %v", codes)
	}
}

// Reporting what this connection already reports writes and publishes nothing.
func TestIdenticalActivityReportIsSuppressed(t *testing.T) {
	store := &countingStore{MemoryActivityStore: NewMemoryActivityStore()}
	useActivityStore(t, store)
	n := &recNotifier{}
	h := newActivityHub(t, n)
	c := newClient(h, nil, 1, 0)
	start := time.Now().Add(-time.Hour).UnixMilli()

	c.handleActivity(frame(playing("Minecraft", start)))
	settle(t, c)
	c.handleActivity(frame(playing("Minecraft", start)))
	c.handleActivity(frame(playing(" Minecraft ", start))) // the same once sanitised
	settle(t, c)
	if store.count() != 1 || n.count(1) != 1 {
		t.Fatalf("an identical report wrote %d times and published %d times, want 1 and 1", store.count(), n.count(1))
	}
	// A repeat without a start of its own keeps the clock already shown.
	c.handleActivity(frame(playing("Minecraft", 0)))
	settle(t, c)
	if acts := ActivitiesOf(context.Background(), 1); len(acts) != 1 || acts[0].StartedAt != start || store.count() != 1 {
		t.Fatalf("a repeat with no start: %+v after %d writes", acts, store.count())
	}
	// A clear with nothing reported is nothing too.
	c2 := newClient(h, nil, 2, 0)
	c2.handleActivity(frame(""))
	settle(t, c2)
	if store.count() != 1 || n.count(2) != 0 {
		t.Fatalf("a clear of nothing wrote (%d writes) or published (%d)", store.count(), n.count(2))
	}
}

// Over budget a report waits for the next token instead of being dropped, newer ones
// replacing it, and nothing is said to the client: the last report always lands.
func TestOverBudgetActivityIsCoalescedNotDropped(t *testing.T) {
	store := &countingStore{MemoryActivityStore: NewMemoryActivityStore()}
	useActivityStore(t, store)
	h := newActivityHub(t, &recNotifier{})
	h.activityRefill = 150 * time.Millisecond
	c := newClient(h, nil, 1, 0)

	names := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L"}
	for _, name := range names {
		c.handleActivity(frame(playing(name, 0)))
	}
	settle(t, c)
	if got := effectiveName(t, 1); got != "L" {
		t.Fatalf("after a burst the store shows %q, want the last report", got)
	}
	if w := store.count(); w > activityBurst+1 {
		t.Errorf("%d writes for %d reports; over budget they should coalesce", w, len(names))
	}
	if codes := errorFrames(c); len(codes) != 0 {
		t.Errorf("over budget answered with %v, want nothing", codes)
	}
	// A clear that arrives over budget is applied too, once a token frees.
	c.handleActivity(frame(""))
	settle(t, c)
	if got := effectiveName(t, 1); got != "" {
		t.Fatalf("an over-budget clear was lost: %q still shown", got)
	}
}

// A report that is no longer wanted by the time a token frees is never written.
func TestActivityReportUndoneWhileWaiting(t *testing.T) {
	store := &countingStore{MemoryActivityStore: NewMemoryActivityStore()}
	useActivityStore(t, store)
	h := newActivityHub(t, &recNotifier{})
	h.activityRefill = time.Hour
	c := newClient(h, nil, 1, 0)
	start := time.Now().Add(-time.Minute).UnixMilli()
	for _, name := range []string{"A", "B", "C", "D", "E"} {
		c.handleActivity(frame(playing(name, start)))
		settle(t, c)
	}
	before := store.count()
	c.handleActivity(frame(playing("F", start))) // over budget: waits
	c.handleActivity(frame(playing("E", start))) // back to what is stored
	c.act.mu.Lock()
	pending := c.act.pending
	c.act.mu.Unlock()
	if pending != nil || store.count() != before {
		t.Fatalf("pending %+v, %d writes; the undone report should be gone", pending, store.count()-before)
	}
	c.closeActivity()
}

// An invalid report is answered 4007 and leaves the connection - and what it reports - as
// they were.
func TestInvalidActivityAnswers4007(t *testing.T) {
	store := &countingStore{MemoryActivityStore: NewMemoryActivityStore()}
	useActivityStore(t, store)
	h := newActivityHub(t, &recNotifier{})
	c := newClient(h, nil, 1, 0)
	c.handleActivity(frame(playing("Minecraft", 0)))
	settle(t, c)

	for _, raw := range []string{
		`{"op":9,"d":{"activities":[{"type":"playing","name":"A"},{"type":"playing","name":"B"}]}}`,
		`{"op":9,"d":{"activities":[{"type":"listening","name":"A"}]}}`,
		`{"op":9,"d":{"activities":[{"type":"playing","name":"   "}]}}`,
		`{"op":9,"d":{"activities":[{"type":"playing","name":"` + strings.Repeat("x", 129) + `"}]}}`,
	} {
		c.handleActivity([]byte(raw))
	}
	settle(t, c)
	codes := errorFrames(c)
	if len(codes) != 4 {
		t.Fatalf("error frames %v, want four 4007s", codes)
	}
	for _, code := range codes {
		if code != errInvalidActivity {
			t.Errorf("error code %d, want %d", code, errInvalidActivity)
		}
	}
	if got := effectiveName(t, 1); got != "Minecraft" || store.count() != 1 {
		t.Errorf("an invalid report changed the activity: %q after %d writes", got, store.count())
	}
}

// ---- lifetime ------------------------------------------------------------------------------

// A closed connection's entry goes a grace period after the socket. While the user keeps
// another socket the change is published to them and their audience; when it was their
// last, the entry goes with the offline, unannounced.
func TestActivityClearedAfterDisconnectGrace(t *testing.T) {
	store := NewMemoryActivityStore()
	useActivityStore(t, store)
	n := &recNotifier{}
	h := newActivityHub(t, n)

	// Two sockets; the one playing closes.
	gaming := newClient(h, nil, 1, 0)
	other := newClient(h, nil, 1, 0)
	h.register(gaming)
	h.register(other)
	gaming.handleActivity(frame(playing("Minecraft", 0)))
	settle(t, gaming)
	if n.count(1) != 1 {
		t.Fatalf("setting the activity published %d times", n.count(1))
	}
	h.unregister(gaming)
	if store.Conns(1) != 1 {
		t.Fatal("the entry went before the grace period")
	}
	waitFor(t, "the cleanup", func() bool { return store.Conns(1) == 0 })
	waitFor(t, "the cleanup's publish", func() bool { return n.count(1) == 2 })
	if got := effectiveName(t, 1); got != "" {
		t.Errorf("after the grace %q is still shown", got)
	}

	// The last socket, playing too, closes: its entry is gone by the time the offline is
	// written, and its going is not published.
	other.handleActivity(frame(playing("Factorio", 0)))
	settle(t, other)
	h.unregister(other)
	if store.Conns(1) != 1 {
		t.Fatal("the last socket's entry went before the grace period")
	}
	waitFor(t, "the offline", func() bool { return n.offlined(1) == 1 })
	if store.Conns(1) != 0 {
		t.Error("the offline came before the last socket's entry went")
	}
	time.Sleep(3 * h.grace)
	if got := n.count(1); got != 3 {
		t.Errorf("%d activity changes published, want 3: the last socket's cleanup published", got)
	}
}

// The ghost online: when a playing user's last socket closes, nothing after it says they
// are online - every PRESENCE_UPDATE reads offline, without a game, and the one relay to
// other instances is the offline's.
func TestLastSocketCloseSendsOnlyOffline(t *testing.T) {
	store := NewMemoryActivityStore()
	useActivityStore(t, store)
	rdb, rec := recordingRedis(t)
	users := &fakeUsers{byID: map[int64]*auth.User{1: activityUser("online", false)}}
	fed := &fakeFederator{}
	notifier := NewDefaultPresenceNotifier(users, rdb, "")
	notifier.SetFederator(fed)
	h := newActivityHub(t, notifier)

	c := newClient(h, nil, 1, 0)
	h.register(c)
	waitFor(t, "the connect", func() bool { return fed.count() == 1 })
	c.handleActivity(frame(playing("Minecraft", 0)))
	settle(t, c)
	if space := rec.on("stargate:space:100"); fed.count() != 2 || len(space) != 2 || len(activityNames(space[1].presence)) != 1 {
		t.Fatalf("the activity went out %d times: %+v", fed.count()-1, space)
	}

	before, fedBefore := len(rec.all()), fed.count()
	entryLeft := -1
	users.mu.Lock()
	users.onUpdate = func() { entryLeft = store.Conns(1) }
	users.mu.Unlock()
	h.unregister(c)
	waitFor(t, "the offline", func() bool {
		users.mu.Lock()
		defer users.mu.Unlock()
		return !users.byID[1].Presence.Online
	})
	time.Sleep(3 * h.grace)

	after := rec.all()[before:]
	if len(after) == 0 {
		t.Fatal("the offline was not published")
	}
	for _, p := range after {
		if p.event != presenceUpdateEvent || p.presence["status"] != "offline" || p.presence["activities"] != nil {
			t.Errorf("after the last socket closed %s got %s %v", p.channel, p.event, p.presence)
		}
	}
	if relayed := fed.relayed()[fedBefore:]; len(relayed) != 1 || relayed[0] != "offline" {
		t.Errorf("relayed after the last socket closed: %v, want only the offline", relayed)
	}
	users.mu.Lock()
	left := entryLeft
	users.mu.Unlock()
	if left != 0 {
		t.Errorf("%d entries still stored when the offline was written", left)
	}
}

// A clear whose entry had lapsed - the store's expiry, or Redis losing it - changes nothing
// in the store, but whoever was told about the game still shows it, so it is published.
// A clear from a connection that never reported anything is still nothing.
func TestLapsedActivityClearStillPublishes(t *testing.T) {
	store := NewMemoryActivityStore()
	useActivityStore(t, store)
	n := &recNotifier{}
	h := newActivityHub(t, n)
	c := newClient(h, nil, 1, 0)
	c.handleActivity(frame(playing("Minecraft", 0)))
	settle(t, c)
	_, _ = store.ClearConn(context.Background(), 1, c.connID) // the entry expires
	c.handleActivity(frame(""))
	settle(t, c)
	if got := n.count(1); got != 2 {
		t.Errorf("published %d times, want the game and its end", got)
	}
	c.closeActivity()

	quiet := newClient(h, nil, 2, 0)
	quiet.handleActivity(frame(playing("Factorio", 0)))
	quiet.handleActivity(frame(""))
	settle(t, quiet)
	if got := n.count(2); got > 2 {
		t.Errorf("a set and a clear published %d times", got)
	}
	quiet.closeActivity()
}

// A write that fails is tried again after a short wait, a token per try, until it lands -
// unless something newer replaced it, or the socket closed.
func TestFailedActivityWriteIsRetried(t *testing.T) {
	store := &countingStore{MemoryActivityStore: NewMemoryActivityStore()}
	useActivityStore(t, store)
	n := &recNotifier{}
	h := newActivityHub(t, n)
	c := newClient(h, nil, 1, 0)

	store.failNext(2)
	c.handleActivity(frame(playing("Minecraft", 0)))
	settle(t, c)
	if got := effectiveName(t, 1); got != "Minecraft" || store.count() != 3 || n.count(1) != 1 {
		t.Fatalf("after two failures: %q shown, %d writes, %d publishes", got, store.count(), n.count(1))
	}
	c.act.mu.Lock()
	tokens := c.act.tokens
	c.act.mu.Unlock()
	if tokens != activityBurst-3 {
		t.Errorf("%d tokens left after three tries, want %d", tokens, activityBurst-3)
	}

	// A newer report takes a waiting retry's place.
	h.activityRetry = 200 * time.Millisecond
	store.failNext(1)
	c.handleActivity(frame(playing("Factorio", 0)))
	waitFor(t, "the retry to wait", func() bool {
		c.act.mu.Lock()
		defer c.act.mu.Unlock()
		return c.act.timer != nil && c.act.pending != nil
	})
	c.handleActivity(frame(""))
	settle(t, c)
	if got := effectiveName(t, 1); got != "" || store.count() != 5 || n.count(1) != 2 {
		t.Fatalf("after a replaced retry: %q shown, %d writes, %d publishes", got, store.count(), n.count(1))
	}
	c.closeActivity()

	// A closed connection stops trying.
	h.activityRetry = 5 * time.Millisecond
	c2 := newClient(h, nil, 2, 0)
	store.failNext(1 << 20)
	w := store.count()
	c2.handleActivity(frame(playing("Minecraft", 0)))
	waitFor(t, "a retry", func() bool { return store.count() >= w+2 })
	c2.closeActivity()
	waitFor(t, "the worker to stop", func() bool {
		c2.act.mu.Lock()
		defer c2.act.mu.Unlock()
		return !c2.act.running
	})
	w = store.count()
	time.Sleep(20 * h.activityRetry)
	if store.count() != w {
		t.Errorf("%d writes after the socket closed", store.count()-w)
	}
	store.failNext(0)

	// The wait: the base, then twice it from then on.
	plain := &Client{}
	for i, want := range []time.Duration{activityRetryDelay, activityRetryMax, activityRetryMax, activityRetryMax} {
		if got := plain.activityRetryAfter(i + 1); got != want {
			t.Errorf("retry %d after %v, want %v", i+1, got, want)
		}
	}
}

// While a connection holds an activity its entry is written again on every keepalive,
// renewing the expiry without publishing, and every activityRelayTicks-th time the
// presence is relayed to other instances again. An entry Redis lost comes back, published.
// A clear or a close stops the ticks.
func TestActivityKeepalive(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{MemoryActivityStore: NewMemoryActivityStore()}
	useActivityStore(t, store)
	n := &recNotifier{}
	h := newActivityHub(t, n)
	h.activityKeepalive = 15 * time.Millisecond
	start := time.Now().Add(-time.Hour).UnixMilli()

	c := newClient(h, nil, 1, 0)
	c.handleActivity(frame(playing("Minecraft", start)))
	waitFor(t, "a keepalive relay", func() bool { return n.relayed(1) >= 1 })
	if w := store.count(); w < 1+activityRelayTicks {
		t.Errorf("%d writes by the first relay, want the report and %d keepalives", w, activityRelayTicks)
	}
	if got := n.count(1); got != 1 {
		t.Errorf("keepalives published %d changes", got-1)
	}
	if acts := ActivitiesOf(ctx, 1); len(acts) != 1 || acts[0].Name != "Minecraft" || acts[0].StartedAt != start {
		t.Errorf("after keepalives: %+v", acts)
	}

	// Redis lost the entry: the next keepalive puts it back, and says so.
	_, _ = store.MemoryActivityStore.ClearConn(ctx, 1, c.connID)
	waitFor(t, "the entry back", func() bool { return store.Conns(1) == 1 })
	waitFor(t, "its publish", func() bool { return n.count(1) == 2 })

	// A clear stops the ticks.
	c.handleActivity(frame(""))
	settle(t, c)
	w := store.count()
	time.Sleep(5 * h.activityKeepalive)
	if store.count() != w || store.Conns(1) != 0 {
		t.Errorf("after a clear: %d more writes, %d entries", store.count()-w, store.Conns(1))
	}

	// So does a close.
	c2 := newClient(h, nil, 2, 0)
	c2.handleActivity(frame(playing("Factorio", start)))
	settle(t, c2)
	c2.closeActivity()
	w = store.count()
	time.Sleep(5 * h.activityKeepalive)
	if store.count() != w {
		t.Errorf("%d keepalive writes after the socket closed", store.count()-w)
	}
}

// The keepalive relay goes to other instances only while others see the user, and tells
// nobody here anything.
func TestRelayActivityOnlyWhileVisible(t *testing.T) {
	rdb, rec := recordingRedis(t)
	users := &fakeUsers{byID: map[int64]*auth.User{1: activityUser("online", true)}}
	fed := &fakeFederator{}
	notifier := NewDefaultPresenceNotifier(users, rdb, "")
	notifier.SetFederator(fed)

	notifier.RelayActivity(context.Background(), 1)
	if fed.count() != 1 || len(rec.all()) != 0 {
		t.Fatalf("visible: %d relays, %d publishes", fed.count(), len(rec.all()))
	}
	for _, u := range []*auth.User{activityUser("invisible", true), activityUser("online", false)} {
		users.mu.Lock()
		users.byID[1] = u
		users.mu.Unlock()
		notifier.RelayActivity(context.Background(), 1)
	}
	if fed.count() != 1 || len(rec.all()) != 0 {
		t.Errorf("invisible or offline: %d relays, %d publishes", fed.count()-1, len(rec.all()))
	}
}

// A desktop that reconnects and reports the same game again changes nothing anyone sees:
// neither its new report nor the old connection's cleanup publishes.
func TestReconnectReReportPublishesNothing(t *testing.T) {
	store := NewMemoryActivityStore()
	useActivityStore(t, store)
	n := &recNotifier{}
	h := newActivityHub(t, n)
	start := time.Now().Add(-time.Hour).UnixMilli()

	old := newClient(h, nil, 1, 7)
	h.register(old)
	old.handleActivity(frame(playing("Minecraft", start)))
	settle(t, old)
	h.unregister(old)

	// Same session (a reload), new socket, same game and start.
	fresh := newClient(h, nil, 1, 7)
	h.register(fresh)
	fresh.handleActivity(frame(playing("Minecraft", start)))
	settle(t, fresh)
	waitFor(t, "the old connection's cleanup", func() bool { return store.Conns(1) == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := n.count(1); got != 1 {
		t.Fatalf("published %d times, want only the first report's", got)
	}
	if got := effectiveName(t, 1); got != "Minecraft" {
		t.Fatalf("after the reconnect %q is shown", got)
	}
	if fresh.connID == old.connID {
		t.Fatal("two sockets of one session share a connection id")
	}
}

// Once the socket is gone nothing is written for it: not a report still waiting for a
// token, and not one that arrives late. A write already in flight is cleared by the
// cleanup after it.
func TestClosedConnectionWritesNothing(t *testing.T) {
	store := &countingStore{MemoryActivityStore: NewMemoryActivityStore()}
	useActivityStore(t, store)
	h := newActivityHub(t, &recNotifier{})
	h.activityRefill = time.Hour

	c := newClient(h, nil, 1, 0)
	h.register(c)
	for _, name := range []string{"A", "B", "C", "D", "E"} {
		c.handleActivity(frame(playing(name, 1)))
		settle(t, c)
	}
	c.handleActivity(frame(playing("F", 1))) // waits for a token that never comes
	h.unregister(c)
	c.queueActivity(activityReport{act: &auth.Activity{Type: "playing", Name: "G"}, at: 1}, false)
	waitFor(t, "the cleanup", func() bool { return store.Conns(1) == 0 })
	if store.count() != activityBurst+1 { // five reports and the cleanup
		t.Errorf("%d writes, want %d", store.count(), activityBurst+1)
	}

	// A write in flight when the socket closes.
	gate := make(chan struct{})
	store2 := &countingStore{MemoryActivityStore: NewMemoryActivityStore(), gate: gate}
	useActivityStore(t, store2)
	c2 := newClient(h, nil, 2, 0)
	h.register(c2)
	c2.handleActivity(frame(playing("Minecraft", 1)))
	waitFor(t, "the write to start", func() bool {
		c2.act.mu.Lock()
		defer c2.act.mu.Unlock()
		return c2.act.running && c2.act.pending == nil
	})
	h.unregister(c2)
	close(gate)
	waitFor(t, "the in-flight write", func() bool { return store2.count() >= 1 })
	waitFor(t, "the cleanup after it", func() bool { return store2.Conns(2) == 0 && store2.count() == 2 })
}

// ---- the effective activity -----------------------------------------------------------------

func TestPickEffective(t *testing.T) {
	enc := func(name string, at int64) string {
		return encodeActivity(auth.Activity{Type: "playing", Name: name, StartedAt: 1}, at)
	}
	cases := []struct {
		name   string
		fields map[string]string
		fed    string
		want   string
	}{
		{"nothing", nil, "", ""},
		{"one connection", map[string]string{"11": enc("A", 5)}, "", "A"},
		{"the latest report wins", map[string]string{"11": enc("A", 5), "12": enc("B", 9), "13": enc("C", 7)}, "", "B"},
		{"a tie goes to the greater field", map[string]string{"11": enc("A", 5), "12": enc("B", 5)}, "", "B"},
		{"the federated entry counts", map[string]string{"11": enc("A", 5)}, enc("F", 6), "F"},
		{"an older federated entry loses", map[string]string{"11": enc("A", 7)}, enc("F", 6), "A"},
		{"garbage is skipped", map[string]string{"11": "not json", "12": `{"at":3}`, "13": enc("C", 1)}, "", "C"},
	}
	for _, c := range cases {
		got := pickEffective(c.fields, c.fed)
		name := ""
		if got != nil {
			name = got.Name
		}
		if name != c.want {
			t.Errorf("%s: effective %q, want %q", c.name, name, c.want)
		}
	}
	// "at" leads the value: the Lua script reads it by prefix.
	if v := enc("A", 42); !strings.HasPrefix(v, `{"at":42,`) {
		t.Errorf("stored value %s does not start with at", v)
	}
}

// "changed" is about what others see - type, name, start - never when it was reported.
func TestMemoryStoreChangedIgnoresReportTime(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryActivityStore()
	a := auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: 100}
	if changed, _ := s.SetConn(ctx, 1, 11, a, 1000); !changed {
		t.Fatal("the first activity is not a change")
	}
	if changed, _ := s.SetConn(ctx, 1, 11, a, 2000); changed {
		t.Error("the same activity reported later is a change")
	}
	if changed, _ := s.SetConn(ctx, 1, 12, a, 3000); changed {
		t.Error("a second connection reporting the same activity is a change")
	}
	if changed, _ := s.ClearConn(ctx, 1, 12); changed {
		t.Error("clearing one of two identical reports is a change")
	}
	b := a
	b.StartedAt = 200
	if changed, _ := s.SetConn(ctx, 1, 12, b, 4000); !changed {
		t.Error("a new start is not a change")
	}
	if changed, _ := s.ClearConn(ctx, 1, 12); !changed {
		t.Error("going back to the older report is not a change")
	}
	if changed, _ := s.SetFederated(ctx, 1, &auth.Activity{Type: "playing", Name: "Remote", StartedAt: 1}, 5000); !changed {
		t.Error("a newer federated entry is not a change")
	}
	if changed, _ := s.SetFederated(ctx, 1, nil, 0); !changed {
		t.Error("removing the effective federated entry is not a change")
	}
	got, _ := s.Effective(ctx, []int64{1, 2})
	if len(got) != 1 || got[1][0].Name != "Minecraft" {
		t.Errorf("batch read %+v", got)
	}
}

// ---- fan-out ------------------------------------------------------------------------------

func activityUser(status string, online bool) *auth.User {
	return &auth.User{
		ID: 1, Username: "gamer",
		Presence:      auth.UserPresence{Online: online, Status: status},
		Relationships: []int64{2, 3},
		Spaces:        []int64{100, 101},
	}
}

// PRESENCE_UPDATE carries what the store holds for the user (the store both binaries
// share), and the others view only while it is not offline.
func TestPublishPresenceUpdateCarriesActivities(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryActivityStore()
	useActivityStore(t, store)
	_, _ = store.SetConn(ctx, 1, 11, auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: 1}, 1)

	rdb, rec := recordingRedis(t)
	PublishPresenceUpdate(ctx, rdb, "", activityUser("dnd", true))
	pubs := rec.all()
	if len(pubs) != 5 { // self, two friends, two spaces
		t.Fatalf("%d publishes, want 5", len(pubs))
	}
	for _, p := range pubs {
		if p.event != presenceUpdateEvent || strings.Join(activityNames(p.presence), ",") != "Minecraft" {
			t.Errorf("%s: %s %v", p.channel, p.event, p.presence)
		}
	}

	rdb, rec = recordingRedis(t)
	PublishPresenceUpdate(ctx, rdb, "", activityUser("invisible", true))
	if self := rec.on("stargate:user:1"); len(self) != 1 || self[0].presence["status"] != "invisible" || len(activityNames(self[0].presence)) != 1 {
		t.Errorf("an invisible user's own view: %+v", self)
	}
	for _, ch := range []string{"stargate:user:2", "stargate:user:3", "stargate:space:100", "stargate:space:101"} {
		p := rec.on(ch)
		if len(p) != 1 || p[0].presence["status"] != "offline" {
			t.Fatalf("%s: %+v", ch, p)
		}
		if _, ok := p[0].presence["activities"]; ok {
			t.Errorf("%s: an invisible user's activity reached others: %v", ch, p[0].presence)
		}
	}
}

// An invisible user's activity changes reach their own devices and nobody else - no
// frame at all on a friend's or a space's channel, which a real offline user never causes
// either, and nothing to other instances. Through op 9, as a client sends it.
func TestInvisibleActivityChangeStaysWithSelf(t *testing.T) {
	store := NewMemoryActivityStore()
	useActivityStore(t, store)
	rdb, rec := recordingRedis(t)
	users := &fakeUsers{byID: map[int64]*auth.User{1: activityUser("invisible", true)}}
	fed := &fakeFederator{}
	notifier := NewDefaultPresenceNotifier(users, rdb, "")
	notifier.SetFederator(fed)
	h := newActivityHub(t, notifier)
	c := newClient(h, nil, 1, 0)

	c.handleActivity(frame(playing("Minecraft", 0)))
	settle(t, c)
	c.handleActivity(frame(playing("Factorio", 0)))
	settle(t, c)
	c.handleActivity(frame(""))
	settle(t, c)

	for _, p := range rec.all() {
		if p.channel != "stargate:user:1" {
			t.Errorf("an invisible user's activity change published on %s", p.channel)
		}
	}
	if got := len(rec.on("stargate:user:1")); got != 3 {
		t.Errorf("their own devices heard %d changes, want 3", got)
	}
	if fed.count() != 0 {
		t.Errorf("%d federation sends for an invisible user", fed.count())
	}

	// Visible again: the same reports reach friends, spaces and other instances.
	users.mu.Lock()
	users.byID[1] = activityUser("online", true)
	users.mu.Unlock()
	c.handleActivity(frame(playing("Minecraft", 0)))
	settle(t, c)
	if len(rec.on("stargate:user:2")) != 1 || len(rec.on("stargate:space:100")) != 1 || fed.count() != 1 {
		t.Errorf("a visible change: friend %d, space %d, federation %d", len(rec.on("stargate:user:2")), len(rec.on("stargate:space:100")), fed.count())
	}

	// Not connected at all (a cleanup after the last socket left): nobody is told.
	users.mu.Lock()
	users.byID[1] = activityUser("online", false)
	users.mu.Unlock()
	before := len(rec.all())
	notifier.ActivityChanged(context.Background(), 1)
	if len(rec.all()) != before || fed.count() != 1 {
		t.Errorf("an offline user's change published %d frames, %d federation sends", len(rec.all())-before, fed.count()-1)
	}
}

// READY's own presence shows the user what their devices report, invisible included.
func TestSelfPresenceForReady(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryActivityStore()
	useActivityStore(t, store)
	_, _ = store.SetConn(ctx, 1, 11, auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: 1}, 1)
	if p := SelfPresence(ctx, activityUser("invisible", true)); p.Status != "invisible" || len(p.Activities) != 1 {
		t.Errorf("invisible: %+v", p)
	}
	if p := SelfPresence(ctx, activityUser("online", false)); p.Status != "offline" || len(p.Activities) != 0 {
		t.Errorf("offline: %+v", p)
	}

	// READY goes to a socket that is open, whatever the record says before OnConnect has
	// written it: never "offline", and with what the user is playing.
	for _, c := range []struct {
		status, want string
	}{{"online", "online"}, {"dnd", "dnd"}, {"invisible", "invisible"}, {"", "online"}} {
		u := activityUser(c.status, false)
		p := connectedSelfPresence(ctx, u)
		if p.Status != c.want || len(p.Activities) != 1 {
			t.Errorf("READY for a %q user the record has offline: %+v", c.status, p)
		}
		if u.Presence.Online {
			t.Error("building READY's presence changed the user")
		}
	}
}
