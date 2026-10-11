package push

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The producers and the stream workers need a real Redis (Lua, streams, consumer groups).
// They run when PUSH_TEST_REDIS names one, e.g. redis://localhost:6379/15 - a database
// the test may treat as its own: it refuses to run in one holding anything but push keys.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("PUSH_TEST_REDIS")
	if raw == "" {
		t.Skip("set PUSH_TEST_REDIS=redis://host:port/db to run the Redis-backed push tests")
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
		if len(k) < 5 || k[:5] != "push:" {
			t.Skipf("database holds %q - use an empty one", k)
		}
	}
	clean := func() {
		if ks, _ := rdb.Keys(ctx, "push:*").Result(); len(ks) > 0 {
			rdb.Del(ctx, ks...)
		}
	}
	clean()
	SetProducing(true)
	t.Cleanup(func() { clean(); SetProducing(false); _ = rdb.Close() })
	return rdb
}

func readJobs(t *testing.T, rdb *redis.Client) []job {
	t.Helper()
	entries, err := rdb.XRange(context.Background(), streamKey, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	var out []job
	for _, e := range entries {
		var j job
		if err := json.Unmarshal([]byte(e.Values["j"].(string)), &j); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	rdb.Del(context.Background(), streamKey)
	return out
}

func sorted(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func eq(a, b []int64) bool {
	a, b = sorted(a), sorted(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProducers(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	for _, uid := range []int64{2, 3, 5} {
		markSubscribed(ctx, rdb, uid)
	}

	// A message: only subscribed participants, never the sender; mentions narrowed to them;
	// no text for an E2EE room.
	EnqueueMessage(ctx, rdb, Message{RoomID: 9, RoomType: 3, SpaceID: 8, ID: 77, SenderID: 5, Text: "hello",
		Mentioned: []int64{2, 4, 6}}, []int64{1, 2, 3, 4, 5})
	EnqueueMessage(ctx, rdb, Message{RoomID: 9, RoomType: 1, E2EE: true, ID: 78, SenderID: 1, Text: "secret"}, []int64{1, 2})
	EnqueueMessage(ctx, rdb, Message{RoomID: 9, RoomType: 1, ID: 79, SenderID: 1}, []int64{1, 4}) // nobody to notify
	jobs := readJobs(t, rdb)
	if len(jobs) != 2 {
		t.Fatalf("%d jobs, want 2", len(jobs))
	}
	if j := jobs[0]; !eq(j.Users, []int64{2, 3}) || j.Message.Text != "hello" || !eq(j.Message.Mentioned, []int64{2}) || j.Space != 8 {
		t.Errorf("first job: %+v %+v", j, j.Message)
	}
	if j := jobs[1]; !eq(j.Users, []int64{2}) || j.Message.Text != "" || !j.Message.E2EE {
		t.Errorf("E2EE job carries text: %+v", j.Message)
	}

	// Read-sync only after a message push was delivered for the room, and only once.
	EnqueueRead(ctx, rdb, 2, 9, 3, 8, 77)
	if n := len(readJobs(t, rdb)); n != 0 {
		t.Errorf("read queued with nothing showing")
	}
	rdb.Set(ctx, sentKey(2, 9), "1", time.Minute)
	if !NotificationShowing(ctx, rdb, 2, 9) {
		t.Error("NotificationShowing false after a delivery")
	}
	EnqueueRead(ctx, rdb, 2, 9, 3, 8, 77)
	EnqueueRead(ctx, rdb, 2, 9, 3, 8, 78)
	jobs = readJobs(t, rdb)
	if len(jobs) != 1 || jobs[0].T != TypeRead || jobs[0].ReadTo != 77 || !eq(jobs[0].Users, []int64{2}) {
		t.Errorf("read jobs: %+v", jobs)
	}

	// Calls: a new call rings everyone; an answer stops one; a re-ring rings the rest
	// again; the end stops whoever was left. The caller is never rung.
	EnqueueCall(ctx, rdb, Call{RoomID: 9, RoomType: 2, CallerID: 1, Ringing: []int64{2, 3, 4}, RingAll: true})
	jobs = readJobs(t, rdb)
	if len(jobs) != 1 || jobs[0].T != TypeCall || !eq(jobs[0].Users, []int64{2, 3}) || jobs[0].Caller != 1 {
		t.Fatalf("call start: %+v", jobs)
	}
	EnqueueCall(ctx, rdb, Call{RoomID: 9, RoomType: 2, CallerID: 1, Ringing: []int64{3, 4}})
	jobs = readJobs(t, rdb)
	if len(jobs) != 1 || jobs[0].T != TypeCallEnd || !eq(jobs[0].Users, []int64{2}) {
		t.Fatalf("answered: %+v", jobs)
	}
	// The same state again (a mirror's relay of its own update) announces nothing.
	EnqueueCall(ctx, rdb, Call{RoomID: 9, RoomType: 2, CallerID: 1, Ringing: []int64{3, 4}})
	if jobs = readJobs(t, rdb); len(jobs) != 0 {
		t.Fatalf("repeat announced: %+v", jobs)
	}
	EnqueueCall(ctx, rdb, Call{RoomID: 9, RoomType: 2, CallerID: 1, Ringing: []int64{3, 4, 2}, RingAll: true})
	jobs = readJobs(t, rdb)
	if len(jobs) != 1 || jobs[0].T != TypeCall || !eq(jobs[0].Users, []int64{2, 3}) {
		t.Fatalf("re-ring: %+v", jobs)
	}
	EnqueueCallEnded(ctx, rdb, 9, 2)
	jobs = readJobs(t, rdb)
	if len(jobs) != 1 || jobs[0].T != TypeCallEnd || !eq(jobs[0].Users, []int64{2, 3}) {
		t.Fatalf("ended: %+v", jobs)
	}
	if n, _ := rdb.Exists(ctx, ringKey(9)).Result(); n != 0 {
		t.Error("ringing set left behind after the call ended")
	}

	// Producers are inert until switched on.
	SetProducing(false)
	EnqueueMessage(ctx, rdb, Message{RoomID: 9, RoomType: 3, ID: 80, SenderID: 5}, []int64{2})
	if jobs = readJobs(t, rdb); len(jobs) != 0 {
		t.Errorf("queued while switched off")
	}
	SetProducing(true)
}

// End to end through Redis: a queued message reaches the device through the stream
// workers, the job is removed from the stream, and the room is marked as showing a
// notification for read-sync.
func TestWorkersDeliverFromStream(t *testing.T) {
	rdb := testRedis(t)
	w := newWorld(t)
	w.svc.redis = rdb
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.svc.RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := rdb.SIsMember(ctx, usersKey, "2").Result(); !ok {
		t.Fatal("index not rebuilt from the subscriptions")
	}
	w.svc.StartWorkers(ctx)
	// Let the group exist before producing ("$" skips anything queued earlier).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if groups, err := rdb.XInfoGroups(ctx, streamKey).Result(); err == nil && len(groups) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("consumer group never created")
		}
		time.Sleep(20 * time.Millisecond)
	}
	EnqueueMessage(ctx, rdb, Message{RoomID: pmID, RoomType: roomTypePM, E2EE: true, ID: 4242, SenderID: alice}, []int64{alice, bob})
	for len(w.f.pushes()) == 0 {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("push never delivered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	p := w.devs[bob].open(t, w.f.pushes()[0])
	if p.T != TypeMessage || p.MessageID != "4242" || !p.Encrypted {
		t.Errorf("payload: %+v", p)
	}
	for {
		n, _ := rdb.XLen(ctx, streamKey).Result()
		if n == 0 {
			break
		}
		if time.Now().After(deadline.Add(10 * time.Second)) {
			t.Fatal("processed job left in the stream")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !NotificationShowing(ctx, rdb, bob, pmID) {
		t.Error("delivered message not marked for read-sync")
	}
}

// A job that takes longer than claimIdle must not look abandoned to the other API
// processes: the consumer working on it keeps renewing its claim, so XAUTOCLAIM elsewhere
// never hands it out a second time (which would deliver every push in it twice).
func TestKeepClaimedRenewsTheClaim(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	old := claimRefresh
	claimRefresh = 50 * time.Millisecond
	t.Cleanup(func() { claimRefresh = old })
	s := &Service{redis: rdb}
	if err := s.ensureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: streamKey, Values: map[string]interface{}{"j": "{}"}}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Group: groupName, Consumer: "a", Streams: []string{streamKey, ">"}, Count: 1}).Result(); err != nil {
		t.Fatal(err)
	}
	idle := func() time.Duration {
		pending, err := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{Stream: streamKey, Group: groupName, Start: id, End: id, Count: 1}).Result()
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending: %v %v", pending, err)
		}
		return pending[0].Idle
	}

	stop := s.keepClaimed("a", id)
	time.Sleep(400 * time.Millisecond)
	if d := idle(); d > 200*time.Millisecond {
		t.Errorf("claim not renewed while processing: idle %s", d)
	}
	// Another consumer asking for jobs idle longer than the renewal interval gets nothing.
	if msgs, _, err := rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: streamKey, Group: groupName, Consumer: "b", MinIdle: 300 * time.Millisecond, Start: "0-0", Count: 10}).Result(); err != nil || len(msgs) != 0 {
		t.Errorf("a job being processed was claimed elsewhere: %v %v", msgs, err)
	}
	stop()
	time.Sleep(300 * time.Millisecond)
	if d := idle(); d < 250*time.Millisecond {
		t.Errorf("claim still renewed after stop: idle %s", d)
	}
}
