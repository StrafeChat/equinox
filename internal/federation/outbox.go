package federation

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/safego"
)

// A relay is made durable before it is sent: it is written to federation_outbox and
// delivered by that peer's worker, which retries with backoff until the peer takes it,
// refuses it for good, or the entry expires. A peer's entries go out one at a time in
// order, so a message never overtakes the room announce it depends on, and a peer that
// was down gets everything it missed when it comes back - which is what lets a mirror on
// an instance that was briefly unreachable converge without a resync.
//
// Typing, presence and call state are the exception (ephemeralPath): they describe the
// moment and are superseded within seconds, so they are sent once, from memory, and
// dropped if the peer cannot be reached right then.
//
// One process drains the outbox - the API, which calls startOutbox. The gateway writes
// entries too (it relays presence, which is ephemeral, but nothing assumes it stays that
// way) and wakes the API over Redis. Running several API replicas would deliver the same
// entry from each; every receiver is idempotent for messages, rooms and reactions, but a
// per-entry claim belongs in front of that step.

const (
	outboxTTL         = 7 * 24 * time.Hour // an entry nobody could deliver in this time is dropped
	outboxBatch       = 50
	outboxMinDelay    = time.Second
	outboxMaxDelay    = 30 * time.Second
	outboxPoll        = 2 * time.Minute // an idle worker looks anyway, for entries another process wrote
	outboxSendTimeout = 30 * time.Second
)

// ephemeralPath reports whether a relay to this path is worth nothing once stale.
func ephemeralPath(path string) bool {
	return path == "/rooms/typing" || path == "/users/presence" || strings.HasPrefix(path, "/rooms/voice/")
}

type peerOutbox struct {
	wake chan struct{}
}

// send relays one request to a peer. Federation is fire-and-forget from the sender's
// point of view: the local write already succeeded, and a peer being slow or down must
// never fail or delay the user's own request.
func (s *Service) send(domain, method, path string, body any) {
	if ephemeralPath(path) {
		s.enqueue(domain, func() { s.deliverOnce(domain, method, path, body) })
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		logger.Err("federation", err, map[string]any{"peer": domain, "path": path})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e := &OutboxEntry{Domain: domain, Seq: id.Next(), Method: method, Path: path, Body: string(raw), CreatedAt: time.Now().UTC()}
	if err := s.repo.PutOutbox(ctx, e, outboxTTL); err != nil {
		// The store is the durable part; without it the relay is still worth one try.
		logger.Err("federation", err, map[string]any{"peer": domain, "path": path, "stage": "outbox"})
		s.enqueue(domain, func() { s.deliverOnce(domain, method, path, json.RawMessage(raw)) })
		return
	}
	s.wakeOutbox(domain)
}

// deliverWith is the one call that hands a relay to a peer: the signed client.
func (s *Service) deliverWith(ctx context.Context, domain, method, path string, body any) error {
	_, err := s.client.Do(ctx, domain, method, path, body, nil)
	return err
}

// deliverOnce sends an ephemeral relay: one attempt, from memory.
func (s *Service) deliverOnce(domain, method, path string, body any) {
	ctx, cancel := context.WithTimeout(context.Background(), outboxSendTimeout)
	defer cancel()
	if err := s.deliver(ctx, domain, method, path, body); err != nil {
		logger.Err("federation", err, map[string]any{"peer": domain, "method": method, "path": path})
		return
	}
	s.touchPeer(ctx, domain)
}

func (s *Service) outboxChannel() string {
	return s.cfg.Database.Redis.CachePrefix + "federation:outbox"
}

// wakeOutbox tells the peer's worker there is something to send, starting the worker if
// this is the first relay to that peer since the process started. A process that runs
// no workers (the gateway) tells the API over Redis instead.
func (s *Service) wakeOutbox(domain string) {
	s.outMu.Lock()
	if !s.outboxOn {
		s.outMu.Unlock()
		s.notifyOutbox(domain)
		return
	}
	if s.outboxes == nil {
		s.outboxes = map[string]*peerOutbox{}
	}
	ob, ok := s.outboxes[domain]
	if !ok {
		ob = &peerOutbox{wake: make(chan struct{}, 1)}
		s.outboxes[domain] = ob
		ctx := s.outboxCtx
		if ctx == nil {
			ctx = context.Background()
		}
		safego.Go("federation", func() { s.runOutbox(ctx, domain, ob) })
	}
	s.outMu.Unlock()
	select {
	case ob.wake <- struct{}{}:
	default:
	}
}

func (s *Service) notifyOutbox(domain string) {
	if s.redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.redis.Publish(ctx, s.outboxChannel(), domain).Err(); err != nil {
		logger.Err("federation", err, map[string]any{"peer": domain, "stage": "outbox wake"})
	}
}

// startOutbox (API only) resumes delivery to every peer with entries waiting and listens
// for wake-ups from other processes. Called before the API serves requests, so what was
// queued when the process last stopped goes out ahead of anything new.
func (s *Service) startOutbox(ctx context.Context) {
	s.outMu.Lock()
	s.outboxOn = true
	s.outboxCtx = ctx
	s.outMu.Unlock()
	domains, err := s.repo.ListOutboxDomains(ctx)
	if err != nil {
		logger.Err("federation", err, map[string]any{"stage": "outbox"})
	}
	for _, d := range domains {
		s.wakeOutbox(d)
	}
	if len(domains) > 0 {
		logger.Info("federation", "resuming relays to %d peer(s)", len(domains))
	}
	if s.redis == nil {
		return
	}
	sub := s.redis.Subscribe(ctx, s.outboxChannel())
	safego.Go("federation", func() {
		defer sub.Close()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				s.wakeOutbox(msg.Payload)
			}
		}
	})
}

