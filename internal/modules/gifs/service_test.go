package gifs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/logger"
)

// Made-up keys, distinctive enough that finding one in any output means it leaked.
const (
	giphyKey    = "giphyTESTKEY0123456789abcdef"
	heypsterKey = "heypsterTESTKEY9876543210zyx"
)

var bothKeys = config.GifsConfig{GiphyAPIKey: giphyKey, HeypsterAPIKey: heypsterKey}

const okBody = `{"data":[{"id":"g1","title":"Hi","images":{
	"fixed_width":{"url":"https://media.example/fw.gif","width":"200","height":"100"},
	"downsized":{"url":"https://media.example/d.gif","width":"400","height":"200"},
	"fixed_width_still":{"url":"https://media.example/s.gif"}}}],
	"pagination":{"offset":0,"count":1,"total_count":40}}`

// fakeUpstream is a provider that records each request it gets and answers with whatever
// the test set.
type fakeUpstream struct {
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []*http.Request
	status int
	body   string
	header map[string]string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{status: http.StatusOK, body: okBody}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(context.Background()))
		status, body, header := u.status, u.body, u.header
		u.mu.Unlock()
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeUpstream) set(status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.body = status, body
}

func (u *fakeUpstream) requests() []*http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request(nil), u.reqs...)
}

// newTestService points both providers at base, under paths that say which one was called.
func newTestService(keys config.GifsConfig, base string) *Service {
	s := NewService(keys, nil, "test:")
	s.bases[ProviderGiphy] = base + "/giphy/v1"
	s.bases[ProviderHeypster] = base + "/heypster/giphy/v1"
	return s
}

func newTestApp(svc *Service) *fiber.App {
	h := NewHandler(svc)
	app := fiber.New()
	app.Get("/gifs/search", h.Search)
	app.Get("/gifs/trending", h.Trending)
	return app
}

type response struct {
	status int
	raw    string
	json   map[string]any
}

func get(t *testing.T, app *fiber.App, path string, headers ...string) response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := response{status: res.StatusCode, raw: string(raw)}
	if err := json.Unmarshal(raw, &out.json); err != nil {
		t.Fatalf("GET %s: body is not JSON: %q", path, raw)
	}
	return out
}

// captureLogs routes the package logger into a buffer for the rest of the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	logger.InitDefault(logger.LevelDebug)
	t.Cleanup(func() {
		log.SetOutput(prev)
		logger.InitDefault(logger.LevelOff)
	})
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// assertNoKey fails if either test key, in any spelling, shows up in text.
func assertNoKey(t *testing.T, where, text string) {
	t.Helper()
	for _, k := range []string{giphyKey, heypsterKey} {
		if strings.Contains(text, k) || strings.Contains(text, url.QueryEscape(k)) {
			t.Errorf("%s contains an API key: %s", where, text)
		}
	}
	if strings.Contains(text, "api_key="+giphyKey[:6]) || strings.Contains(text, "api_key="+heypsterKey[:6]) {
		t.Errorf("%s contains an api_key parameter: %s", where, text)
	}
}

