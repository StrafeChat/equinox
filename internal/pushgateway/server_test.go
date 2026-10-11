package pushgateway

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testPublicURL = "http://127.0.0.1:4029"

type testGateway struct {
	t     *testing.T
	srv   *Server
	clock *fakeClock
	vapid *ecdsa.PrivateKey
}

func newTestGateway(t *testing.T, mutate func(*Config, *Options)) *testGateway {
	t.Helper()
	clock := newFakeClock()
	cfg := &Config{PublicURL: mustURL(t, testPublicURL), SealKey: randomKey(t), Debug: true}
	opts := Options{Now: clock.Now}
	if mutate != nil {
		mutate(cfg, &opts)
	}
	srv, err := NewServer(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return &testGateway{t: t, srv: srv, clock: clock, vapid: newP256(t)}
}

func (g *testGateway) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	g.srv.ServeHTTP(rec, req)
	return rec
}

func (g *testGateway) register(body string, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if remote != "" {
		req.RemoteAddr = remote
	}
	return g.do(req)
}

// endpointPath registers a debug token and returns the endpoint's path.
func (g *testGateway) endpointPath(slot string) string {
	g.t.Helper()
	rec := g.register(`{"platform":"debug","token":"emulator","slot":"`+slot+`"}`, "")
	if rec.Code != http.StatusOK {
		g.t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Endpoint string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Endpoint, testPublicURL+"/v1/push/") {
		g.t.Fatalf("endpoint = %q", out.Endpoint)
	}
	return strings.TrimPrefix(out.Endpoint, testPublicURL)
}

func (g *testGateway) pushReq(path string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", vapidHeader(g.t, g.vapid, testPublicURL, g.clock.Now().Add(12*time.Hour), "mailto:ops@example.com"))
	return req
}

func (g *testGateway) debugPushes(after string) []DebugPush {
	g.t.Helper()
	rec := g.do(httptest.NewRequest(http.MethodGet, "/v1/debug/pushes?after="+after, nil))
	if rec.Code != http.StatusOK {
		g.t.Fatalf("debug pushes: %d", rec.Code)
	}
	var out struct{ Pushes []DebugPush }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Pushes
}