// runOutbox is one peer's worker: send what is queued, oldest first; on a failure that
// may pass, wait (longer each time, up to outboxMaxDelay) and start from the head again,
// because the failed entry must still go before the ones after it.
func (s *Service) runOutbox(ctx context.Context, domain string, ob *peerOutbox) {
	delay := outboxMinDelay
	idle := time.NewTimer(outboxPoll)
	defer idle.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		entries, err := s.repo.ListOutbox(ctx, domain, outboxBatch)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": domain, "stage": "outbox"})
			if !sleepCtx(ctx, jitter(delay)) {
				return
			}
			delay = nextDelay(delay)
			continue
		}
		if len(entries) == 0 {
			delay = outboxMinDelay
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(outboxPoll)
			select {
			case <-ctx.Done():
				return
			case <-ob.wake:
			case <-idle.C:
			}
			continue
		}
		for i := range entries {
			e := &entries[i]
			err := s.deliverEntry(ctx, domain, e)
			if err == nil {
				s.finishEntry(ctx, domain, e, nil)
				delay = outboxMinDelay
				continue
			}
			if permanentRelayError(err) {
				s.finishEntry(ctx, domain, e, err)
				continue
			}
			s.peerFailing(ctx, domain, err)
			logger.Warn("federation", "relay %s %s to %s failed: %v - retrying in %s", e.Method, e.Path, domain, err, jitter(delay).Round(time.Second))
			if !sleepCtx(ctx, jitter(delay)) {
				return
			}
			delay = nextDelay(delay)
			break
		}
	}
}

func (s *Service) deliverEntry(ctx context.Context, domain string, e *OutboxEntry) error {
	dctx, cancel := context.WithTimeout(ctx, outboxSendTimeout)
	defer cancel()
	return s.deliver(dctx, domain, e.Method, e.Path, json.RawMessage(e.Body))
}

// finishEntry removes an entry the peer took, or one it refused for good (which still
// means the peer answered).
func (s *Service) finishEntry(ctx context.Context, domain string, e *OutboxEntry, refused error) {
	if refused != nil {
		logger.Warn("federation", "relay %s %s to %s dropped: %v", e.Method, e.Path, domain, refused)
	}
	s.peerRecovered(ctx, domain)
	if err := s.repo.DeleteOutbox(ctx, domain, e.Seq); err != nil {
		logger.Err("federation", err, map[string]any{"peer": domain, "seq": e.Seq, "stage": "outbox"})
	}
}

// permanentRelayError: the peer answered and will not take this request, so retrying
// would only repeat the refusal. Everything else - unreachable, timed out, a 5xx, a rate
// limit - may pass later.
func permanentRelayError(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
			return false
		}
		return se.Status >= 400 && se.Status < 500
	}
	return errors.Is(err, ErrPeerNotAllowed)
}

func nextDelay(d time.Duration) time.Duration {
	d *= 2
	if d > outboxMaxDelay {
		d = outboxMaxDelay
	}
	return d
}

// jitter spreads retries so peers that went down together do not all knock at once.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// peerFailing records the first failure of an outage on the peer row, where
// GET /federation/peers shows it.
func (s *Service) peerFailing(ctx context.Context, domain string, err error) {
	s.outMu.Lock()
	if s.failing == nil {
		s.failing = map[string]bool{}
	}
	already := s.failing[domain]
	s.failing[domain] = true
	s.outMu.Unlock()
	if already {
		return
	}
	if e := s.repo.MarkPeerFailure(ctx, domain, err.Error(), time.Now().UTC()); e != nil {
		logger.Err("federation", e, map[string]any{"peer": domain, "stage": "outbox"})
	}
}

// peerRecovered clears the failure mark after the peer answered again.
func (s *Service) peerRecovered(ctx context.Context, domain string) {
	s.outMu.Lock()
	was := s.failing[domain]
	delete(s.failing, domain)
	s.outMu.Unlock()
	if was {
		if e := s.repo.ClearPeerFailure(ctx, domain); e != nil {
			logger.Err("federation", e, map[string]any{"peer": domain, "stage": "outbox"})
		}
		logger.Info("federation", "peer %s is reachable again", domain)
	}
	s.touchPeer(ctx, domain)
}
