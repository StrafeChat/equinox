package stargate

import (
	"testing"
	"time"
)

// A client gets a burst of authorizer-backed subscribes and a trickle after it; the bucket
// refills with time, so a legitimate client that hit the ceiling recovers.
func TestAllowSubscribe(t *testing.T) {
	c := &Client{}
	for i := 0; i < subBurst; i++ {
		if !c.allowSubscribe() {
			t.Fatalf("subscribe %d of the burst refused", i+1)
		}
	}
	if c.allowSubscribe() {
		t.Fatal("subscribe past the burst allowed")
	}
	// Wind the clock back as if 1.5 s had passed: 15 tokens come back, not more.
	c.subRefilled = c.subRefilled.Add(-15 * subRefillInterval)
	for i := 0; i < 15; i++ {
		if !c.allowSubscribe() {
			t.Fatalf("refilled subscribe %d refused", i+1)
		}
	}
	if c.allowSubscribe() {
		t.Fatal("more tokens than the refill interval granted")
	}
	// A long idle period never exceeds the burst.
	c.subRefilled = c.subRefilled.Add(-time.Hour)
	n := 0
	for c.allowSubscribe() {
		n++
		if n > subBurst {
			break
		}
	}
	if n != subBurst {
		t.Fatalf("after an idle hour %d subscribes allowed, want %d", n, subBurst)
	}
}
