package push

import (
	"context"
	"encoding/binary"
	"net/http"
	"testing"
	"time"
)

// The real sender against a TLS push service: the body decrypts with the device's keys to
// the payload, padded to a bucket, and every RFC 8030/8291/8292 header is right.
func TestSenderDeliversEncryptedPush(t *testing.T) {
	f := newFakePushService(t)
	dev := newTestDevice(t)
	vapid := testVAPID(t)
	s := NewSender(testPushConfig(), vapid)
	trustServer(t, s, f.srv)

	endpoint := f.srv.URL + "/v1/push/AbC123"
	sub := dev.subscription(1, 2, endpoint, DefaultPrefs())
	payload := []byte(`{"v":1,"t":"call","uid":"1","ts":5,"rid":"77","rt":1}`)
	out := s.Send(context.Background(), &sub, payload, deliveryFor(TypeCall, 77), true)
	if out.kind != delivered || out.status != http.StatusCreated {
		t.Fatalf("outcome = %+v, want delivered/201", out)
	}
	got := f.pushes()
	if len(got) != 1 {
		t.Fatalf("push service saw %d requests", len(got))
	}
	p := got[0]
	h := p.Header
	for k, want := range map[string]string{
		"Content-Encoding": "aes128gcm",
		"Content-Type":     "application/octet-stream",
		"Ttl":              "45",
		"Urgency":          "high",
		"Topic":            "c77",
	} {
		if h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
	claims := verifyVAPID(t, h.Get("Authorization"), vapid.PublicKey())
	if claims["aud"] != f.srv.URL {
		t.Errorf("aud = %v, want the endpoint origin %s", claims["aud"], f.srv.URL)
	}
	if claims["sub"] != "mailto:ops@example.com" {
		t.Errorf("sub = %v", claims["sub"])
	}
	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	if d := time.Until(exp); d < 11*time.Hour || d > 12*time.Hour+time.Minute {
		t.Errorf("exp is %v away, want ~12h", d)
	}

	// The aes128gcm header: rs = 4096, a 65-byte keyid; then the 256-byte bucket + tag.
	if rs := binary.BigEndian.Uint32(p.Body[16:20]); rs != 4096 {
		t.Errorf("rs = %d", rs)
	}
	if p.Body[20] != 65 || p.Body[21] != 0x04 {
		t.Errorf("keyid is not an uncompressed P-256 point")
	}
	if len(p.Body) != headerLen+256+tagLen {
		t.Errorf("body is %d bytes, want the 256-byte bucket", len(p.Body))
	}
	if plain := decryptBody(t, p.Body, dev.priv, dev.auth); string(plain) != string(payload) {
		t.Errorf("decrypted %s", plain)
	}

	// Without a topic (msg) no Topic header is sent.
	_ = s.Send(context.Background(), &sub, payload, deliveryFor(TypeMessage, 77), true)
	last := f.pushes()[1].Header
	if last.Get("Topic") != "" || last.Get("Ttl") != "86400" || last.Get("Urgency") != "high" {
		t.Errorf("msg headers: topic=%q ttl=%q urgency=%q", last.Get("Topic"), last.Get("Ttl"), last.Get("Urgency"))
	}
}

func TestDeliveryParameters(t *testing.T) {
	cases := []struct {
		t       string
		ttl     int
		urgency string
		topic   string
	}{
		{TypeMessage, 86400, "high", ""},
		{TypeCall, 45, "high", "c9"},
		{TypeCallEnd, 60, "normal", "c9"},
		{TypeRead, 3600, "low", "r9"},
		{TypeTest, 300, "high", ""},
	}
	for _, c := range cases {
		d := deliveryFor(c.t, 9)
		if d.ttl != c.ttl || d.urgency != c.urgency || d.topic != c.topic {
			t.Errorf("%s: %+v", c.t, d)
		}
	}
}

// How a push service's answers are treated: 404/410 gone, 4xx rejected, 5xx and 429
// retried then unavailable.
func TestSenderResponseHandling(t *testing.T) {
	dev := newTestDevice(t)
	payload := []byte(`{"v":1,"t":"test","uid":"1","ts":1}`)
	cases := []struct {
		name     string
		statuses []int
		retries  bool
		want     outcomeKind
		requests int
	}{
		{"gone", []int{410}, true, gone, 1},
		{"not found", []int{404}, true, gone, 1},
		{"forbidden", []int{403}, true, rejected, 1},
		{"too large", []int{413}, true, rejected, 1},
		{"5xx then ok", []int{503, 502}, true, delivered, 3},
		{"5xx exhausts retries", []int{500, 500, 500, 500}, true, unavailable, 4},
		{"429 then ok", []int{429}, true, delivered, 2},
		{"no retries for a test push", []int{503}, false, unavailable, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakePushService(t, c.statuses...)
			f.retryAfter = "1"
			s := NewSender(testPushConfig(), testVAPID(t))
			trustServer(t, s, f.srv)
			var waits []time.Duration
			s.sleep = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return nil }
			sub := dev.subscription(1, 2, f.srv.URL+"/p", DefaultPrefs())
			out := s.Send(context.Background(), &sub, payload, deliveryFor(TypeMessage, 1), c.retries)
			if out.kind != c.want {
				t.Errorf("outcome = %+v, want kind %d", out, c.want)
			}
			if n := len(f.pushes()); n != c.requests {
				t.Errorf("%d requests, want %d", n, c.requests)
			}
			if c.name == "5xx exhausts retries" && (len(waits) != 3 || waits[0] != time.Second || waits[1] != 4*time.Second || waits[2] != 15*time.Second) {
				t.Errorf("backoff = %v, want 1s 4s 15s", waits)
			}
			if c.name == "429 then ok" && (len(waits) != 1 || waits[0] != time.Second) {
				t.Errorf("429 wait = %v, want Retry-After (1s)", waits)
			}
		})
	}
}