func TestDebugPushFlow(t *testing.T) {
	g := newTestGateway(t, nil)
	path := g.endpointPath("acc_1")
	body := fakeAES128GCM(t, 300)

	req := g.pushReq(path, body)
	req.Header.Set("Topic", "c42")
	rec := g.do(req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("TTL") != "86400" {
		t.Errorf("response TTL = %q", rec.Header().Get("TTL"))
	}

	req = g.pushReq(path, body)
	req.Header.Del("Urgency")
	req.Header.Set("TTL", "999999999") // capped to 28 days
	if rec := g.do(req); rec.Code != http.StatusCreated || rec.Header().Get("TTL") != strconv.Itoa(maxTTL) {
		t.Fatalf("second push: %d TTL %q", rec.Code, rec.Header().Get("TTL"))
	}

	pushes := g.debugPushes("")
	if len(pushes) != 2 {
		t.Fatalf("%d debug pushes, want 2", len(pushes))
	}
	p := pushes[0]
	if p.Seq != 1 || p.Token != "emulator" || p.Slot != "acc_1" || p.Urgency != "high" || p.TTL != 86400 || p.Topic != "c42" {
		t.Errorf("debug push = %+v", p)
	}
	if got, _ := b64.DecodeString(p.P); !bytes.Equal(got, body) {
		t.Error("the body must be forwarded byte for byte")
	}
	if pushes[1].Urgency != "normal" || pushes[1].TTL != maxTTL {
		t.Errorf("defaults: %+v", pushes[1])
	}
	if after := g.debugPushes("1"); len(after) != 1 || after[0].Seq != 2 {
		t.Errorf("after=1 -> %+v", after)
	}
}

func TestDebugStoreKeepsLast200(t *testing.T) {
	d := &debugStore{}
	for range 250 {
		_ = d.Send(t.Context(), &Message{Reg: &Registration{}, Urgency: "high"})
	}
	all := d.after(0)
	if len(all) != debugCapacity || all[0].Seq != 51 || all[len(all)-1].Seq != 250 {
		t.Fatalf("kept %d, first %d last %d", len(all), all[0].Seq, all[len(all)-1].Seq)
	}
}

func TestPushRejections(t *testing.T) {
	g := newTestGateway(t, nil)
	path := g.endpointPath("acc1")
	good := fakeAES128GCM(t, 200)

	raw, _ := b64.DecodeString(strings.TrimPrefix(path, "/v1/push/"))
	raw[len(raw)-5] ^= 0x01
	tampered := "/v1/push/" + b64.EncodeToString(raw)

	cases := []struct {
		name   string
		mutate func(*http.Request)
		body   []byte
		path   string
		want   int
	}{
		{"no Content-Encoding", func(r *http.Request) { r.Header.Del("Content-Encoding") }, good, path, 415},
		{"aesgcm (old draft) encoding", func(r *http.Request) { r.Header.Set("Content-Encoding", "aesgcm") }, good, path, 415},
		{"no TTL", func(r *http.Request) { r.Header.Del("TTL") }, good, path, 400},
		{"negative TTL", func(r *http.Request) { r.Header.Set("TTL", "-1") }, good, path, 400},
		{"junk TTL", func(r *http.Request) { r.Header.Set("TTL", "soon") }, good, path, 400},
		{"bad Urgency", func(r *http.Request) { r.Header.Set("Urgency", "urgent") }, good, path, 400},
		{"bad Topic", func(r *http.Request) { r.Header.Set("Topic", "has space") }, good, path, 400},
		{"long Topic", func(r *http.Request) { r.Header.Set("Topic", strings.Repeat("a", 33)) }, good, path, 400},
		{"body over 4096", nil, fakeAES128GCM(t, 4096), path, 413},
		{"body too short", nil, good[:85], path, 400},
		{"key id not 65 bytes", nil, func() []byte { b := bytes.Clone(good); b[20] = 32; return b }(), path, 400},
		{"missing VAPID", func(r *http.Request) { r.Header.Del("Authorization") }, good, path, 401},
		{"VAPID for another audience", func(r *http.Request) {
			r.Header.Set("Authorization", vapidHeader(t, g.vapid, "https://push.strafe.chat", g.clock.Now().Add(time.Hour), "mailto:a@b.c"))
		}, good, path, 401},
		{"expired VAPID", func(r *http.Request) {
			r.Header.Set("Authorization", vapidHeader(t, g.vapid, testPublicURL, g.clock.Now().Add(-time.Minute), "mailto:a@b.c"))
		}, good, path, 401},
		{"tampered endpoint", nil, good, tampered, 404},
		{"made-up endpoint", nil, good, "/v1/push/AAAA", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := g.pushReq(c.path, c.body)
			if c.mutate != nil {
				c.mutate(req)
			}
			rec := g.do(req)
			if rec.Code != c.want {
				t.Fatalf("got %d (%s), want %d", rec.Code, rec.Body, c.want)
			}
			if c.want == 401 && rec.Header().Get("WWW-Authenticate") != "vapid" {
				t.Error("401 must carry WWW-Authenticate: vapid")
			}
		})
	}
	if n := len(g.debugPushes("")); n != 0 {
		t.Fatalf("%d rejected pushes were delivered", n)
	}

	// GET on the push path is not a Web Push.
	if rec := g.do(httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET push endpoint = %d, want 405", rec.Code)
	}
}

func TestPushRateLimits(t *testing.T) {
	g := newTestGateway(t, nil)
	path := g.endpointPath("acc1")
	body := fakeAES128GCM(t, 100)

	for i := range endpointPerMinute {
		if rec := g.do(g.pushReq(path, body)); rec.Code != http.StatusCreated {
			t.Fatalf("push %d: %d", i, rec.Code)
		}
	}
	rec := g.do(g.pushReq(path, body))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("push over the per-endpoint limit: %d", rec.Code)
	}
	if ra, _ := strconv.Atoi(rec.Header().Get("Retry-After")); ra < 1 || ra > 60 {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	// Another endpoint (even for the same device) has its own budget...
	if rec := g.do(g.pushReq(g.endpointPath("acc1"), body)); rec.Code != http.StatusCreated {
		t.Fatalf("other endpoint: %d", rec.Code)
	}
	// ...and the original one recovers as tokens refill (2 per second).
	g.clock.Advance(time.Second)
	if rec := g.do(g.pushReq(path, body)); rec.Code != http.StatusCreated {
		t.Fatalf("after refill: %d", rec.Code)
	}
}

func TestPushSenderRateLimit(t *testing.T) {
	g := newTestGateway(t, nil)
	// Shrink the per-VAPID-key budget so the test does not need 20000 requests; the
	// limiter itself is covered in ratelimit_test.go.
	g.srv.senderLimit = newLimiter(3, g.clock.Now)
	body := fakeAES128GCM(t, 100)
	for i := range 3 {
		if rec := g.do(g.pushReq(g.endpointPath("acc1"), body)); rec.Code != http.StatusCreated {
			t.Fatalf("push %d: %d", i, rec.Code)
		}
	}
	if rec := g.do(g.pushReq(g.endpointPath("acc1"), body)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the per-sender limit on a fresh endpoint: %d", rec.Code)
	}
	g.vapid = newP256(t)
	if rec := g.do(g.pushReq(g.endpointPath("acc1"), body)); rec.Code != http.StatusCreated {
		t.Fatalf("another sender: %d", rec.Code)
	}
}

