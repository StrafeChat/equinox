package pushgateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAPNsToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type apnsRequest struct {
	proto   int
	path    string
	header  http.Header
	payload map[string]any
}

// fakeAPNs is an HTTP/2 TLS httptest server standing in for one APNs environment.
type fakeAPNs struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []apnsRequest
	respond  func(w http.ResponseWriter, n int)
}

func newFakeAPNs(t *testing.T) *fakeAPNs {
	f := &fakeAPNs{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.requests = append(f.requests, apnsRequest{r.ProtoMajor, r.URL.Path, r.Header.Clone(), payload})
		n, respond := len(f.requests), f.respond
		f.mu.Unlock()
		if respond != nil {
			respond(w, n)
			return
		}
		w.Header().Set("apns-id", "00000000-0000-0000-0000-000000000000")
	}))
	f.srv.EnableHTTP2 = true
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPNs) setRespond(fn func(w http.ResponseWriter, n int)) {
	f.mu.Lock()
	f.respond = fn
	f.mu.Unlock()
}

func (f *fakeAPNs) reqs() []apnsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apnsRequest(nil), f.requests...)
}

func testAPNsConfig(t *testing.T) *APNsConfig {
	return &APNsConfig{Key: newP256(t), KeyID: "ABC123DEFG", TeamID: "TEAM123456", Topic: "chat.strafe.mobile"}
}

func apnsMessage(urgency, topic string, sandbox bool) *Message {
	return &Message{
		Reg:     &Registration{Platform: PlatformAPNs, Token: testAPNsToken, Slot: "acc1", Sandbox: sandbox, Fallback: "New message"},
		Body:    []byte{0xde, 0xad, 0xbe, 0xef},
		TTL:     45,
		Urgency: urgency,
		Topic:   topic,
	}
}

// verifyProviderToken checks an APNs provider JWT as Apple would and returns its iat.
func verifyProviderToken(t *testing.T, cfg *APNsConfig, auth string) int64 {
	t.Helper()
	tok, ok := strings.CutPrefix(auth, "bearer ")
	if !ok {
		t.Fatalf("authorization = %q, want bearer <jwt>", auth)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("provider token is not a JWT")
	}
	sig, _ := b64.DecodeString(parts[2])
	if len(sig) != 64 {
		t.Fatalf("ES256 signature is %d bytes, want raw 64", len(sig))
	}
	d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&cfg.Key.PublicKey, d[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("provider token signature does not verify")
	}
	var hdr map[string]string
	var claims map[string]any
	_ = decodeJWTPart(parts[0], &hdr)
	_ = decodeJWTPart(parts[1], &claims)
	if hdr["alg"] != "ES256" || hdr["kid"] != cfg.KeyID {
		t.Fatalf("provider token header = %v", hdr)
	}
	if claims["iss"] != cfg.TeamID || len(claims) != 2 {
		t.Fatalf("claims = %v, want exactly iss (team id) and iat", claims)
	}
	iat, _ := claims["iat"].(json.Number).Int64()
	return iat
}