// A call push lives 45 s: a retry that would land after that is not attempted.
func TestSenderRetriesStayWithinTTL(t *testing.T) {
	f := newFakePushService(t, 503, 503, 503, 503)
	dev := newTestDevice(t)
	s := NewSender(testPushConfig(), testVAPID(t))
	trustServer(t, s, f.srv)
	s.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	sub := dev.subscription(1, 2, f.srv.URL+"/p", DefaultPrefs())
	d := deliveryFor(TypeCall, 1)
	d.ttl = 3 // only the 1 s retry fits
	out := s.Send(context.Background(), &sub, []byte(`{}`), d, true)
	if out.kind != unavailable || len(f.pushes()) != 2 {
		t.Errorf("outcome %+v after %d requests, want unavailable after 2", out, len(f.pushes()))
	}
}

// Outside development a loopback endpoint is refused before anything is sent, and so is
// a redirect (the endpoint never gets to send the request elsewhere).
func TestSenderRefusesPrivateEndpointsAndRedirects(t *testing.T) {
	f := newFakePushService(t)
	dev := newTestDevice(t)
	cfg := testPushConfig()
	cfg.AllowInsecure = false
	s := NewSender(cfg, testVAPID(t))
	sub := dev.subscription(1, 2, f.srv.URL+"/p", DefaultPrefs())
	if out := s.Send(context.Background(), &sub, []byte(`{}`), deliveryFor(TypeTest, 0), true); out.kind != refused {
		t.Errorf("loopback endpoint: %+v, want refused", out)
	}
	if len(f.pushes()) != 0 {
		t.Error("a request reached the loopback server")
	}

	redirect := newFakePushService(t, http.StatusTemporaryRedirect)
	s = NewSender(testPushConfig(), testVAPID(t))
	trustServer(t, s, redirect.srv)
	sub = dev.subscription(1, 2, redirect.srv.URL+"/p", DefaultPrefs())
	if out := s.Send(context.Background(), &sub, []byte(`{}`), deliveryFor(TypeTest, 0), true); out.kind != rejected || out.status != 307 {
		t.Errorf("redirect: %+v, want rejected 307", out)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if parseRetryAfter("7") != 7*time.Second || parseRetryAfter("") != 0 || parseRetryAfter("soon") != 0 {
		t.Error("delay-seconds form misparsed")
	}
	at := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(at); d < 25*time.Second || d > 31*time.Second {
		t.Errorf("HTTP-date form gave %v", d)
	}
}