func TestRegisterValidation(t *testing.T) {
	g := newTestGateway(t, nil) // debug only: FCM and APNs not configured
	longFCM := strings.Repeat("a", 4097)
	cases := []struct {
		name, body string
		want       int
		msg        string
	}{
		{"fcm not configured", `{"platform":"fcm","token":"abc","slot":"s"}`, 400, "platform not configured"},
		{"apns not configured", `{"platform":"apns","token":"` + testAPNsToken + `","slot":"s"}`, 400, "platform not configured"},
		{"unknown platform", `{"platform":"webpush","token":"abc","slot":"s"}`, 400, "invalid platform"},
		{"bad debug token", `{"platform":"debug","token":"has space","slot":"s"}`, 400, "invalid token"},
		{"bad slot", `{"platform":"debug","token":"emulator","slot":"a/b"}`, 400, "invalid slot"},
		{"long slot", `{"platform":"debug","token":"emulator","slot":"` + strings.Repeat("s", 33) + `"}`, 400, "invalid slot"},
		{"long fallback", `{"platform":"debug","token":"emulator","slot":"s","fallback":"` + strings.Repeat("é", 121) + `"}`, 400, "invalid fallback"},
		{"control char in fallback", `{"platform":"debug","token":"emulator","slot":"s","fallback":"a\u0007b"}`, 400, "invalid fallback"},
		{"not JSON", `platform=debug`, 400, "invalid JSON"},
		{"120-rune fallback is fine", `{"platform":"debug","token":"emulator","slot":"s","fallback":"` + strings.Repeat("é", 120) + `"}`, 200, ""},
	}
	for _, c := range cases {
		rec := g.register(c.body, "")
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, rec.Code, rec.Body, c.want, c.msg)
		}
	}

	// With both platforms configured, token formats are enforced per platform.
	full := newTestGateway(t, func(c *Config, _ *Options) {
		c.FCM = &FCMCredentials{ProjectID: "p", ClientEmail: "e", TokenURI: "https://oauth2.example/token", PrivateKey: rsaKey(t)}
		c.APNs = testAPNsConfig(t)
		c.Debug = false
	})
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"fcm ok", `{"platform":"fcm","token":"dGVzdDp0b2tlbi1fLg","slot":"s"}`, 200},
		{"fcm with space", `{"platform":"fcm","token":"a b","slot":"s"}`, 400},
		{"fcm too long", `{"platform":"fcm","token":"` + longFCM + `","slot":"s"}`, 400},
		{"apns ok", `{"platform":"apns","token":"` + testAPNsToken + `","slot":"s","sandbox":true}`, 200},
		{"apns not hex", `{"platform":"apns","token":"` + strings.Repeat("z", 64) + `","slot":"s"}`, 400},
		{"apns too short", `{"platform":"apns","token":"abcd","slot":"s"}`, 400},
		{"debug refused outside debug mode", `{"platform":"debug","token":"emulator","slot":"s"}`, 400},
	} {
		if rec := full.register(c.body, ""); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body, c.want)
		}
	}

	// The sealed registration carries the defaults and the sandbox flag.
	rec := full.register(`{"platform":"apns","token":"`+strings.ToUpper(testAPNsToken)+`","slot":"s","sandbox":true}`, "")
	var out struct{ Endpoint string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	reg, err := full.srv.sealer.Open(out.Endpoint[strings.LastIndex(out.Endpoint, "/")+1:])
	if err != nil {
		t.Fatal(err)
	}
	if reg.Token != testAPNsToken || !reg.Sandbox || reg.Fallback != defaultFallback || reg.Slot != "s" {
		t.Errorf("sealed registration = %+v", reg)
	}
}

func TestRegisterRateLimitPerIP(t *testing.T) {
	g := newTestGateway(t, func(c *Config, _ *Options) { c.TrustedProxies = []string{"10.0.0.0/8"} })
	body := `{"platform":"debug","token":"emulator","slot":"s"}`
	for i := range registerPerMinute {
		if rec := g.register(body, "203.0.113.7:5000"); rec.Code != http.StatusOK {
			t.Fatalf("register %d: %d", i, rec.Code)
		}
	}
	rec := g.register(body, "203.0.113.7:5000")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := g.register(body, "203.0.113.8:5000"); rec.Code != http.StatusOK {
		t.Fatalf("another IP: %d", rec.Code)
	}

	// Behind a trusted proxy, the forwarded client address is what is limited...
	viaProxy := func(client string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/register", strings.NewReader(body))
		req.RemoteAddr = "10.1.2.3:443"
		req.Header.Set("X-Forwarded-For", client)
		return g.do(req).Code
	}
	if code := viaProxy("198.51.100.1"); code != http.StatusOK {
		t.Fatalf("via proxy: %d", code)
	}
	// ...so the throttled client stays throttled through the proxy, even when it prepends
	// a fake address of its own.
	if code := viaProxy("1.2.3.4, 203.0.113.7"); code != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF escaped the limit: %d", code)
	}
}

