package push

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/safehttp"
)

const (
	sendTimeout = 10 * time.Second
	// maxResponseRead caps how much of a push service's answer is read: only the status
	// matters, and an endpoint is someone else's server.
	maxResponseRead = 4 << 10
	// maxRetryAfter caps how long a 429's Retry-After is honoured.
	maxRetryAfter = 60 * time.Second
	// deliveryBudget bounds one delivery including retries, so a job finishes well before
	// another API process may reclaim it from the stream (claimIdle).
	deliveryBudget = 45 * time.Second
)

// retryBackoff is the wait before each retry of a delivery that hit a 5xx or a network
// error; there are as many retries as entries.
var retryBackoff = []time.Duration{time.Second, 4 * time.Second, 15 * time.Second}

// Per-type delivery parameters (RFC 8030 §5.2-5.4). Urgency lets a phone's push service
// hold low-priority pushes for a battery-friendly moment; the gateway maps "high" to an
// alert and the rest to silent background pushes. A Topic replaces a still-undelivered
// push with the same topic, so a stale "ringing" never arrives after "stopped ringing".
type delivery struct {
	ttl     int
	urgency string
	topic   string
}

func deliveryFor(t string, roomID int64) delivery {
	rid := strconv.FormatInt(roomID, 10)
	switch t {
	case TypeMessage:
		return delivery{ttl: 86400, urgency: "high"}
	case TypeCall:
		return delivery{ttl: 45, urgency: "high", topic: "c" + rid}
	case TypeCallEnd:
		return delivery{ttl: 60, urgency: "normal", topic: "c" + rid}
	case TypeRead:
		return delivery{ttl: 3600, urgency: "low", topic: "r" + rid}
	default: // TypeTest
		return delivery{ttl: 300, urgency: "high"}
	}
}

// outcomeKind is how a delivery ended, which decides what happens to the subscription.
type outcomeKind int

const (
	delivered outcomeKind = iota
	// gone: the push service says the subscription no longer exists (404/410) - delete it.
	gone
	// rejected: the push service refused this request (400/403/413, an unexpected status) -
	// count a failure; a subscription refused five times running is deleted.
	rejected
	// unavailable: 5xx, 429 or network trouble outlasted the retries - count a failure but
	// never delete for it: a push service having a bad hour must not cost every device its
	// subscription.
	unavailable
	// refused: this instance will not send there (the endpoint no longer passes
	// PUSH_ALLOWED_HOSTS / the address checks) - delete it.
	refused
)

type outcome struct {
	kind   outcomeKind
	status int
	reason string
}

// Sender posts encrypted pushes.
type Sender struct {
	client  *http.Client
	vapid   *VAPID
	checker *endpointChecker
	// sleep waits between retries; replaceable in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewSender returns a sender whose connections only reach public addresses over HTTPS,
// unless PUSH_ALLOW_INSECURE (development) lifts both.
func NewSender(cfg config.PushConfig, vapid *VAPID) *Sender {
	dialer := &net.Dialer{Timeout: sendTimeout}
	if !cfg.AllowInsecure {
		dialer = safehttp.NewDialer(sendTimeout)
	}
	transport := &http.Transport{
		// No Proxy: an environment proxy would sit outside the dial-time address check.
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: sendTimeout,
		// Pushes concentrate on a few services (a gateway, a distributor), so keep
		// connections to them warm.
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Sender{
		client: &http.Client{
			Transport: transport,
			Timeout:   sendTimeout,
			// A push service answers; it never redirects. Following one would let an
			// endpoint bounce the request somewhere the subscribe-time checks never saw.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		vapid:   vapid,
		checker: newEndpointChecker(cfg),
		sleep:   sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Send encrypts payload to the subscription and delivers it, retrying transient failures
// (with retries) within the push's TTL and the delivery budget.
func (s *Sender) Send(ctx context.Context, sub *Subscription, payload []byte, d delivery, retries bool) outcome {
	endpoint, err := s.checker.parse(sub.Endpoint)
	if err != nil {
		return outcome{kind: refused, reason: "endpoint refused by this instance"}
	}
	uaPublic, authSecret, err := parseReceiverKeys(sub.P256dh, sub.Auth)
	if err != nil {
		return outcome{kind: refused, reason: "invalid subscription keys"}
	}
	body, err := encrypt(payload, uaPublic, authSecret)
	if err != nil {
		return outcome{kind: rejected, reason: "payload could not be encrypted"}
	}
	budget := min(time.Duration(d.ttl)*time.Second, deliveryBudget)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	for attempt := 0; ; attempt++ {
		status, retryAfter, err := s.post(ctx, endpoint, body, d)
		res := classify(status, err)
		if res.kind != unavailable || !retries || attempt >= len(retryBackoff) {
			return res
		}
		wait := retryBackoff[attempt]
		if status == http.StatusTooManyRequests && retryAfter > 0 {
			wait = min(retryAfter, maxRetryAfter)
		}
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
			return res
		}
		if s.sleep(ctx, wait) != nil {
			return res
		}
	}
}

func (s *Sender) post(ctx context.Context, endpoint *url.URL, body []byte, d delivery) (int, time.Duration, error) {
	authz, err := s.vapid.Authorization(endpoint)
	if err != nil {
		return 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(d.ttl))
	req.Header.Set("Urgency", d.urgency)
	if d.topic != "" {
		req.Header.Set("Topic", d.topic)
	}
	req.Header.Set("Authorization", authz)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseRead))
	return resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

// classify maps a push service's answer to what to do about the subscription.
func classify(status int, err error) outcome {
	if err != nil {
		reason := "network error"
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			reason = "timeout"
		}
		if errors.Is(err, safehttp.ErrBlockedAddress) {
			return outcome{kind: refused, reason: "endpoint resolves to a non-public address"}
		}
		return outcome{kind: unavailable, reason: reason}
	}
	reason := "http " + strconv.Itoa(status)
	switch {
	case status >= 200 && status < 300:
		return outcome{kind: delivered, status: status}
	case status == http.StatusNotFound || status == http.StatusGone:
		return outcome{kind: gone, status: status, reason: reason}
	case status == http.StatusTooManyRequests || status >= 500:
		return outcome{kind: unavailable, status: status, reason: reason}
	default:
		return outcome{kind: rejected, status: status, reason: reason}
	}
}

// parseRetryAfter reads delay-seconds or an HTTP date; zero when absent or unparseable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
