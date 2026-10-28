package stargate

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/safego"
)

// Op 9 (ACTIVITY): a connection says what the user is doing right now, or that they
// stopped. The read loop only parses and validates; the Redis write and the fan-out run on
// the connection's one worker, in the order the reports arrived, so a set followed by a
// clear can never land as a set.

const (
	// A desktop reports when the game changes, not on a timer; this allows a burst (a
	// reconnect, a quick game switch) and then one report every 12 s. Over budget, the latest
	// report waits for the next token instead of being dropped: the desktop only reports on
	// a change, so a lost clear would leave "Playing X" up for hours.
	activityBurst          = 5
	activityRefillInterval = 12 * time.Second
	// activityWriteTimeout bounds one report's Redis write. It must stay well under
	// presenceGrace: the disconnect cleanup runs a grace period after the socket closed, and
	// a write still in flight then could put the dead connection's entry back.
	activityWriteTimeout = 3 * time.Second
	// activityNotifyTimeout bounds the fan-out of a change (the user read, the publishes,
	// working out which instances to tell). It runs after the write, so it needs no such
	// bound, but the connection's next report waits for it.
	activityNotifyTimeout = 5 * time.Second
	// A write that failed is tried again after activityRetryDelay, doubling to
	// activityRetryMax - each try still costs a token, so a Redis outage costs a connection
	// one write per token interval once its burst is spent. The desktop reports a game once,
	// so a report that is not retried is lost until the game changes.
	activityRetryDelay = time.Second
	activityRetryMax   = 2 * time.Second
	// While a connection holds an activity its entry is rewritten every
	// activityKeepaliveInterval, well inside the store's 12 h expiry (a game can run for
	// longer than that), and every activityRelayTicks-th time the user's presence is relayed
	// to other instances again, which keep a relayed activity for 12 h too.
	activityKeepaliveInterval = time.Hour
	activityRelayTicks        = 4

	errInvalidActivity = 4007
)

// ActivityNotifier is told when a user's effective activity changed, to publish it. Hubs
// type-assert their PresenceNotifier to it (DefaultPresenceNotifier implements it), so the
// PresenceNotifier interface and HubConfig stay as they are.
type ActivityNotifier interface {
	ActivityChanged(ctx context.Context, userID int64)
}

// ActivityRelayer sends a user's presence, activity included, to other instances again
// without telling anyone here: the keepalive that stops another instance's copy of a long
// game from expiring. Optional, like ActivityNotifier.
type ActivityRelayer interface {
	RelayActivity(ctx context.Context, userID int64)
}

// activityReport is one accepted op 9: the activity (nil clears) and when it arrived.
type activityReport struct {
	act *auth.Activity
	at  int64 // server receive time, unix ms
	// shown: this connection had reported something before this report. A clear of it that
	// changes nothing in the store means the entry lapsed, and is published anyway.
	shown bool
}

// clientActivity is one connection's op 9 state, guarded by mu.
type clientActivity struct {
	mu sync.Mutex
	// token bucket for applied reports
	tokens   int
	refilled time.Time
	// pending is the latest report not yet applied: one slot, a newer report replaces it.
	pending *activityReport
	// current is what the store holds - or is being written to hold - for this connection,
	// reported at currentAt; stale means a write failed, so it may not.
	current   *auth.Activity
	currentAt int64
	stale     bool
	// retries counts the failed writes in a row, for the retry backoff.
	retries int
	// dirty: the store may hold an entry for this connection, so its disconnect must clear it.
	dirty bool
	// running: the worker goroutine is applying reports.
	running bool
	// timer is armed while over budget, or after a failed write, and starts the worker when
	// it fires.
	timer *time.Timer
	// keepalive ticks while current is stored (keepaliveGen tells a stopped chain's last
	// tick that it is stale); keepaliveDue and relayDue are a tick's work for the worker.
	keepalive      *time.Timer
	keepaliveGen   int
	keepaliveTicks int
	keepaliveDue   bool
	relayDue       bool
	// closed: the socket is gone; nothing is written for it any more.
	closed bool
}

// takeTokenLocked refills the bucket and takes one token if there is one.
func (a *clientActivity) takeTokenLocked(now time.Time, every time.Duration) bool {
	if a.refilled.IsZero() {
		a.tokens, a.refilled = activityBurst, now
	}
	if n := int(now.Sub(a.refilled) / every); n > 0 {
		a.tokens += n
		a.refilled = a.refilled.Add(time.Duration(n) * every)
		if a.tokens >= activityBurst {
			a.tokens, a.refilled = activityBurst, now
		}
	}
	if a.tokens <= 0 {
		return false
	}
	a.tokens--
	return true
}