func TestPushThroughFCMMapsUpstream(t *testing.T) {
	google := newFakeGoogle(t)
	g := newTestGateway(t, func(c *Config, o *Options) {
		c.FCM = google.creds()
		o.HTTPClient = google.srv.Client()
		o.FCMBaseURL = google.srv.URL
	})
	rec := g.register(`{"platform":"fcm","token":"fcm-token-123","slot":"acc1"}`, "")
	var out struct{ Endpoint string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	path := strings.TrimPrefix(out.Endpoint, testPublicURL)
	body := fakeAES128GCM(t, 100)

	if rec := g.do(g.pushReq(path, body)); rec.Code != http.StatusCreated {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	_, android, msg := google.last()
	if msg["token"] != "fcm-token-123" || android["priority"] != "HIGH" {
		t.Fatalf("forwarded message = %v", msg)
	}

	for _, c := range []struct {
		status int
		body   string
		want   int
	}{
		{404, `{"error":{"details":[{"errorCode":"UNREGISTERED"}]}}`, http.StatusGone},
		{429, `{}`, http.StatusTooManyRequests},
		{500, `{}`, http.StatusBadGateway},
	} {
		google.setRespond(func(w http.ResponseWriter, _ int32) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		})
		rec := g.do(g.pushReq(path, body))
		if rec.Code != c.want {
			t.Errorf("upstream %d -> %d, want %d", c.status, rec.Code, c.want)
		}
		if c.want == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
			t.Error("upstream 429 must carry Retry-After")
		}
	}
}

func TestPushToUnconfiguredPlatform(t *testing.T) {
	key := randomKey(t)
	withFCM := newTestGateway(t, func(c *Config, _ *Options) {
		c.SealKey = key
		c.FCM = &FCMCredentials{ProjectID: "p", ClientEmail: "e", TokenURI: "https://oauth2.example/token", PrivateKey: rsaKey(t)}
	})
	rec := withFCM.register(`{"platform":"fcm","token":"tok","slot":"s"}`, "")
	var out struct{ Endpoint string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	fcmPath := strings.TrimPrefix(out.Endpoint, testPublicURL)
	debugPath := withFCM.endpointPath("s")

	// Same seal key, but FCM switched off and debug mode off.
	without := newTestGateway(t, func(c *Config, _ *Options) { c.SealKey = key; c.Debug = false })
	without.vapid = withFCM.vapid
	if rec := without.do(without.pushReq(fcmPath, fakeAES128GCM(t, 50))); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("FCM endpoint without FCM configured: %d, want 503 (not the device's fault)", rec.Code)
	}
	if rec := without.do(without.pushReq(debugPath, fakeAES128GCM(t, 50))); rec.Code != http.StatusGone {
		t.Errorf("debug endpoint outside debug mode: %d, want 410", rec.Code)
	}
	if rec := without.do(httptest.NewRequest(http.MethodGet, "/v1/debug/pushes", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("debug listing outside debug mode: %d, want 404", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	g := newTestGateway(t, func(c *Config, _ *Options) { c.APNs = testAPNsConfig(t) })
	rec := g.do(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok\nplatforms: apns,debug\n" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body)
	}
}

func TestPublicURLWithPathPrefix(t *testing.T) {
	g := newTestGateway(t, func(c *Config, _ *Options) { c.PublicURL = mustURL(t, "https://example.com/push/") })
	rec := g.register(`{"platform":"debug","token":"emulator","slot":"s"}`, "")
	var out struct{ Endpoint string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Endpoint, "https://example.com/push/v1/push/") {
		t.Fatalf("endpoint = %q", out.Endpoint)
	}
	sealedPath := strings.TrimPrefix(out.Endpoint, "https://example.com/push")
	for _, path := range []string{"/push" + sealedPath, sealedPath} { // proxy keeps or strips the prefix
		req := g.pushReq(path, fakeAES128GCM(t, 50))
		req.Header.Set("Authorization", vapidHeader(t, g.vapid, "https://example.com", g.clock.Now().Add(time.Hour), "mailto:a@b.c"))
		if rec := g.do(req); rec.Code != http.StatusCreated {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}
