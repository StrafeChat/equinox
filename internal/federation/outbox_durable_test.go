package federation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeOutboxRepo is the outbox slice of Repository, in memory, in seq order per peer.
type fakeOutboxRepo struct {
	Repository
	mu      sync.Mutex
	entries map[string][]OutboxEntry
	marks   map[string]string
	puts    int
}

func newFakeOutboxRepo() *fakeOutboxRepo {
	return &fakeOutboxRepo{entries: map[string][]OutboxEntry{}, marks: map[string]string{}}
}

func (r *fakeOutboxRepo) PutOutbox(_ context.Context, e *OutboxEntry, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.puts++
	list := append(r.entries[e.Domain], *e)
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	r.entries[e.Domain] = list
	return nil
}

func (r *fakeOutboxRepo) ListOutbox(_ context.Context, domain string, limit int) ([]OutboxEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.entries[domain]
	if len(list) > limit {
		list = list[:limit]
	}
	return append([]OutboxEntry(nil), list...), nil
}

func (r *fakeOutboxRepo) DeleteOutbox(_ context.Context, domain string, seq int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.entries[domain]
	for i := range list {
		if list[i].Seq == seq {
			r.entries[domain] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	return nil
}

func (r *fakeOutboxRepo) ListOutboxDomains(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for d, list := range r.entries {
		if len(list) > 0 {
			out = append(out, d)
		}
	}
	return out, nil
}

func (r *fakeOutboxRepo) MarkPeerFailure(_ context.Context, domain, msg string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marks[domain] = msg
	return nil
}

func (r *fakeOutboxRepo) ClearPeerFailure(_ context.Context, domain string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.marks, domain)
	return nil
}

func (r *fakeOutboxRepo) pending(domain string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries[domain])
}

func (r *fakeOutboxRepo) marked(domain string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.marks[domain]
	return m, ok
}

// recorder is the deliver stub: it fails as scripted, then records what was sent.
type recorder struct {
	mu       sync.Mutex
	failures map[string]error // path → error for the first attempt at that path
	sent     []string
	calls    int
}

func (rec *recorder) deliver(_ context.Context, _, _, path string, _ any) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.calls++
	if err, ok := rec.failures[path]; ok {
		delete(rec.failures, path)
		return err
	}
	rec.sent = append(rec.sent, path)
	return nil
}

func (rec *recorder) paths() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.sent...)
}

