package pushgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPNsURL and DefaultAPNsSandboxURL are Apple's production and development
	// gateways; a registration's sandbox flag picks between them.
	DefaultAPNsURL        = "https://api.push.apple.com"
	DefaultAPNsSandboxURL = "https://api.sandbox.push.apple.com"
	// apnsMaxPayload is Apple's limit on a notification's JSON payload.
	apnsMaxPayload = 4096
	// apnsTokenRefresh: Apple rejects provider tokens older than an hour and throttles
	// providers that mint new ones more often than every 20 minutes; 40 sits between.
	apnsTokenRefresh = 40 * time.Minute
)

// apnsClient sends to APNs over HTTP/2 with a token-based (.p8) provider identity.
type apnsClient struct {
	cfg        *APNsConfig
	prodURL    string
	sandboxURL string
	http       *http.Client
	now        func() time.Time

	mu     sync.Mutex
	jwt    string
	issued time.Time
}

func newAPNsClient(cfg *APNsConfig, prodURL, sandboxURL string, client *http.Client, now func() time.Time) *apnsClient {
	return &apnsClient{
		cfg:        cfg,
		prodURL:    strings.TrimRight(prodURL, "/"),
		sandboxURL: strings.TrimRight(sandboxURL, "/"),
		http:       client,
		now:        now,
	}
}

// apnsPayload builds the notification JSON. A high-urgency message is an alert with
// mutable-content, so the Notification Service Extension can decrypt it and replace the
// fallback text before anything is shown; the fallback is only what appears if the
// extension cannot run in time. Everything else is a silent background push for the app.
func apnsPayload(m *Message) ([]byte, error) {
	aps := map[string]any{"content-available": 1}
	if m.alert() {
		fallback := m.Reg.Fallback
		if fallback == "" {
			fallback = defaultFallback
		}
		aps = map[string]any{
			"alert":           map[string]string{"body": fallback},
			"mutable-content": 1,
			"sound":           "default",
		}
	}
	return json.Marshal(map[string]any{"aps": aps, "s": m.Reg.Slot, "p": b64.EncodeToString(m.Body)})
}

func (c *apnsClient) Send(ctx context.Context, m *Message) error {
	payload, err := apnsPayload(m)
	if err != nil {
		return err
	}
	if len(payload) > apnsMaxPayload {
		return &DeliveryError{Kind: deliveryTooLarge, Reason: "exceeds APNs payload limit"}
	}
	base := c.prodURL
	if m.Reg.Sandbox {
		base = c.sandboxURL
	}
	endpoint := base + "/3/device/" + m.Reg.Token

	for attempt := 0; ; attempt++ {
		token, err := c.providerToken()
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		c.setHeaders(req.Header, m, token)
		resp, err := c.http.Do(req)
		if err != nil {
			return networkError(err)
		}
		body := readUpstream(resp.Body)
		resp.Body.Close()

		var reason struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(body, &reason)
		// Apple refused our provider token (expired, or the clock jumped): mint a fresh
		// one and try once more.
		if resp.StatusCode == http.StatusForbidden && attempt == 0 &&
			(reason.Reason == "ExpiredProviderToken" || reason.Reason == "InvalidProviderToken") {
			c.invalidate(token)
			continue
		}
		return classifyAPNs(resp.StatusCode, reason.Reason, resp.Header, c.now())
	}
}

func (c *apnsClient) setHeaders(h http.Header, m *Message, token string) {
	h.Set("Authorization", "bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("apns-topic", c.cfg.Topic)
	if m.alert() {
		h.Set("apns-push-type", "alert")
		h.Set("apns-priority", "10")
	} else {
		h.Set("apns-push-type", "background")
		h.Set("apns-priority", "5")
	}
	// 0 asks APNs to try once and not store the notification, matching Web Push TTL 0.
	expiration := int64(0)
	if m.TTL > 0 {
		expiration = c.now().Unix() + int64(m.TTL)
	}
	h.Set("apns-expiration", strconv.FormatInt(expiration, 10))
	if m.Topic != "" {
		h.Set("apns-collapse-id", m.Topic)
	}
}

// classifyAPNs turns APNs' answer into nil or a DeliveryError.
func classifyAPNs(status int, reason string, header http.Header, now time.Time) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusGone,
		status == http.StatusBadRequest && reason == "BadDeviceToken":
		if reason == "" {
			reason = "Unregistered"
		}
		return &DeliveryError{Kind: deliveryGone, Upstream: status, Reason: reason}
	case status == http.StatusBadRequest && reason == "DeviceTokenNotForTopic":
		// The token is for another bundle id than PUSHGW_APNS_TOPIC: a wrong topic setting
		// (or an app build with another bundle id) - never one device's state, and it hits
		// every token alike. 410 would have every instance delete every subscription; 502
		// keeps them until the operator fixes the configuration.
		return &DeliveryError{Kind: deliveryUpstream, Upstream: status, Reason: reason}
	case status == http.StatusTooManyRequests:
		return &DeliveryError{Kind: deliveryRateLimited, Upstream: status, Reason: "TooManyRequests",
			RetryAfter: upstreamRetryAfter(header, now)}
	case status == http.StatusRequestEntityTooLarge:
		return &DeliveryError{Kind: deliveryTooLarge, Upstream: status, Reason: "PayloadTooLarge"}
	default:
		if reason == "" {
			reason = http.StatusText(status)
		}
		return &DeliveryError{Kind: deliveryUpstream, Upstream: status, Reason: reason}
	}
}

// providerToken returns the cached provider JWT, minting a new one every 40 minutes.
func (c *apnsClient) providerToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.jwt != "" && now.Sub(c.issued) < apnsTokenRefresh && !now.Before(c.issued) {
		return c.jwt, nil
	}
	token, err := signES256(c.cfg.Key,
		map[string]string{"alg": "ES256", "kid": c.cfg.KeyID},
		map[string]any{"iss": c.cfg.TeamID, "iat": now.Unix()})
	if err != nil {
		return "", err
	}
	c.jwt, c.issued = token, now
	return token, nil
}

func (c *apnsClient) invalidate(token string) {
	c.mu.Lock()
	if c.jwt == token {
		c.jwt = ""
	}
	c.mu.Unlock()
}