func TestAPNsAlertShape(t *testing.T) {
	prod, sandbox := newFakeAPNs(t), newFakeAPNs(t)
	clock := newFakeClock()
	cfg := testAPNsConfig(t)
	c := newAPNsClient(cfg, prod.srv.URL, sandbox.srv.URL, prod.srv.Client(), clock.Now)

	if err := c.Send(context.Background(), apnsMessage("high", "c42", false)); err != nil {
		t.Fatal(err)
	}
	r := prod.reqs()[0]
	if r.proto != 2 {
		t.Fatalf("APNs must be spoken over HTTP/2, got HTTP/%d", r.proto)
	}
	if r.path != "/3/device/"+testAPNsToken {
		t.Errorf("path = %s", r.path)
	}
	if iat := verifyProviderToken(t, cfg, r.header.Get("Authorization")); iat != clock.Now().Unix() {
		t.Errorf("iat = %d", iat)
	}
	want := map[string]string{
		"apns-topic":       "chat.strafe.mobile",
		"apns-push-type":   "alert",
		"apns-priority":    "10",
		"apns-expiration":  strconv.FormatInt(clock.Now().Unix()+45, 10),
		"apns-collapse-id": "c42",
	}
	for k, v := range want {
		if got := r.header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	aps := r.payload["aps"].(map[string]any)
	if aps["alert"].(map[string]any)["body"] != "New message" || aps["mutable-content"] != float64(1) || aps["sound"] != "default" {
		t.Errorf("alert aps = %v", aps)
	}
	if _, ok := aps["content-available"]; ok {
		t.Error("an alert must not also be a background push")
	}
	if r.payload["s"] != "acc1" || r.payload["p"] != "3q2-7w" {
		t.Errorf("payload = %v", r.payload)
	}
	if len(sandbox.reqs()) != 0 {
		t.Error("a production registration must not reach the sandbox")
	}
}

func TestAPNsSilentShapeAndSandbox(t *testing.T) {
	prod, sandbox := newFakeAPNs(t), newFakeAPNs(t)
	clock := newFakeClock()
	c := newAPNsClient(testAPNsConfig(t), prod.srv.URL, sandbox.srv.URL, sandbox.srv.Client(), clock.Now)

	for _, urgency := range []string{"normal", "low", "very-low"} {
		m := apnsMessage(urgency, "", true)
		m.TTL = 0
		if err := c.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	reqs := sandbox.reqs()
	if len(reqs) != 3 || len(prod.reqs()) != 0 {
		t.Fatalf("sandbox registrations must go to the sandbox gateway (sandbox %d, prod %d)", len(reqs), len(prod.reqs()))
	}
	for _, r := range reqs {
		if r.header.Get("apns-push-type") != "background" || r.header.Get("apns-priority") != "5" {
			t.Errorf("silent push headers: type %q priority %q", r.header.Get("apns-push-type"), r.header.Get("apns-priority"))
		}
		if r.header.Get("apns-expiration") != "0" {
			t.Errorf("TTL 0 -> apns-expiration %q, want 0", r.header.Get("apns-expiration"))
		}
		if _, ok := r.header["Apns-Collapse-Id"]; ok {
			t.Error("no Topic -> no apns-collapse-id")
		}
		aps := r.payload["aps"].(map[string]any)
		if aps["content-available"] != float64(1) || len(aps) != 1 {
			t.Errorf("silent aps = %v", aps)
		}
		if r.payload["s"] != "acc1" || r.payload["p"] != "3q2-7w" {
			t.Errorf("payload = %v", r.payload)
		}
	}
}

func TestAPNsProviderTokenRefresh(t *testing.T) {
	prod := newFakeAPNs(t)
	clock := newFakeClock()
	cfg := testAPNsConfig(t)
	c := newAPNsClient(cfg, prod.srv.URL, prod.srv.URL, prod.srv.Client(), clock.Now)

	_ = c.Send(context.Background(), apnsMessage("high", "", false))
	clock.Advance(39 * time.Minute)
	_ = c.Send(context.Background(), apnsMessage("high", "", false))
	clock.Advance(2 * time.Minute)
	_ = c.Send(context.Background(), apnsMessage("high", "", false))

	reqs := prod.reqs()
	a0, a1, a2 := reqs[0].header.Get("Authorization"), reqs[1].header.Get("Authorization"), reqs[2].header.Get("Authorization")
	if a0 != a1 {
		t.Error("the provider token must be reused within 40 minutes")
	}
	if a1 == a2 {
		t.Error("the provider token must be refreshed after 40 minutes")
	}
	if iat := verifyProviderToken(t, cfg, a2); iat != clock.Now().Unix() {
		t.Errorf("refreshed iat = %d, want now", iat)
	}
}

func TestAPNsRetriesOnExpiredProviderToken(t *testing.T) {
	prod := newFakeAPNs(t)
	prod.setRespond(func(w http.ResponseWriter, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"reason":"ExpiredProviderToken"}`)
		}
	})
	clock := newFakeClock()
	c := newAPNsClient(testAPNsConfig(t), prod.srv.URL, prod.srv.URL, prod.srv.Client(), clock.Now)
	if err := c.Send(context.Background(), apnsMessage("high", "", false)); err != nil {
		t.Fatalf("expected success after a fresh provider token, got %v", err)
	}
	if n := len(prod.reqs()); n != 2 {
		t.Fatalf("%d requests, want exactly one retry", n)
	}
}

func TestAPNsErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		reason     string
		retryAfter string
		want       int
	}{
		{"unregistered", 410, "Unregistered", "", 410},
		{"expired token, no body", 410, "", "", 410},
		{"bad device token", 400, "BadDeviceToken", "", 410},
		// A wrong topic refuses every token: configuration, not a dead device - never 410.
		{"token not for topic", 400, "DeviceTokenNotForTopic", "", 502},
		{"bad topic is our config", 400, "BadTopic", "", 502},
		{"forbidden other", 403, "BadCertificate", "", 502},
		{"provider token twice", 403, "InvalidProviderToken", "", 502},
		{"too many requests", 429, "TooManyProviderTokenUpdates", "5", 429},
		{"payload too large", 413, "PayloadTooLarge", "", 413},
		{"internal", 500, "InternalServerError", "", 502},
		{"unavailable", 503, "ServiceUnavailable", "", 502},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prod := newFakeAPNs(t)
			prod.setRespond(func(w http.ResponseWriter, _ int) {
				if c.retryAfter != "" {
					w.Header().Set("Retry-After", c.retryAfter)
				}
				w.WriteHeader(c.status)
				if c.reason != "" {
					_, _ = io.WriteString(w, `{"reason":"`+c.reason+`"}`)
				}
			})
			client := newAPNsClient(testAPNsConfig(t), prod.srv.URL, prod.srv.URL, prod.srv.Client(), newFakeClock().Now)
			err := client.Send(context.Background(), apnsMessage("high", "", false))
			var de *DeliveryError
			if !errors.As(err, &de) || de.httpStatus() != c.want {
				t.Fatalf("got %v, want status %d", err, c.want)
			}
			if c.want == 429 && de.RetryAfter != 5*time.Second {
				t.Fatalf("Retry-After = %s, want 5s", de.RetryAfter)
			}
		})
	}
}

func TestAPNsRefusesOversizedPayload(t *testing.T) {
	prod := newFakeAPNs(t)
	c := newAPNsClient(testAPNsConfig(t), prod.srv.URL, prod.srv.URL, prod.srv.Client(), newFakeClock().Now)
	m := apnsMessage("high", "", false)
	m.Body = make([]byte, 3500)
	err := c.Send(context.Background(), m)
	var de *DeliveryError
	if !errors.As(err, &de) || de.httpStatus() != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %v, want 413", err)
	}
	if len(prod.reqs()) != 0 {
		t.Fatal("an oversized payload must not reach APNs")
	}
}