func newOutboxService(repo Repository, rec *recorder) *Service {
	s := &Service{repo: repo, outQueues: map[string]chan func(){}}
	s.deliver = rec.deliver
	return s
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A relay the peer could not take right then is retried, and nothing queued behind it
// goes out before it: the peer sees the room before the message in it.
func TestOutboxRetriesTransientFailureInOrder(t *testing.T) {
	repo := newFakeOutboxRepo()
	rec := &recorder{failures: map[string]error{"/rooms": fmt.Errorf("%w: connection refused", ErrRemoteUnavailable)}}
	s := newOutboxService(repo, rec)
	s.startOutbox(context.Background())

	s.send("peer.example", http.MethodPost, "/rooms", map[string]string{"room": "1"})
	s.send("peer.example", http.MethodPost, "/rooms/messages", map[string]string{"message": "1"})
	s.send("peer.example", http.MethodPost, "/rooms/messages/delete", map[string]string{"message": "1"})

	waitUntil(t, "the peer to be marked failing", func() bool { _, ok := repo.marked("peer.example"); return ok })
	waitUntil(t, "all three relays", func() bool { return len(rec.paths()) == 3 })
	got := rec.paths()
	if got[0] != "/rooms" || got[1] != "/rooms/messages" || got[2] != "/rooms/messages/delete" {
		t.Fatalf("delivered in order %v", got)
	}
	waitUntil(t, "the outbox to drain", func() bool { return repo.pending("peer.example") == 0 })
	waitUntil(t, "the failure mark to clear", func() bool { _, ok := repo.marked("peer.example"); return !ok })
}

// A refusal the peer will repeat (a 4xx) is dropped so it cannot block what follows.
func TestOutboxDropsRefusedRelay(t *testing.T) {
	repo := newFakeOutboxRepo()
	rec := &recorder{failures: map[string]error{"/rooms/messages": &StatusError{Domain: "peer.example", Status: http.StatusNotFound, Body: "room not found"}}}
	s := newOutboxService(repo, rec)
	s.startOutbox(context.Background())

	s.send("peer.example", http.MethodPost, "/rooms/messages", map[string]string{"message": "1"})
	s.send("peer.example", http.MethodPost, "/relationships", map[string]string{"action": "request"})

	waitUntil(t, "the relay after the refused one", func() bool { return len(rec.paths()) == 1 })
	if got := rec.paths(); got[0] != "/relationships" {
		t.Fatalf("delivered %v, want the relationship relay only", got)
	}
	waitUntil(t, "the outbox to drain", func() bool { return repo.pending("peer.example") == 0 })
	if _, ok := repo.marked("peer.example"); ok {
		t.Fatal("a refusal is an answer: the peer must not be marked unreachable")
	}
}

// Entries left from before a restart go out when the outbox starts.
func TestOutboxResumesPendingEntriesOnStart(t *testing.T) {
	repo := newFakeOutboxRepo()
	_ = repo.PutOutbox(context.Background(), &OutboxEntry{Domain: "peer.example", Seq: 1, Method: http.MethodPost, Path: "/rooms", Body: `{"room":"1"}`}, outboxTTL)
	_ = repo.PutOutbox(context.Background(), &OutboxEntry{Domain: "other.example", Seq: 2, Method: http.MethodPost, Path: "/relationships", Body: `{}`}, outboxTTL)
	rec := &recorder{}
	s := newOutboxService(repo, rec)
	s.startOutbox(context.Background())
	waitUntil(t, "both resumed relays", func() bool { return len(rec.paths()) == 2 })
}

// Typing, presence and call state are sent once, from memory, never stored.
func TestEphemeralRelaysSkipTheOutbox(t *testing.T) {
	repo := newFakeOutboxRepo()
	rec := &recorder{}
	s := newOutboxService(repo, rec)
	s.startOutbox(context.Background())
	s.send("peer.example", http.MethodPost, "/users/presence", map[string]string{"status": "online"})
	s.send("peer.example", http.MethodPost, "/rooms/typing", map[string]string{"user": "x"})
	s.send("peer.example", http.MethodPost, "/rooms/voice/self", map[string]string{"user": "x"})
	waitUntil(t, "the three ephemeral relays", func() bool { return len(rec.paths()) == 3 })
	repo.mu.Lock()
	puts := repo.puts
	repo.mu.Unlock()
	if puts != 0 {
		t.Fatalf("%d ephemeral relays were written to the outbox", puts)
	}
}

func TestPermanentRelayError(t *testing.T) {
	cases := []struct {
		err       error
		permanent bool
	}{
		{&StatusError{Status: http.StatusNotFound}, true},
		{&StatusError{Status: http.StatusForbidden}, true},
		{&StatusError{Status: http.StatusBadRequest}, true},
		{&StatusError{Status: http.StatusTooManyRequests}, false},
		{&StatusError{Status: http.StatusRequestTimeout}, false},
		{&StatusError{Status: http.StatusBadGateway}, false},
		{&StatusError{Status: http.StatusInternalServerError}, false},
		{fmt.Errorf("%w: dial tcp: connection refused", ErrRemoteUnavailable), false},
		{ErrPeerNotAllowed, true},
		{errors.New("something else"), false},
	}
	for _, c := range cases {
		if got := permanentRelayError(c.err); got != c.permanent {
			t.Errorf("permanentRelayError(%v) = %v, want %v", c.err, got, c.permanent)
		}
	}
}

func TestEphemeralPath(t *testing.T) {
	for _, p := range []string{"/rooms/typing", "/users/presence", "/rooms/voice/self", "/rooms/voice/ring"} {
		if !ephemeralPath(p) {
			t.Errorf("%s should be ephemeral", p)
		}
	}
	for _, p := range []string{"/rooms", "/rooms/messages", "/relationships", "/spaces/members", "/users/update", "/rooms/reactions"} {
		if ephemeralPath(p) {
			t.Errorf("%s must be durable", p)
		}
	}
}
