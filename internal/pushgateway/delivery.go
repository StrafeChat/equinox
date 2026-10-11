package pushgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Message is one Web Push message on its way to a device.
type Message struct {
	Reg *Registration
	// Body is the RFC 8291 aes128gcm body exactly as the instance sent it. The gateway
	// cannot read it and forwards it byte for byte (base64url) for the device to decrypt.
	Body []byte
	// TTL is in seconds, already capped to what the platforms accept.
	TTL int
	// Urgency is one of the RFC 8030 values, "normal" when the sender gave none.
	Urgency string
	// Topic is the RFC 8030 Topic, "" when none: a newer push with the same topic replaces
	// an undelivered older one (collapse key / collapse id).
	Topic string
}

// alert reports whether the message must wake the device with a visible notification.
// Only high-urgency messages do; everything else (read receipts clearing a notification,
// a ring that stopped) is delivered silently and handled in the background.
func (m *Message) alert() bool { return m.Urgency == "high" }

// transport delivers to one platform.
type transport interface {
	Send(ctx context.Context, m *Message) error
}

type deliveryKind int

const (
	// deliveryGone: the platform says the token will never work again. Answered with 410,
	// which tells the instance to delete the subscription.
	deliveryGone deliveryKind = iota + 1
	// deliveryRateLimited: the platform is throttling us; 429 with its Retry-After.
	deliveryRateLimited
	// deliveryTooLarge: the message cannot fit the platform's payload limit; 413.
	deliveryTooLarge
	// deliveryUpstream: the platform failed or refused for a reason that is not the
	// device's fault (outage, timeout, our credentials); 502, which the instance retries.
	deliveryUpstream
)

// DeliveryError is how transports report a failed delivery. Reason is a short, fixed
// vocabulary (an upstream error code or our own label), never a token or a body, so it is
// safe to log and to return to the sender.
type DeliveryError struct {
	Kind       deliveryKind
	Upstream   int // the platform's HTTP status, 0 when it never answered
	Reason     string
	RetryAfter time.Duration
}

func (e *DeliveryError) Error() string {
	return fmt.Sprintf("delivery failed: upstream %d %s", e.Upstream, e.Reason)
}

// httpStatus maps a delivery failure to the status the Web Push sender gets.
func (e *DeliveryError) httpStatus() int {
	switch e.Kind {
	case deliveryGone:
		return http.StatusGone
	case deliveryRateLimited:
		return http.StatusTooManyRequests
	case deliveryTooLarge:
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusBadGateway
	}
}

// maxUpstreamBody caps how much of a platform's response we read: error documents are a
// few hundred bytes, and nothing we need is ever larger.
const maxUpstreamBody = 64 << 10

func readUpstream(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, maxUpstreamBody))
	return b
}

// upstreamRetryAfter reads a platform's Retry-After (delta-seconds or an HTTP date),
// capped at a minute - the most a Web Push sender is expected to wait anyway.
func upstreamRetryAfter(h http.Header, now time.Time) time.Duration {
	const fallback, ceiling = 30 * time.Second, time.Minute
	v := h.Get("Retry-After")
	if v == "" {
		return fallback
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	} else {
		return fallback
	}
	return min(max(d, time.Second), ceiling)
}

func networkError(err error) error {
	reason := "network error"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "timeout"
	}
	return &DeliveryError{Kind: deliveryUpstream, Reason: reason}
}
