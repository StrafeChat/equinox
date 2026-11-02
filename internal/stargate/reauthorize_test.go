package stargate

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// reauthFake answers CanAccessRoom from a table, counts the checks, and can hold the first
// one until released.
type reauthFake struct {
	mu      sync.Mutex
	answers map[string]error // nil = allowed, errRefused = refused, anything else = a failed read
	calls   map[string]int
	hold    chan struct{}
}

var errRefused = errors.New("refused")

func (f *reauthFake) CanAccessRoom(_ context.Context, _ int64, roomID string) (bool, error) {
	f.mu.Lock()
	f.calls[roomID]++
	first := f.calls[roomID] == 1
	hold := f.hold
	err := f.answers[roomID]
	f.mu.Unlock()
	if first && hold != nil {
		<-hold
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errRefused):
		return false, nil
	default:
		return false, err
	}
}

func (f *reauthFake) count(roomID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[roomID]
}

func reauthHub(t *testing.T, f *reauthFake, rooms ...string) (*Hub, *Client) {
	t.Helper()
	h := NewHubWithConfig(HubConfig{Authorizer: f})
	c := newClient(h, nil, 7, 1)
	h.regMu.Lock()
	h.clients[c] = struct{}{}
	h.regMu.Unlock()
	for _, r := range rooms {
		c.subscribe("space", r)
		h.subscribe(c, "space", r)
	}
	return h, c
}

// A permission change re-checks what a member holds. A refusal ends the subscription; a check
// that fails (a timeout under the load the change itself causes) is no lost permission, and the
// member would otherwise stop getting a room they can still see, with nothing telling the client.
func TestReauthorizeDropsOnlyWhatIsRefused(t *testing.T) {
	f := &reauthFake{answers: map[string]error{"2": errRefused, "3": errors.New("timeout")}, calls: map[string]int{}}
	h, c := reauthHub(t, f, "1", "2", "3")
	h.reauthorizeUser(7)
	got := c.listSubs()
	slices.Sort(got)
	if want := []string{"space:1", "space:3"}; !slices.Equal(got, want) {
		t.Fatalf("subscriptions %v, want %v", got, want)
	}
}

// One role change can send a member a SPACE_ROOM_DELETE per room. Each pass re-checks every
// subscription, so the deletes that arrive during a pass share one more pass, not one each.
func TestReauthorizeCoalescesABurst(t *testing.T) {
	f := &reauthFake{answers: map[string]error{}, calls: map[string]int{}, hold: make(chan struct{})}
	h, _ := reauthHub(t, f, "1")
	for i := 0; i < 20; i++ {
		h.queueReauthorize(7)
	}
	close(f.hold)
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.reauthMu.Lock()
		_, busy := h.reauthing[7]
		h.reauthMu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reauthorization never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := f.count("1"); n != 2 {
		t.Fatalf("%d passes, want 2 (the first, and one for everything that came during it)", n)
	}
}
