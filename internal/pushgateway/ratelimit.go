package pushgateway

import (
	"math"
	"sync"
	"time"
)

// limiter is a keyed token bucket: each key may spend `perMinute` requests per minute,
// with up to a minute's worth in one burst.
//
// The state is in memory on purpose - the gateway is stateless and has no Redis. A
// deployment with several replicas therefore limits per replica (N replicas admit up to
// N times the configured rate); that is fine for what these limits are for, which is
// stopping one runaway sender or registrant from monopolising the gateway, not metering.
type limiter struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	burst     float64
	buckets   map[string]*bucket
	maxKeys   int
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// limiterMaxKeys bounds the memory a flood of distinct keys (fresh VAPID keys, say) can
// pin. Full buckets are dropped first; only if that is not enough is the map reset, which
// briefly forgives everyone rather than letting the process grow without bound.
const limiterMaxKeys = 200_000

func newLimiter(perMinute int, now func() time.Time) *limiter {
	return &limiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(perMinute),
		buckets: make(map[string]*bucket),
		maxKeys: limiterMaxKeys,
		now:     now,
	}
}

// allow spends one token for key. When none is left it reports how long until one is.
func (l *limiter) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) > time.Minute || len(l.buckets) >= l.maxKeys {
		l.sweep(now)
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// sweep forgets buckets that have refilled completely: they are indistinguishable from a
// key never seen before.
func (l *limiter) sweep(now time.Time) {
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.maxKeys {
		clear(l.buckets)
	}
}

// retryAfterSeconds renders a wait as a Retry-After value: whole seconds, at least 1.
func retryAfterSeconds(d time.Duration) int {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		s = 1
	}
	return s
}
