package federation

import (
	"sync"
	"testing"
	"time"
)

// Relays to one peer must run one at a time, in order - a message sent right after its
// room was created must not overtake the room announce.
func TestEnqueueRunsPeerJobsInOrderWithoutOverlap(t *testing.T) {
	s := &Service{outQueues: map[string]chan func(){}}
	var mu sync.Mutex
	var order []int
	running := 0
	overlapped := false
	done := make(chan struct{}, 3)
	for i := 1; i <= 3; i++ {
		i := i
		s.enqueue("peer.example", func() {
			mu.Lock()
			running++
			if running > 1 {
				overlapped = true
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			order = append(order, i)
			running--
			mu.Unlock()
			done <- struct{}{}
		})
	}
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("jobs did not run")
		}
	}
	if overlapped {
		t.Fatal("two relays to the same peer ran at once")
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("order = %v, want [1 2 3]", order)
	}
}

// Different peers get independent workers: a slow peer must not hold up another.
func TestEnqueueDoesNotSerialiseAcrossPeers(t *testing.T) {
	s := &Service{outQueues: map[string]chan func(){}}
	release := make(chan struct{})
	s.enqueue("slow.example", func() { <-release })
	fast := make(chan struct{})
	s.enqueue("fast.example", func() { close(fast) })
	select {
	case <-fast:
	case <-time.After(time.Second):
		t.Fatal("a relay to another peer waited behind a slow one")
	}
	close(release)
}

// A job that panics is logged and must not kill the peer's worker.
func TestEnqueueSurvivesPanickingJob(t *testing.T) {
	s := &Service{outQueues: map[string]chan func(){}}
	s.enqueue("peer.example", func() { panic("peer sent garbage") })
	after := make(chan struct{})
	s.enqueue("peer.example", func() { close(after) })
	select {
	case <-after:
	case <-time.After(time.Second):
		t.Fatal("worker died after a panicking job")
	}
}
