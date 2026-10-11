package pushgateway

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	testRSAOnce sync.Once
	testRSAKey  *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testRSAOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testRSAKey = k
	})
	return testRSAKey
}

// fakeGoogle is an httptest TLS server playing both Google's OAuth2 token endpoint and
// FCM HTTP v1.
type fakeGoogle struct {
	t          *testing.T
	srv        *httptest.Server
	key        *rsa.PrivateKey
	tokenCalls atomic.Int32
	sendCalls  atomic.Int32

	mu        sync.Mutex
	lastAuth  string
	lastSend  map[string]any
	nextToken string
	// respond, when set, answers sends; nil answers 200.
	respond func(w http.ResponseWriter, attempt int32)
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	g := &fakeGoogle{t: t, key: rsaKey(t), nextToken: "ya29.first"}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

// setRespond and last go through the lock: the server runs on its own goroutines, and a
// socket round trip is not a happens-before edge the race detector can see.
func (g *fakeGoogle) setRespond(fn func(w http.ResponseWriter, attempt int32)) {
	g.mu.Lock()
	g.respond = fn
	g.mu.Unlock()
}

func (g *fakeGoogle) last() (auth string, android map[string]any, msg map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	msg, _ = g.lastSend["message"].(map[string]any)
	android, _ = msg["android"].(map[string]any)
	return g.lastAuth, android, msg
}

func (g *fakeGoogle) creds() *FCMCredentials {
	return &FCMCredentials{
		ProjectID:    "strafe-test",
		ClientEmail:  "push@strafe-test.iam.gserviceaccount.com",
		PrivateKeyID: "kid-1",
		TokenURI:     g.srv.URL + "/token",
		PrivateKey:   g.key,
	}
}

func (g *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/token":
		g.tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			g.t.Errorf("grant_type = %q", r.PostForm.Get("grant_type"))
		}
		g.checkAssertion(r.PostForm.Get("assertion"))
		g.mu.Lock()
		tok := g.nextToken
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"`+tok+`","expires_in":3599,"token_type":"Bearer"}`)
	case "/v1/projects/strafe-test/messages:send":
		n := g.sendCalls.Add(1)
		if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			g.t.Errorf("send: method %s content-type %q", r.Method, r.Header.Get("Content-Type"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.lastAuth, g.lastSend = r.Header.Get("Authorization"), body
		respond := g.respond
		g.mu.Unlock()
		if respond != nil {
			respond(w, n)
			return
		}
		_, _ = io.WriteString(w, `{"name":"projects/strafe-test/messages/1"}`)
	default:
		http.NotFound(w, r)
	}
}

// checkAssertion verifies the service-account JWT the way Google does: RS256 under the
// account's key, with the right issuer, scope, audience and lifetime.
func (g *fakeGoogle) checkAssertion(assertion string) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		g.t.Errorf("assertion is not a JWT")
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, _ := b64.DecodeString(parts[2])
	if err := rsa.VerifyPKCS1v15(&g.key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		g.t.Errorf("assertion signature: %v", err)
	}
	var hdr map[string]string
	var claims map[string]any
	_ = decodeJWTPart(parts[0], &hdr)
	_ = decodeJWTPart(parts[1], &claims)
	if hdr["alg"] != "RS256" || hdr["kid"] != "kid-1" {
		g.t.Errorf("assertion header = %v", hdr)
	}
	if claims["iss"] != "push@strafe-test.iam.gserviceaccount.com" || claims["scope"] != fcmScope ||
		claims["aud"] != g.srv.URL+"/token" {
		g.t.Errorf("assertion claims = %v", claims)
	}
	iat, _ := claims["iat"].(json.Number).Int64()
	exp, _ := claims["exp"].(json.Number).Int64()
	if exp-iat != 3600 {
		g.t.Errorf("assertion lifetime = %ds, want 3600", exp-iat)
	}
}

func testMessage(urgency, topic string) *Message {
	return &Message{
		Reg:     &Registration{Platform: PlatformFCM, Token: "fcm-token-123", Slot: "acc1", Fallback: "New message"},
		Body:    []byte{0xde, 0xad, 0xbe, 0xef},
		TTL:     86400,
		Urgency: urgency,
		Topic:   topic,
	}
}

func TestFCMRequestShape(t *testing.T) {
	g := newFakeGoogle(t)
	clock := newFakeClock()
	c := newFCMClient(g.creds(), g.srv.URL, g.srv.Client(), clock.Now)

	if err := c.Send(context.Background(), testMessage("high", "c42")); err != nil {
		t.Fatal(err)
	}
	auth, android, msg := g.last()
	if auth != "Bearer ya29.first" {
		t.Fatalf("Authorization = %q", auth)
	}
	if msg["token"] != "fcm-token-123" {
		t.Errorf("token = %v", msg["token"])
	}
	if _, ok := msg["notification"]; ok {
		t.Error("FCM messages must be data-only")
	}
	data := msg["data"].(map[string]any)
	if data["s"] != "acc1" || data["p"] != "3q2-7w" || len(data) != 2 {
		t.Errorf("data = %v", data)
	}
	if android["priority"] != "HIGH" || android["ttl"] != "86400s" || android["collapse_key"] != "c42" {
		t.Errorf("android (high) = %v", android)
	}

	// A silent message without a topic: NORMAL priority and no collapse key at all.
	m := testMessage("low", "")
	m.TTL = 0
	if err := c.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	_, android, _ = g.last()
	if android["priority"] != "NORMAL" || android["ttl"] != "0s" {
		t.Errorf("android (low) = %v", android)
	}
	if _, ok := android["collapse_key"]; ok {
		t.Error("collapse_key must be omitted without a Topic")
	}
	for _, u := range []string{"normal", "very-low"} {
		_ = c.Send(context.Background(), testMessage(u, ""))
		if _, android, _ := g.last(); android["priority"] != "NORMAL" {
			p := android["priority"]
			t.Errorf("urgency %s -> priority %v, want NORMAL", u, p)
		}
	}
	if n := g.tokenCalls.Load(); n != 1 {
		t.Fatalf("access token fetched %d times, want 1 (cached)", n)
	}
}

func TestFCMAccessTokenRefresh(t *testing.T) {
	g := newFakeGoogle(t)
	clock := newFakeClock()
	c := newFCMClient(g.creds(), g.srv.URL, g.srv.Client(), clock.Now)

	_ = c.Send(context.Background(), testMessage("high", ""))
	clock.Advance(50 * time.Minute) // still > 5 min before the 3599 s expiry
	_ = c.Send(context.Background(), testMessage("high", ""))
	if n := g.tokenCalls.Load(); n != 1 {
		t.Fatalf("token refreshed too early: %d exchanges", n)
	}
	g.mu.Lock()
	g.nextToken = "ya29.second"
	g.mu.Unlock()
	clock.Advance(6 * time.Minute) // inside the 5-minute early-refresh window
	_ = c.Send(context.Background(), testMessage("high", ""))
	if auth, _, _ := g.last(); g.tokenCalls.Load() != 2 || auth != "Bearer ya29.second" {
		t.Fatalf("token not refreshed near expiry: %d exchanges, auth %q", g.tokenCalls.Load(), auth)
	}
}

func TestFCMRetriesOnceOnUnauthorized(t *testing.T) {
	g := newFakeGoogle(t)
	g.setRespond(func(w http.ResponseWriter, attempt int32) {
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	c := newFCMClient(g.creds(), g.srv.URL, g.srv.Client(), newFakeClock().Now)
	if err := c.Send(context.Background(), testMessage("high", "")); err != nil {
		t.Fatalf("expected success after re-auth, got %v", err)
	}
	if g.tokenCalls.Load() != 2 || g.sendCalls.Load() != 2 {
		t.Fatalf("want 2 token exchanges and 2 sends, got %d/%d", g.tokenCalls.Load(), g.sendCalls.Load())
	}

	// Unauthorized twice in a row is our credentials' problem: 502, no loop.
	g.setRespond(func(w http.ResponseWriter, _ int32) { w.WriteHeader(http.StatusUnauthorized) })
	err := c.Send(context.Background(), testMessage("high", ""))
	var de *DeliveryError
	if !errors.As(err, &de) || de.httpStatus() != http.StatusBadGateway {
		t.Fatalf("persistent 401 -> %v, want 502", err)
	}
}

func TestFCMErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		want       int
		wantWait   time.Duration
	}{
		{"unregistered", 404, `{"error":{"code":404,"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`, "", 410, 0},
		{"invalid token by field", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid value","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"INVALID_ARGUMENT"},{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"message.token","description":"Invalid registration token"}]}]}}`, "", 410, 0},
		{"invalid token by message", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"The registration token is not a valid FCM registration token"}}`, "", 410, 0},
		{"invalid argument not about the token", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid JSON payload received.","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"message.android.ttl"}]}]}}`, "", 502, 0},
		// Credentials of another Firebase project refuse every token: never 410.
		{"sender id mismatch", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"SENDER_ID_MISMATCH"}]}}`, "", 502, 0},
		{"bare 404 (wrong project) is not a dead token", 404, `{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found."}}`, "", 502, 0},
		{"quota", 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"errorCode":"QUOTA_EXCEEDED"}]}}`, "7", 429, 7 * time.Second},
		{"quota without Retry-After", 429, `{}`, "", 429, 30 * time.Second},
		{"quota capped", 429, `{}`, "3600", 429, time.Minute},
		{"unavailable", 503, `{"error":{"code":503,"status":"UNAVAILABLE"}}`, "", 502, 0},
		{"internal", 500, ``, "", 502, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newFakeGoogle(t)
			g.setRespond(func(w http.ResponseWriter, _ int32) {
				if c.retryAfter != "" {
					w.Header().Set("Retry-After", c.retryAfter)
				}
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			client := newFCMClient(g.creds(), g.srv.URL, g.srv.Client(), newFakeClock().Now)
			err := client.Send(context.Background(), testMessage("high", ""))
			var de *DeliveryError
			if !errors.As(err, &de) {
				t.Fatalf("got %v, want a DeliveryError", err)
			}
			if de.httpStatus() != c.want || de.RetryAfter != c.wantWait {
				t.Fatalf("got status %d wait %s, want %d wait %s", de.httpStatus(), de.RetryAfter, c.want, c.wantWait)
			}
		})
	}
}

func TestFCMOAuthFailureIsBadGateway(t *testing.T) {
	g := newFakeGoogle(t)
	creds := g.creds()
	creds.TokenURI = g.srv.URL + "/no-such-token-endpoint"
	c := newFCMClient(creds, g.srv.URL, g.srv.Client(), newFakeClock().Now)
	err := c.Send(context.Background(), testMessage("high", ""))
	var de *DeliveryError
	if !errors.As(err, &de) || de.httpStatus() != http.StatusBadGateway {
		t.Fatalf("got %v, want 502", err)
	}
	if g.sendCalls.Load() != 0 {
		t.Fatal("must not send without an access token")
	}
}

func TestFCMRefusesOversizedData(t *testing.T) {
	g := newFakeGoogle(t)
	c := newFCMClient(g.creds(), g.srv.URL, g.srv.Client(), newFakeClock().Now)
	m := testMessage("high", "")
	m.Body = make([]byte, 4096) // base64url grows it past FCM's 4 KiB data limit
	err := c.Send(context.Background(), m)
	var de *DeliveryError
	if !errors.As(err, &de) || de.httpStatus() != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %v, want 413", err)
	}
	if g.sendCalls.Load() != 0 {
		t.Fatal("an oversized message must not reach FCM")
	}
}