func TestParamClamping(t *testing.T) {
	for in, want := range map[int]int{-5: 1, 0: 1, 1: 1, 24: 24, 50: 50, 51: 50, 1 << 30: 50} {
		if got := clampLimit(in); got != want {
			t.Errorf("clampLimit(%d) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[int]int{-1: 0, 0: 0, 120: 120, 4999: 4999, 5000: 4999, 1 << 30: 4999} {
		if got := clampOffset(in); got != want {
			t.Errorf("clampOffset(%d) = %d, want %d", in, got, want)
		}
	}
	long := strings.Repeat("é", 60)
	for in, want := range map[string]string{
		"  cats  ":                     "cats",
		"   ":                          "",
		"":                             "",
		long:                           strings.Repeat("é", 50),
		strings.Repeat("a", 49) + " b": strings.Repeat("a", 49), // cut lands on the space
	} {
		if got := normalizeQuery(in); got != want {
			t.Errorf("normalizeQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// The parameters that reach the provider are the clamped ones, whatever the client sent.
func TestUpstreamParamsAreClamped(t *testing.T) {
	up := newFakeUpstream(t)
	app := newTestApp(newTestService(bothKeys, up.srv.URL))

	cases := []struct {
		query         string
		limit, offset string
	}{
		{"", "24", "0"},
		{"&limit=500&offset=-3", "50", "0"},
		{"&limit=0&offset=99999", "1", "4999"},
		{"&limit=abc&offset=xyz", "24", "0"},
		{"&limit=99999999999&offset=-99999999999", "50", "0"},
		{"&limit=10&offset=30", "10", "30"},
	}
	for _, c := range cases {
		before := len(up.requests())
		if r := get(t, app, "/gifs/search?q=cat"+c.query); r.status != http.StatusOK {
			t.Fatalf("search%s: status %d %s", c.query, r.status, r.raw)
		}
		reqs := up.requests()
		if len(reqs) != before+1 {
			t.Fatalf("search%s: want one upstream call", c.query)
		}
		q := reqs[len(reqs)-1].URL.Query()
		if q.Get("limit") != c.limit || q.Get("offset") != c.offset {
			t.Errorf("search%s: upstream got limit=%s offset=%s, want %s/%s", c.query, q.Get("limit"), q.Get("offset"), c.limit, c.offset)
		}
	}

	long := strings.Repeat("ü", 70)
	get(t, app, "/gifs/search?q="+url.QueryEscape("  "+long+"  "))
	reqs := up.requests()
	if got := reqs[len(reqs)-1].URL.Query().Get("q"); got != strings.Repeat("ü", 50) {
		t.Errorf("long query reached the provider as %q (%d runes)", got, len([]rune(got)))
	}
}

// The provider sees a request this server built: the key, the query, fixed headers - and
// nothing the user sent, so not their address, cookies, token or any other header.
func TestUpstreamRequestShape(t *testing.T) {
	up := newFakeUpstream(t)
	app := newTestApp(newTestService(bothKeys, up.srv.URL))

	r := get(t, app, "/gifs/search?q=happy%20cat&limit=12&offset=24",
		"Cookie", "session=secret-cookie",
		"Authorization", "Bearer secret-token",
		"X-Forwarded-For", "203.0.113.7",
		"X-Real-IP", "203.0.113.7",
		"User-Agent", "Mozilla/5.0 users-browser",
		"Referer", "https://app.example/rooms/1")
	if r.status != http.StatusOK {
		t.Fatalf("status %d %s", r.status, r.raw)
	}
	reqs := up.requests()
	if len(reqs) != 1 {
		t.Fatalf("want one upstream call, got %d", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodGet || req.URL.Path != "/giphy/v1/gifs/search" {
		t.Errorf("upstream request %s %s", req.Method, req.URL.Path)
	}
	q := req.URL.Query()
	want := url.Values{"api_key": {giphyKey}, "q": {"happy cat"}, "limit": {"12"}, "offset": {"24"}, "rating": {"pg-13"}}
	if q.Encode() != want.Encode() {
		t.Errorf("upstream query = %s, want %s", q.Encode(), want.Encode())
	}
	if ua := req.Header.Get("User-Agent"); ua != "Strafe/1.0 (+https://strafe.chat)" {
		t.Errorf("User-Agent = %q", ua)
	}
	for _, h := range []string{"Cookie", "Authorization", "X-Forwarded-For", "X-Real-Ip", "Referer", "Forwarded"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("upstream got the user's %s: %q", h, v)
		}
	}

	// Trending sends no q.
	get(t, app, "/gifs/trending")
	req = up.requests()[1]
	if req.URL.Path != "/giphy/v1/gifs/trending" || req.URL.Query().Has("q") {
		t.Errorf("trending request %s?%s", req.URL.Path, req.URL.RawQuery)
	}
}

func TestSearchResponse(t *testing.T) {
	up := newFakeUpstream(t)
	app := newTestApp(newTestService(bothKeys, up.srv.URL))

	r := get(t, app, "/gifs/search?q=hi")
	if r.status != http.StatusOK {
		t.Fatalf("status %d %s", r.status, r.raw)
	}
	want := `{"results":[{"id":"g1","title":"Hi","url":"https://media.example/d.gif","width":400,"height":200,` +
		`"preview_url":"https://media.example/fw.gif","preview_width":200,"preview_height":100,` +
		`"still_url":"https://media.example/s.gif"}],"next_offset":1,"total_count":40}`
	if r.raw != want {
		t.Errorf("body =\n%s\nwant\n%s", r.raw, want)
	}
}

func TestProviderSelection(t *testing.T) {
	up := newFakeUpstream(t)
	app := newTestApp(newTestService(bothKeys, up.srv.URL))

	cases := []struct {
		param, path, key string
	}{
		{"", "/giphy/v1/gifs/search", giphyKey},
		{"&provider=giphy", "/giphy/v1/gifs/search", giphyKey},
		{"&provider=heypster", "/heypster/giphy/v1/gifs/search", heypsterKey},
		{"&provider=%20HEYPSTER%20", "/heypster/giphy/v1/gifs/search", heypsterKey},
	}
	for _, c := range cases {
		if r := get(t, app, "/gifs/search?q=x"+c.param); r.status != http.StatusOK {
			t.Fatalf("provider%s: status %d %s", c.param, r.status, r.raw)
		}
		reqs := up.requests()
		req := reqs[len(reqs)-1]
		if req.URL.Path != c.path || req.URL.Query().Get("api_key") != c.key {
			t.Errorf("provider%s: went to %s with the wrong key", c.param, req.URL.Path)
		}
	}

	calls := len(up.requests())
	for _, path := range []string{"/gifs/search?q=x&provider=tenor", "/gifs/trending?provider=giphy2"} {
		r := get(t, app, path)
		if r.status != http.StatusBadRequest || r.json["code"] != "gif_provider_unknown" || r.json["error"] != "unknown gif provider" {
			t.Errorf("%s: %d %s", path, r.status, r.raw)
		}
	}
	if len(up.requests()) != calls {
		t.Error("an unknown provider must not reach any upstream")
	}
}

func TestNotConfigured(t *testing.T) {
	up := newFakeUpstream(t)
	app := newTestApp(newTestService(config.GifsConfig{GiphyAPIKey: giphyKey}, up.srv.URL))

	for _, path := range []string{"/gifs/search?q=x&provider=heypster", "/gifs/trending?provider=heypster"} {
		r := get(t, app, path)
		if r.status != http.StatusNotFound || r.json["code"] != "gif_not_configured" || r.json["error"] != "gif provider not configured" {
			t.Errorf("%s: %d %s", path, r.status, r.raw)
		}
	}
	if len(up.requests()) != 0 {
		t.Error("a provider without a key must not be called")
	}

	// No keys at all: the default provider is not configured either.
	app = newTestApp(newTestService(config.GifsConfig{}, up.srv.URL))
	if r := get(t, app, "/gifs/trending"); r.status != http.StatusNotFound || r.json["code"] != "gif_not_configured" {
		t.Errorf("no keys: %d %s", r.status, r.raw)
	}
}

func TestSearchRequiresQuery(t *testing.T) {
	up := newFakeUpstream(t)
	app := newTestApp(newTestService(bothKeys, up.srv.URL))
	for _, path := range []string{"/gifs/search", "/gifs/search?q=", "/gifs/search?q=%20%20%09"} {
		r := get(t, app, path)
		if r.status != http.StatusBadRequest || r.json["error"] != "query is required" {
			t.Errorf("%s: %d %s", path, r.status, r.raw)
		}
	}
	if len(up.requests()) != 0 {
		t.Error("an empty search must not reach the provider")
	}
}

func TestUpstreamErrorMapping(t *testing.T) {
	up := newFakeUpstream(t)
	logs := captureLogs(t)
	app := newTestApp(newTestService(bothKeys, up.srv.URL))

	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"server error", http.StatusInternalServerError, `{"message":"boom"}`, http.StatusBadGateway},
		{"bad key", http.StatusForbidden, `{"message":"Invalid authentication credentials ` + giphyKey + `"}`, http.StatusBadGateway},
		{"not found", http.StatusNotFound, `{}`, http.StatusBadGateway},
		{"quota", http.StatusTooManyRequests, `{"message":"API rate limit exceeded"}`, http.StatusServiceUnavailable},
		{"not json", http.StatusOK, `<html>maintenance</html>`, http.StatusBadGateway},
		{"wrong shape", http.StatusOK, `{"data":{"id":"x"}}`, http.StatusBadGateway},
		{"too large", http.StatusOK, `{"data":[],"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`, http.StatusBadGateway},
	}
	for _, c := range cases {
		up.set(c.status, c.body)
		for _, path := range []string{"/gifs/search?q=secret+search", "/gifs/trending"} {
			r := get(t, app, path)
			if r.status != c.want || r.json["code"] != "gif_upstream" || r.json["error"] != "gif provider unavailable" {
				t.Errorf("%s %s: %d %s", c.name, path, r.status, r.raw)
			}
			assertNoKey(t, c.name+" response", r.raw)
		}
	}
	out := logs.String()
	assertNoKey(t, "log", out)
	if strings.Contains(out, "secret") {
		t.Errorf("the log carries the user's search text: %s", out)
	}
	if !strings.Contains(out, "giphy") || !strings.Contains(out, "429") {
		t.Errorf("the log should name the provider and status: %s", out)
	}
}

// A redirect is not followed: it would re-send the key to wherever Location points.
func TestUpstreamRedirectNotFollowed(t *testing.T) {
	var elsewhere int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		_, _ = io.WriteString(w, okBody)
	}))
	defer target.Close()

	up := newFakeUpstream(t)
	up.mu.Lock()
	up.status, up.body = http.StatusFound, ""
	up.header = map[string]string{"Location": target.URL + "/steal"}
	up.mu.Unlock()

	app := newTestApp(newTestService(bothKeys, up.srv.URL))
	r := get(t, app, "/gifs/search?q=x")
	if r.status != http.StatusBadGateway || r.json["code"] != "gif_upstream" {
		t.Errorf("redirect: %d %s", r.status, r.raw)
	}
	if elsewhere != 0 {
		t.Error("the redirect was followed")
	}
}

// net/http puts the full request URL - api_key included - into every transport error. None
// of that may reach the client or the log.
func TestTransportErrorsDoNotLeakTheKey(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close() // nothing listens there now: connection refused

	for _, base := range []string{closed.URL, "http://bad host"} {
		logs := captureLogs(t)
		app := newTestApp(newTestService(bothKeys, base))
		for _, path := range []string{"/gifs/search?q=secret+search", "/gifs/search?q=secret&provider=heypster", "/gifs/trending"} {
			r := get(t, app, path)
			if r.status != http.StatusBadGateway || r.json["code"] != "gif_upstream" {
				t.Errorf("%s via %s: %d %s", path, base, r.status, r.raw)
			}
			assertNoKey(t, "response", r.raw)
		}
		out := logs.String()
		if out == "" {
			t.Fatalf("%s: the failure was not logged", base)
		}
		assertNoKey(t, "log", out)
		if strings.Contains(out, "secret") {
			t.Errorf("the log carries the user's search text: %s", out)
		}
	}

	// And at the source: the service's own error never names the key either.
	svc := newTestService(bothKeys, closed.URL)
	_, err := svc.Search(context.Background(), "giphy", "x", 1, 0)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
	assertNoKey(t, "error", err.Error())
}

func TestScrub(t *testing.T) {
	s := NewService(config.GifsConfig{GiphyAPIKey: giphyKey, HeypsterAPIKey: "with space&amp"}, nil, "")
	cases := []error{
		&url.Error{Op: "Get", URL: "https://api.giphy.com/v1/gifs/search?api_key=" + giphyKey + "&q=secret", Err: errors.New("dial tcp: connection refused")},
		fmt.Errorf("wrapped: %w", &url.Error{Op: "Get", URL: "https://x/?api_key=" + giphyKey, Err: &url.Error{Op: "Get", URL: "https://y/?q=secret", Err: errors.New("inner")}}),
		fmt.Errorf("a cause that quotes %s?api_key=%s&q=secret", "https://api.giphy.com/v1/gifs/search", giphyKey),
		errors.New("key on its own: " + giphyKey),
		errors.New("escaped: api_key=" + url.QueryEscape("with space&amp") + "&limit=1"),
		errors.New("raw: with space&amp"),
	}
	for _, err := range cases {
		got := s.scrub(err)
		if strings.Contains(got, giphyKey) || strings.Contains(got, "with space&amp") || strings.Contains(got, url.QueryEscape("with space&amp")) || strings.Contains(got, "secret") {
			t.Errorf("scrub(%q) = %q", err, got)
		}
	}
	if got := s.scrub(cases[0]); got != "dial tcp: connection refused" {
		t.Errorf("scrub should keep the cause and drop the URL, got %q", got)
	}
}

// The provider's whole answer does not pass through: only the normalized fields do, so a key
// echoed anywhere else in the upstream body never reaches the client.
func TestSuccessResponseCarriesOnlyNormalizedFields(t *testing.T) {
	up := newFakeUpstream(t)
	up.set(http.StatusOK, `{"data":[{"id":"g1","url":"https://giphy.com/x?api_key=`+giphyKey+`",
		"analytics":{"onload":{"url":"https://giphy.com/a?api_key=`+giphyKey+`"}},
		"images":{"fixed_width":{"url":"https://media.example/fw.gif","webp":"https://w?api_key=`+giphyKey+`"}}}],
		"meta":{"msg":"OK","api_key":"`+giphyKey+`"},"pagination":{"offset":0,"count":1,"total_count":1}}`)
	app := newTestApp(newTestService(bothKeys, up.srv.URL))
	r := get(t, app, "/gifs/search?q=x")
	if r.status != http.StatusOK {
		t.Fatalf("status %d %s", r.status, r.raw)
	}
	assertNoKey(t, "response", r.raw)
}

// memCache records what the service caches.
type memCache struct {
	mu   sync.Mutex
	data map[string][]byte
	ttl  map[string]time.Duration
}

func newMemCache() *memCache {
	return &memCache{data: map[string][]byte{}, ttl: map[string]time.Duration{}}
}

func (m *memCache) get(_ context.Context, key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	return b, ok
}

func (m *memCache) set(_ context.Context, key string, val []byte, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = val
	m.ttl[key] = ttl
}

// Trending pages are cached for two minutes per provider/limit/offset; a search is never
// cached, because the search text is the user's.
func TestTrendingCachedSearchNot(t *testing.T) {
	up := newFakeUpstream(t)
	svc := newTestService(bothKeys, up.srv.URL)
	cache := newMemCache()
	svc.cache = cache
	app := newTestApp(svc)

	first := get(t, app, "/gifs/trending")
	second := get(t, app, "/gifs/trending?provider=giphy&limit=24&offset=0")
	if first.status != http.StatusOK || second.raw != first.raw {
		t.Fatalf("cached page differs: %s vs %s", first.raw, second.raw)
	}
	if n := len(up.requests()); n != 1 {
		t.Errorf("same trending page twice: %d upstream calls, want 1", n)
	}
	if ttl := cache.ttl["test:gifs:trending:giphy:24:0"]; ttl != 120*time.Second {
		t.Errorf("cache keys %v, want test:gifs:trending:giphy:24:0 for 120s", cache.ttl)
	}

	// Another page, another provider: separate entries.
	get(t, app, "/gifs/trending?offset=24")
	get(t, app, "/gifs/trending?provider=heypster")
	if n := len(up.requests()); n != 3 {
		t.Errorf("distinct pages: %d upstream calls, want 3", n)
	}

	// An upstream failure is not cached.
	up.set(http.StatusInternalServerError, `{}`)
	get(t, app, "/gifs/trending?offset=48")
	up.set(http.StatusOK, okBody)
	if r := get(t, app, "/gifs/trending?offset=48"); r.status != http.StatusOK {
		t.Errorf("a failed page was cached: %d", r.status)
	}

	entries := len(cache.data)
	get(t, app, "/gifs/search?q=private+words")
	get(t, app, "/gifs/search?q=private+words")
	if n := len(up.requests()); n != 7 {
		t.Errorf("searches must always go upstream: %d calls, want 7", n)
	}
	if len(cache.data) != entries {
		t.Error("a search was cached")
	}
	for k, v := range cache.data {
		if strings.Contains(k, "private") || strings.Contains(string(v), "private") {
			t.Errorf("search text in the cache: %s", k)
		}
	}
}

// Without Redis there is no cache, and trending still works.
func TestTrendingWithoutRedis(t *testing.T) {
	up := newFakeUpstream(t)
	app := newTestApp(newTestService(bothKeys, up.srv.URL))
	for range 2 {
		if r := get(t, app, "/gifs/trending"); r.status != http.StatusOK {
			t.Fatalf("status %d %s", r.status, r.raw)
		}
	}
	if n := len(up.requests()); n != 2 {
		t.Errorf("%d upstream calls, want 2", n)
	}
}

func TestConfiguredProviders(t *testing.T) {
	cases := []struct {
		cfg  config.GifsConfig
		want string
	}{
		{config.GifsConfig{}, ""},
		{config.GifsConfig{GiphyAPIKey: "k"}, "giphy"},
		{config.GifsConfig{HeypsterAPIKey: "k"}, "heypster"},
		{bothKeys, "giphy,heypster"},
	}
	for _, c := range cases {
		got := ConfiguredProviders(c.cfg)
		if got == nil || strings.Join(got, ",") != c.want {
			t.Errorf("ConfiguredProviders(%+v) = %#v, want %q", c.cfg, got, c.want)
		}
	}
}