// untilTokenLocked is how long until the next token accrues.
func (a *clientActivity) untilTokenLocked(now time.Time, every time.Duration) time.Duration {
	if d := every - now.Sub(a.refilled); d > 10*time.Millisecond {
		return d
	}
	return 10 * time.Millisecond
}

// parseActivityFrame reads op 9's d. A missing or null d, a missing or null activities and
// an empty list all clear. startedNow is true when the report gave no usable start of its
// own (0, or a future time clamped to now).
func parseActivityFrame(raw []byte, now time.Time) (act *auth.Activity, startedNow bool, err error) {
	var frame struct {
		D json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return nil, false, auth.ErrInvalidActivity
	}
	d := bytes.TrimSpace(frame.D)
	if len(d) == 0 || string(d) == "null" {
		return nil, false, nil
	}
	if d[0] != '{' {
		return nil, false, auth.ErrInvalidActivity
	}
	var p ActivityPayload
	if err := json.Unmarshal(d, &p); err != nil {
		return nil, false, auth.ErrInvalidActivity
	}
	acts, err := auth.NormalizeActivities(p.Activities, now)
	if err != nil {
		return nil, false, err
	}
	if len(acts) == 0 {
		return nil, false, nil
	}
	reported := p.Activities[0].StartedAt
	return &acts[0], reported == 0 || reported > now.UnixMilli(), nil
}

// handleActivity is op 9 on the read loop: validate, then hand to the worker. An invalid
// report is answered 4007 and the socket stays open; an over-budget one is not answered.
func (c *Client) handleActivity(raw []byte) {
	now := time.Now()
	act, startedNow, err := parseActivityFrame(raw, now)
	if err != nil {
		c.sendError(errInvalidActivity, "invalid activity")
		return
	}
	c.queueActivity(activityReport{act: act, at: now.UnixMilli()}, startedNow)
}

func (c *Client) activityRefill() time.Duration {
	if c.hub != nil && c.hub.activityRefill > 0 {
		return c.hub.activityRefill
	}
	return activityRefillInterval
}

