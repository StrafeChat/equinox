package pushgateway

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestLimiterBurstAndRefill(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(60, clock.Now) // one per second, bursts of 60

	for i := range 60 {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("request %d refused inside the burst", i)
		}
	}
	ok, wait := l.allow("k")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("61st request: ok=%v wait=%s, want refused with wait <= 1s", ok, wait)
	}
	if ok, _ := l.allow("other"); !ok {
		t.Fatal("keys must not share a bucket")
	}

	clock.Advance(500 * time.Millisecond)
	if ok, _ := l.allow("k"); ok {
		t.Fatal("half a token is not a token")
	}
	clock.Advance(600 * time.Millisecond)
	if ok, _ := l.allow("k"); !ok {
		t.Fatal("a refilled token must be spendable")
	}

	// Idle long enough and the bucket is full again - but never fuller than the burst.
	clock.Advance(time.Hour)
	for i := range 60 {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("after idling, request %d refused", i)
		}
	}
	if ok, _ := l.allow("k"); ok {
		t.Fatal("refill must cap at the burst")
	}
}

func TestLimiterSweepsAndBoundsMemory(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(10, clock.Now)
	l.maxKeys = 100
	for i := range 50 {
		l.allow(strconv.Itoa(i))
	}
	clock.Advance(2 * time.Minute) // every bucket refills completely
	l.allow("fresh")
	if n := len(l.buckets); n != 1 {
		t.Fatalf("%d buckets after a sweep, want only the fresh one", n)
	}

	// A flood of distinct keys never grows the map past its bound.
	for i := range 1000 {
		l.allow("flood" + strconv.Itoa(i))
	}
	if n := len(l.buckets); n > l.maxKeys {
		t.Fatalf("%d buckets, bound is %d", n, l.maxKeys)
	}
}

func TestSenderLimitDefault(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(senderPerMinute, clock.Now)
	for i := range senderPerMinute {
		if ok, _ := l.allow("vapid"); !ok {
			t.Fatalf("request %d refused below 20000/min", i)
		}
	}
	if ok, wait := l.allow("vapid"); ok || retryAfterSeconds(wait) != 1 {
		t.Fatalf("over 20000/min: ok=%v Retry-After=%d", ok, retryAfterSeconds(wait))
	}
}

func TestClientIP(t *testing.T) {
	pl, err := newProxyList([]string{"10.0.0.0/8", "192.0.2.10", "loopback"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, remote, xff, want string
	}{
		{"direct client, header ignored", "203.0.113.5:1000", "1.1.1.1", "203.0.113.5"},
		{"trusted proxy, single hop", "10.0.0.2:443", "198.51.100.9", "198.51.100.9"},
		{"chain of trusted proxies", "127.0.0.1:443", "198.51.100.9, 10.3.3.3, 192.0.2.10", "198.51.100.9"},
		{"spoofed prefix ignored", "10.0.0.2:443", "6.6.6.6, 198.51.100.9", "198.51.100.9"},
		{"garbage stops the walk", "10.0.0.2:443", "6.6.6.6, nonsense", "10.0.0.2"},
		{"trusted proxy without header", "10.0.0.2:443", "", "10.0.0.2"},
		{"ipv4-mapped peer", "[::ffff:10.0.0.2]:443", "198.51.100.9", "198.51.100.9"},
		{"ipv6 client", "[::1]:443", "2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/", nil)
		req.RemoteAddr = c.remote
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := pl.clientIP(req); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}

	none, _ := newProxyList(nil)
	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := none.clientIP(req); got != "127.0.0.1" {
		t.Errorf("no trusted proxies: got %s, want the peer", got)
	}

	if _, err := newProxyList([]string{"not-an-ip"}); err == nil {
		t.Error("an invalid proxy entry must be refused")
	}
}