// activityRetryAfter is the wait before retry n (1, 2, ...) of a write that keeps failing:
// activityRetryDelay, doubling up to activityRetryMax.
func (c *Client) activityRetryAfter(n int) time.Duration {
	base, limit := activityRetryDelay, activityRetryMax
	if c.hub != nil && c.hub.activityRetry > 0 {
		base, limit = c.hub.activityRetry, c.hub.activityRetry*(activityRetryMax/activityRetryDelay)
	}
	d := base
	for i := 1; i < n && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

func (c *Client) activityKeepaliveEvery() time.Duration {
	if c.hub != nil && c.hub.activityKeepalive > 0 {
		return c.hub.activityKeepalive
	}
	return activityKeepaliveInterval
}

// queueActivity puts a report in the pending slot unless it changes nothing, and starts the
// worker when it is idle and not waiting for a token.
func (c *Client) queueActivity(r activityReport, startedNow bool) {
	a := &c.act
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	latest := a.current
	if a.pending != nil {
		latest = a.pending.act
	}
	// The same game reported again without a start of its own keeps the one already shown:
	// "now" would restart its clock on every repeat.
	if startedNow && r.act != nil && latest != nil && latest.Type == r.act.Type && latest.Name == r.act.Name {
		r.act.StartedAt = latest.StartedAt
	}
	if a.pending != nil && auth.SameActivity(a.pending.act, r.act) {
		return // already on its way
	}
	if !a.stale && auth.SameActivity(a.current, r.act) {
		// What the store already holds for this connection (or is writing): an identical
		// report publishes nothing, and one that undoes a report still waiting for a token
		// takes that one back.
		a.pending = nil
		return
	}
	a.pending = &r
	c.kickActivityLocked()
}

// kickActivityLocked starts the worker if there is work, it is not running, and it is not
// waiting for a token or a retry (the timer will start it). Caller holds c.act.mu.
func (c *Client) kickActivityLocked() {
	a := &c.act
	if a.closed || a.running || a.timer != nil || (a.pending == nil && !a.keepaliveDue) {
		return
	}
	a.running = true
	safego.Go("stargate", c.activityWorker)
}

// activityTokenReady is the over-budget (or retry) timer: the worker may go on.
func (c *Client) activityTokenReady() {
	a := &c.act
	a.mu.Lock()
	defer a.mu.Unlock()
	a.timer = nil
	c.kickActivityLocked()
}

// activityWorker applies this connection's work one item at a time, in order: a
// keepalive tick, then pending reports, each report costing a token, until there is
// nothing left, the bucket is empty or a write failed (then it arms the timer and stops),
// or the socket is gone.
func (c *Client) activityWorker() {
	a := &c.act
	for {
		a.mu.Lock()
		if a.closed {
			a.running = false
			a.mu.Unlock()
			return
		}
		if a.keepaliveDue {
			relay := a.relayDue
			a.keepaliveDue, a.relayDue = false, false
			var act auth.Activity
			keep := a.current != nil && !a.stale
			if keep {
				act = *a.current
			}
			at := a.currentAt
			a.mu.Unlock()
			if keep {
				safego.Run("stargate", func() { c.keepActivity(act, at, relay) })
			}
			continue
		}
		if a.pending == nil {
			a.running = false
			a.mu.Unlock()
			return
		}
		now := time.Now()
		every := c.activityRefill()
		if !a.takeTokenLocked(now, every) {
			a.running = false
			a.timer = time.AfterFunc(a.untilTokenLocked(now, every), c.activityTokenReady)
			a.mu.Unlock()
			return
		}
		r := *a.pending
		a.pending = nil
		r.shown = r.shown || a.current != nil
		a.current, a.currentAt, a.stale = r.act, r.at, false
		if r.act != nil {
			a.dirty = true
		}
		firstTry := a.retries == 0
		a.mu.Unlock()

		ok := false
		safego.Run("stargate", func() { ok = c.applyActivity(r, firstTry) })

		a.mu.Lock()
		if !ok {
			a.stale = true
			if !a.closed && a.pending == nil {
				// Nothing newer came in: the same report again, after a short wait.
				a.retries++
				a.pending = &r
				a.running = false
				a.timer = time.AfterFunc(c.activityRetryAfter(a.retries), c.activityTokenReady)
				a.mu.Unlock()
				return
			}
		} else {
			a.retries = 0
			if r.act == nil {
				a.dirty = false
				c.stopKeepaliveLocked()
			} else {
				c.startKeepaliveLocked()
			}
		}
		a.mu.Unlock()
	}
}

// applyActivity writes one report and, when the user's effective activity changed,
// publishes it. Reports whether the write landed. Game names are never logged.
func (c *Client) applyActivity(r activityReport, logFailure bool) bool {
	st := CurrentActivityStore()
	if st == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), activityWriteTimeout)
	var changed bool
	var err error
	if r.act == nil {
		changed, err = st.ClearConn(ctx, c.userID, c.connID)
	} else {
		changed, err = st.SetConn(ctx, c.userID, c.connID, *r.act, r.at)
	}
	cancel()
	if err != nil {
		if logFailure { // the first of a run; the retries would only repeat it
			logger.Warn("stargate", "activity write failed: user_id=%d: %v", c.userID, err)
		}
		return false
	}
	// A clear that changes nothing after this connection had reported a game: its entry had
	// lapsed (the 12 h expiry, or Redis lost it), but whoever was told about the game still
	// shows it, so the activity's end is published all the same.
	if !changed && r.act == nil && r.shown {
		changed = true
	}
	if changed && c.hub != nil {
		c.hub.activityChanged(c.userID)
	}
	return true
}

// startKeepaliveLocked starts the keepalive ticks unless they are already running.
// Caller holds c.act.mu.
func (c *Client) startKeepaliveLocked() {
	a := &c.act
	if a.closed || a.keepalive != nil {
		return
	}
	a.keepaliveGen++
	gen := a.keepaliveGen
	a.keepaliveTicks = 0
	a.keepalive = time.AfterFunc(c.activityKeepaliveEvery(), func() { c.activityKeepaliveTick(gen) })
}

// stopKeepaliveLocked stops the ticks and drops a tick's work not done yet. Caller holds
// c.act.mu.
func (c *Client) stopKeepaliveLocked() {
	a := &c.act
	if a.keepalive != nil {
		a.keepalive.Stop()
		a.keepalive = nil
	}
	a.keepaliveGen++ // a tick already on its way finds itself stale
	a.keepaliveDue, a.relayDue = false, false
}

// activityKeepaliveTick hands the worker a keepalive - and every activityRelayTicks-th
// time a relay - and arms the next tick.
func (c *Client) activityKeepaliveTick(gen int) {
	a := &c.act
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || gen != a.keepaliveGen {
		return
	}
	a.keepaliveTicks++
	a.keepaliveDue = true
	if a.keepaliveTicks%activityRelayTicks == 0 {
		a.relayDue = true
	}
	a.keepalive = time.AfterFunc(c.activityKeepaliveEvery(), func() { c.activityKeepaliveTick(gen) })
	c.kickActivityLocked()
}

// keepActivity is one keepalive, on the worker so it cannot overtake a report: the
// connection's entry is written again as it was (same activity, same report time), which
// renews the hash's expiry and puts the entry back if Redis lost it - published then, as
// the change it is - and, when relay is set, the user's presence goes to other instances
// again so their copy of the activity does not expire either.
func (c *Client) keepActivity(act auth.Activity, at int64, relay bool) {
	st := CurrentActivityStore()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), activityWriteTimeout)
	changed, err := st.SetConn(ctx, c.userID, c.connID, act, at)
	cancel()
	if err != nil {
		// The next tick tries again; the expiry leaves room for eleven more.
		logger.Warn("stargate", "activity keepalive failed: user_id=%d: %v", c.userID, err)
		return
	}
	if c.hub == nil {
		return
	}
	if changed {
		c.hub.activityChanged(c.userID) // relays as well
		return
	}
	if relay {
		c.hub.activityRelay(c.userID)
	}
}

// closeActivity marks the connection gone: pending work is dropped, the timers stopped,
// and nothing is written for it again. Reports whether the store may still hold an entry
// for it, which the hub then clears after the grace period.
func (c *Client) closeActivity() bool {
	a := &c.act
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.pending = nil
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	c.stopKeepaliveLocked()
	return a.dirty
}

// activityChanged hands a changed effective activity to the notifier, if it publishes them.
func (h *Hub) activityChanged(userID int64) {
	if n, ok := h.presenceNotify.(ActivityNotifier); ok {
		ctx, cancel := context.WithTimeout(context.Background(), activityNotifyTimeout)
		defer cancel()
		n.ActivityChanged(ctx, userID)
	}
}

// activityRelay hands a keepalive relay to the notifier, if it relays them.
func (h *Hub) activityRelay(userID int64) {
	if n, ok := h.presenceNotify.(ActivityRelayer); ok {
		ctx, cancel := context.WithTimeout(context.Background(), activityNotifyTimeout)
		defer cancel()
		n.RelayActivity(ctx, userID)
	}
}

// hasClient reports whether the user still has a socket on this hub.
func (h *Hub) hasClient(userID int64) bool {
	h.regMu.RLock()
	defer h.regMu.RUnlock()
	return h.hasAnyClientLocked(userID)
}

// scheduleActivityCleanup clears a closed connection's entry once the grace period is over,
// for a socket that was not the user's last (the user's last one is cleared by the offline
// timer instead, see Hub.unregister). A plain delete of that connection's own field is
// enough: nothing else ever writes it, and a reconnecting client reports under its new
// connection id - if it reported the same game, the user's effective activity does not
// change and nothing is published.
func (h *Hub) scheduleActivityCleanup(userID, connID int64) {
	h.activityCleanupAfter(h.grace, userID, connID, 1)
}

// activityCleanupAttempts: a failed cleanup is tried again, a grace period apart. An entry
// left behind would outlive the connection until the hash's 12 h expiry, and come back into
// view whenever the user's other connections stop reporting anything newer.
const activityCleanupAttempts = 3

// activityCleanupAfter clears a closed connection's entry after delay and, if that changed
// the user's activity, publishes it - only while the user still has a socket here. Once
// they have none the offline that follows (or has already gone out) says everything, with
// no activity, and a publish of what the user record says now - online, until the offline
// lands - could reach people, and other instances, after it.
func (h *Hub) activityCleanupAfter(delay time.Duration, userID, connID int64, attempt int) {
	time.AfterFunc(delay, func() {
		safego.Run("stargate", func() {
			changed, ok := h.clearConnActivity(userID, connID, attempt)
			if ok && changed && h.hasClient(userID) {
				h.activityChanged(userID)
			}
		})
	})
}

// clearConnActivity deletes a closed connection's entry, arranging another try when the
// delete fails. ok is false when it failed.
func (h *Hub) clearConnActivity(userID, connID int64, attempt int) (changed, ok bool) {
	st := CurrentActivityStore()
	if st == nil {
		return false, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), activityWriteTimeout)
	changed, err := st.ClearConn(ctx, userID, connID)
	cancel()
	if err != nil {
		logger.Warn("stargate", "activity cleanup failed (attempt %d): user_id=%d: %v", attempt, userID, err)
		if attempt < activityCleanupAttempts {
			h.activityCleanupAfter(max(h.grace, time.Second), userID, connID, attempt+1)
		}
		return false, false
	}
	return changed, true
}
