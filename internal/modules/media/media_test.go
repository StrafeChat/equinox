package media

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/redistest"
)

const (
	testKey    = "0123456789abcdef0123456789abcdef-test-key"
	testPublic = "https://a.example"
	testPrefix = "t:"
	testSecret = "nebula-secret"
)

func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Media = config.MediaProxyConfig{Enabled: true, Key: testKey, MaxBytes: 1 << 20, CacheTTL: 7 * 24 * time.Hour, CacheMaxBytes: 1 << 30, AllowPrivate: true}
	cfg.Federation.PublicURL = testPublic + "/"
	cfg.Database.Redis.CachePrefix = testPrefix
	return cfg
}

func newTestService(t *testing.T, cfg *config.Config, rdb *redis.Client) *Service {
	t.Helper()
	s, err := New(context.Background(), cfg, rdb)
	if err != nil || s == nil {
		t.Fatalf("New: %v, %v", s, err)
	}
	return s
}

// ---- test images and servers ---------------------------------------------------------------

func encoded(t *testing.T, enc func(io.Writer, image.Image) error, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := enc(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngOf(t *testing.T, w, h int) []byte { return encoded(t, png.Encode, w, h) }

// pngClaiming is a valid PNG whose header claims w x h (checksum and all), as a
// decompression bomb's would.
func pngClaiming(t *testing.T, w, h int) []byte {
	b := pngOf(t, 1, 1)
	binary.BigEndian.PutUint32(b[16:20], uint32(w))
	binary.BigEndian.PutUint32(b[20:24], uint32(h))
	binary.BigEndian.PutUint32(b[29:33], crc32.ChecksumIEEE(b[12:29]))
	return b
}

// webpOf is the header of an extended (VP8X) WebP of w x h - all a sniff and a size probe read.
func webpOf(w, h int) []byte {
	b := make([]byte, 30)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], 22)
	copy(b[8:], "WEBPVP8X")
	binary.LittleEndian.PutUint32(b[16:20], 10)
	b[24], b[25], b[26] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
	b[27], b[28], b[29] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
	return b
}

type answer struct {
	status      int
	contentType string
	body        []byte
	chunked     bool
	delay       time.Duration
	redirect    string
}

// source is the site an embed points at: it answers each path as told and counts the fetches,
// and records the headers it was sent.
type source struct {
	srv     *httptest.Server
	mu      sync.Mutex
	answers map[string]answer
	hits    map[string]int
	headers []http.Header
}

func newSource(t *testing.T) *source {
	s := &source{answers: map[string]answer{}, hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		a, ok := s.answers[r.URL.Path]
		s.hits[r.URL.Path]++
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		time.Sleep(a.delay)
		if a.redirect != "" {
			http.Redirect(w, r, a.redirect, http.StatusFound)
			return
		}
		if a.contentType != "" {
			w.Header().Set("Content-Type", a.contentType)
		}
		if !a.chunked {
			w.Header().Set("Content-Length", strconv.Itoa(len(a.body)))
		}
		w.WriteHeader(a.status)
		if a.chunked {
			for i := 0; i < len(a.body); i += 4096 {
				w.Write(a.body[i:min(i+4096, len(a.body))])
				w.(http.Flusher).Flush()
			}
			return
		}
		w.Write(a.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *source) serve(path string, a answer) string {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	s.mu.Lock()
	s.answers[path] = a
	s.mu.Unlock()
	return s.srv.URL + path
}

func (s *source) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// fakeNebula is the object store: PUT (with the upload secret), GET and DELETE under /v1/.
// refuseDelete, when set, fails the deletes of the keys it names.
type fakeNebula struct {
	srv          *httptest.Server
	mu           sync.Mutex
	objects      map[string][]byte
	deletes      int
	refuseDelete func(key string) bool
}

func newFakeNebula(t *testing.T) *fakeNebula {
	n := &fakeNebula{objects: map[string][]byte{}}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/v1/")
		n.mu.Lock()
		defer n.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("Authorization") != "Bearer "+testSecret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			b, _ := io.ReadAll(r.Body)
			n.objects[key] = b
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			b, ok := n.objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		case http.MethodDelete:
			n.deletes++
			if n.refuseDelete != nil && n.refuseDelete(key) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			delete(n.objects, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNebula) put(key string, b []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.objects[key] = b
}

func (n *fakeNebula) drop(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.objects, key)
}

func (n *fakeNebula) deleteCalls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.deletes
}

// cachedService is a proxy that keeps what it fetches in a fake Nebula.
func cachedService(t *testing.T, cfg *config.Config) (*Service, *fakeNebula, *redis.Client, *redistest.Redis) {
	t.Helper()
	rdb, fr := redistest.New()
	neb := newFakeNebula(t)
	cfg.Nebula.BaseURL = neb.srv.URL
	cfg.Nebula.UploadSecret = testSecret
	return newTestService(t, cfg, rdb), neb, rdb, fr
}

// cachedBytes is what the cache counts as stored.
func cachedBytes(t *testing.T, s *Service, rdb *redis.Client) int64 {
	t.Helper()
	n, err := rdb.Get(context.Background(), s.bytesKey()).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		t.Fatal(err)
	}
	return n
}

func (n *fakeNebula) keys() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for k := range n.objects {
		out = append(out, k)
	}
	return out
}

func readImage(t *testing.T, img *Image, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer img.Body.Close()
	b, err := io.ReadAll(img.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- signing --------------------------------------------------------------------------------

func TestProxyURLsAreSigned(t *testing.T) {
	s := newTestService(t, testConfig(), nil)
	src := "https://img.example/a b.png?x=1"
	got := s.ProxyURL(src)

	// The contract: base64url (no padding) of the first 16 bytes of
	// HMAC-SHA256(K, "strafe-media-proxy/v1\x00" + url), then base64url of the url.
	m := hmac.New(sha256.New, []byte(testKey))
	m.Write([]byte("strafe-media-proxy/v1\x00" + src))
	sig := base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
	enc := base64.RawURLEncoding.EncodeToString([]byte(src))
	if want := testPublic + "/media/v1/external/" + sig + "/" + enc; got != want {
		t.Fatalf("ProxyURL = %q, want %q", got, want)
	}
	if back, ok := s.Source(sig, enc); !ok || back != src {
		t.Fatalf("Source = %q, %v", back, ok)
	}

	// Without a public URL the address is a path the client resolves against its API base.
	cfg := testConfig()
	cfg.Federation.PublicURL = ""
	if got := newTestService(t, cfg, nil).ProxyURL(src); !strings.HasPrefix(got, RoutePrefix+sig+"/") {
		t.Fatalf("relative ProxyURL = %q", got)
	}

	// Nothing the proxy would not fetch is signed.
	for _, bad := range []string{"", "javascript:alert(1)", "ftp://img.example/a.png", "https://user:pw@img.example/a.png",
		"//img.example/a.png", "https://img.example/" + strings.Repeat("a", maxSourceLen)} {
		if got := s.ProxyURL(bad); got != "" {
			t.Errorf("ProxyURL(%q) = %q", bad, got)
		}
	}
	// A switched-off proxy signs nothing.
	var off *Service
	if off.ProxyURL(src) != "" || off.IsLocal(src) {
		t.Error("a nil proxy signed or owned something")
	}
}

func TestForgedSignaturesAreRefused(t *testing.T) {
	s := newTestService(t, testConfig(), nil)
	src := "https://img.example/cat.png"
	sig, enc, _ := strings.Cut(strings.TrimPrefix(s.ProxyURL(src), testPublic+RoutePrefix), "/")

	other := testConfig()
	other.Media.Key = strings.Repeat("z", 40)
	otherSig, _, _ := strings.Cut(strings.TrimPrefix(newTestService(t, other, nil).ProxyURL(src), testPublic+RoutePrefix), "/")
	elsewhere := base64.RawURLEncoding.EncodeToString([]byte("https://img.example/dog.png"))
	flip := func(s string) string {
		c := byte('A')
		if s[0] == 'A' {
			c = 'B'
		}
		return string(c) + s[1:]
	}

	for name, c := range map[string][2]string{
		"another key":         {otherSig, enc},
		"another address":     {sig, elsewhere},
		"a changed signature": {flip(sig), enc},
		"a short signature":   {sig[:sigLen-1], enc},
		"a long signature":    {sig + "A", enc},
		"not base64":          {strings.Repeat("!", sigLen), enc},
		"no address":          {sig, ""},
		"an undecodable addr": {sig, "%%%"},
	} {
		if got, ok := s.Source(c[0], c[1]); ok {
			t.Errorf("%s: accepted, source %q", name, got)
		}
	}

	// The key generated for an instance without MEDIA_PROXY_KEY is shared through Redis, so
	// every replica checks what another signed.
	rdb, _ := redistest.New()
	gen := testConfig()
	gen.Media.Key = ""
	a, b := newTestService(t, gen, rdb), newTestService(t, gen, rdb)
	if a.ProxyURL(src) == "" || a.ProxyURL(src) != b.ProxyURL(src) {
		t.Fatalf("replicas disagree: %q vs %q", a.ProxyURL(src), b.ProxyURL(src))
	}
	if a.ProxyURL(src) == s.ProxyURL(src) {
		t.Fatal("the generated key is the configured one")
	}
	if _, err := New(context.Background(), gen, nil); err == nil {
		t.Fatal("no key and no Redis to keep one in was accepted")
	}
}

func TestIsLocal(t *testing.T) {
	cfg := testConfig()
	cfg.Nebula.PublicURL = "https://cdn.a.example/"
	cfg.Nebula.BaseURL = "http://nebula:8080"
	s := newTestService(t, cfg, nil)
	for u, want := range map[string]bool{
		"https://cdn.a.example/v1/attachments/1/x.png": true,
		"http://nebula:8080/v1/emoji/2.png":            true,
		testPublic + RoutePrefix + "sig/src":           true,
		"https://cdn.a.example.evil/x.png":             false,
		"https://img.example/x.png":                    false,
	} {
		if got := s.IsLocal(u); got != want {
			t.Errorf("IsLocal(%q) = %v", u, got)
		}
	}
}

// ---- fetching ---------------------------------------------------------------------------------

func TestOnlyImagesAreServed(t *testing.T) {
	s := newTestService(t, testConfig(), nil)
	src := newSource(t)
	ctx := context.Background()

	// The bytes decide, never what the site says they are.
	for name, c := range map[string]struct {
		body []byte
		want string
		w, h int
	}{
		"png":  {pngOf(t, 3, 2), "image/png", 3, 2},
		"jpeg": {encoded(t, func(w io.Writer, i image.Image) error { return jpeg.Encode(w, i, nil) }, 4, 5), "image/jpeg", 4, 5},
		"gif":  {encoded(t, func(w io.Writer, i image.Image) error { return gif.Encode(w, i, nil) }, 6, 7), "image/gif", 6, 7},
		"webp": {webpOf(300, 200), "image/webp", 300, 200},
	} {
		u := src.serve("/"+name, answer{contentType: "text/html", body: c.body})
		img, err := s.Open(ctx, u, "")
		if got := readImage(t, img, err); !bytes.Equal(got, c.body) || img.ContentType != c.want {
			t.Errorf("%s: served as %q (%d bytes)", name, img.ContentType, len(got))
		}
		if w, h, ok := s.Warm(ctx, u); !ok || w != c.w || h != c.h {
			t.Errorf("%s: size %dx%d %v", name, w, h, ok)
		}
	}

	for name, body := range map[string]string{
		"svg":   `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"html":  `<!DOCTYPE html><html><script>alert(1)</script></html>`,
		"text":  "just some text",
		"empty": "",
	} {
		u := src.serve("/"+name, answer{contentType: "image/png", body: []byte(body)})
		if _, err := s.Open(ctx, u, ""); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A truncated image (the type is there, the size is not) is not served either.
	u := src.serve("/cut", answer{contentType: "image/png", body: pngOf(t, 2, 2)[:12]})
	if _, err := s.Open(ctx, u, ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("truncated: %v", err)
	}

	// Nothing of the reader's goes to the site: a fixed user agent, no cookie, no referer.
	src.mu.Lock()
	h := src.headers[0]
	src.mu.Unlock()
	if h.Get("User-Agent") != userAgent || h.Get("Cookie") != "" || h.Get("Referer") != "" {
		t.Errorf("sent %v", h)
	}
}

func TestSizeLimits(t *testing.T) {
	s := newTestService(t, testConfig(), nil)
	src := newSource(t)
	ctx := context.Background()
	big := append(pngOf(t, 1, 1), make([]byte, s.cfg.MaxBytes)...)

	for name, a := range map[string]answer{
		"declared":   {contentType: "image/png", body: big},
		"undeclared": {contentType: "image/png", body: big, chunked: true},
	} {
		if _, err := s.Open(ctx, src.serve("/"+name, a), ""); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Exactly the limit is fine.
	fits := append(pngOf(t, 1, 1), make([]byte, s.cfg.MaxBytes-len(pngOf(t, 1, 1)))...)
	if _, err := s.Open(ctx, src.serve("/fits", answer{body: fits, chunked: true}), ""); err != nil {
		t.Errorf("an image of exactly MEDIA_PROXY_MAX_MB: %v", err)
	}

	// Small on the wire, enormous on a reader's screen.
	if _, err := s.Open(ctx, src.serve("/bomb", answer{body: pngClaiming(t, maxSide+1, 10)}), ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("past %d px: %v", maxSide, err)
	}
	// Neither side too long, but the area a gigabyte of pixels: as much a bomb.
	if _, err := s.Open(ctx, src.serve("/square", answer{body: pngClaiming(t, maxSide, maxSide)}), ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("%d px square: %v", maxSide, err)
	}
	if _, err := s.Open(ctx, src.serve("/area", answer{body: pngClaiming(t, 8000, 8000)}), ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("64 megapixels: %v", err)
	}
	if _, err := s.Open(ctx, src.serve("/edge", answer{body: pngClaiming(t, maxSide, maxPixels/maxSide)}), ""); err != nil {
		t.Errorf("%d px a side, under %d pixels: %v", maxSide, maxPixels, err)
	}
}

func TestUpstreamFailures(t *testing.T) {
	s := newTestService(t, testConfig(), nil)
	src := newSource(t)
	ctx := context.Background()
	cases := map[string]struct {
		a    answer
		want *Error
	}{
		"gone":      {answer{status: http.StatusNotFound}, ErrNotFound},
		"forbidden": {answer{status: http.StatusForbidden}, ErrNotFound},
		"broken":    {answer{status: http.StatusInternalServerError}, ErrUpstream},
		"loop":      {answer{redirect: "/loop"}, ErrUpstream},
	}
	for name, c := range cases {
		if _, err := s.Open(ctx, src.serve("/"+name, c.a), ""); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if n := src.count("/loop"); n != maxRedirects+1 {
		t.Errorf("followed %d hops, want %d", n, maxRedirects+1)
	}
	// One redirect to the picture is followed.
	img := src.serve("/real.png", answer{body: pngOf(t, 2, 2)})
	if _, err := s.Open(ctx, src.serve("/moved", answer{redirect: img}), ""); err != nil {
		t.Errorf("a redirect: %v", err)
	}
}

func TestTheProxyStaysOffPrivateAddresses(t *testing.T) {
	cfg := testConfig()
	cfg.Media.AllowPrivate = false
	s := newTestService(t, cfg, nil)
	src := newSource(t)
	ctx := context.Background()

	// The test server is on loopback, like this host's own services.
	u := src.serve("/secret.png", answer{body: pngOf(t, 1, 1)})
	local := strings.Replace(u, "127.0.0.1", "localhost", 1)
	for _, target := range []string{u, local} {
		if _, err := s.Open(ctx, target, ""); !errors.Is(err, ErrUpstream) {
			t.Errorf("%s: %v", target, err)
		}
		if _, _, ok := s.Warm(ctx, target); ok {
			t.Errorf("%s: warmed", target)
		}
	}
	if n := src.count("/secret.png"); n != 0 {
		t.Fatalf("a private address was fetched %d times", n)
	}
}

func TestOneFetchForManyReaders(t *testing.T) {
	s := newTestService(t, testConfig(), nil)
	src := newSource(t)
	u := src.serve("/slow.png", answer{body: pngOf(t, 2, 2), delay: 200 * time.Millisecond})
	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			img, err := s.Open(context.Background(), u, "ip:"+strconv.Itoa(i))
			if err != nil {
				failed.Add(1)
				return
			}
			io.Copy(io.Discard, img.Body)
			img.Body.Close()
		}()
	}
	wg.Wait()
	if failed.Load() != 0 || src.count("/slow.png") != 1 {
		t.Fatalf("%d failed, %d fetches", failed.Load(), src.count("/slow.png"))
	}
}

// ---- the cache ----------------------------------------------------------------------------------

func TestCachedImagesAreNotFetchedAgain(t *testing.T) {
	rdb, fr := redistest.New()
	neb := newFakeNebula(t)
	cfg := testConfig()
	cfg.Nebula.BaseURL = neb.srv.URL
	cfg.Nebula.UploadSecret = testSecret
	s := newTestService(t, cfg, rdb)
	src := newSource(t)
	ctx := context.Background()
	body := pngOf(t, 4, 3)
	u := src.serve("/cat.png", answer{body: body})

	// Warming at post time is the one fetch; the image goes into Nebula under its keyed hash.
	if w, h, ok := s.Warm(ctx, u); !ok || w != 4 || h != 3 {
		t.Fatalf("warm: %dx%d %v", w, h, ok)
	}
	h := s.hash(u)
	if keys := neb.keys(); len(keys) != 1 || keys[0] != "media-cache/"+h+".png" {
		t.Fatalf("nebula holds %v", keys)
	}
	if strings.Contains(strings.Join(fr.Strings(), " "), "cat") {
		t.Fatalf("redis names the source: %v", fr.Strings())
	}
	if ttl := fr.TTL(s.recordKey(h)); ttl != cfg.Media.CacheTTL {
		t.Fatalf("the record is kept for %v", ttl)
	}

	// Every view after that - from this replica or another - is the cached copy.
	other := newTestService(t, cfg, rdb)
	for _, svc := range []*Service{s, other, s} {
		img, err := svc.Open(ctx, u, "ip:reader")
		if got := readImage(t, img, err); !bytes.Equal(got, body) || img.ContentType != "image/png" {
			t.Fatalf("served %d bytes as %q", len(got), img.ContentType)
		}
	}
	if w, h, ok := other.Warm(ctx, u); !ok || w != 4 || h != 3 {
		t.Fatalf("warm again: %dx%d %v", w, h, ok)
	}
	if n := src.count("/cat.png"); n != 1 {
		t.Fatalf("fetched %d times", n)
	}

	// The sweeper deletes it from Nebula once MEDIA_PROXY_CACHE_TTL is up, not before.
	s.sweep(ctx)
	if len(neb.keys()) != 1 {
		t.Fatal("swept before its time")
	}
	s.now = func() time.Time { return time.Now().Add(cfg.Media.CacheTTL + time.Minute) }
	s.sweep(ctx)
	if len(neb.keys()) != 0 || len(fr.SortedSet(testPrefix+"media:cache")) != 0 {
		t.Fatalf("after the sweep: nebula %v, index %v", neb.keys(), fr.SortedSet(testPrefix+"media:cache"))
	}
	// A view after that fetches it again rather than failing.
	img, err := s.Open(ctx, u, "ip:reader")
	if got := readImage(t, img, err); !bytes.Equal(got, body) || src.count("/cat.png") != 2 {
		t.Fatalf("after the sweep: %d bytes, %d fetches", len(got), src.count("/cat.png"))
	}
}

// The cache never holds more than MEDIA_PROXY_CACHE_MAX_GB: past it an image is still
// served, just not kept, and what the sweeper deletes makes room again.
func TestTheCacheStaysWithinItsSize(t *testing.T) {
	cfg := testConfig()
	body := pngOf(t, 8, 8)
	cfg.Media.CacheMaxBytes = int64(3 * len(body))
	s, neb, rdb, fr := cachedService(t, cfg)
	src := newSource(t)
	ctx := context.Background()
	img := func(n int) string { return src.serve("/"+strconv.Itoa(n)+".png", answer{body: body}) }

	for i := 0; i < 5; i++ {
		if _, _, ok := s.Warm(ctx, img(i)); !ok {
			t.Fatalf("warm %d failed", i)
		}
	}
	if keys := neb.keys(); len(keys) != 3 {
		t.Fatalf("nebula holds %d objects, want 3: %v", len(keys), keys)
	}
	if got := cachedBytes(t, s, rdb); got != cfg.Media.CacheMaxBytes {
		t.Fatalf("counted %d bytes, want %d", got, cfg.Media.CacheMaxBytes)
	}
	if n := len(fr.SortedSet(s.indexKey())); n != 3 {
		t.Fatalf("%d index entries", n)
	}
	// Past the cap the image is served all the same - fetched for the view, not kept.
	if got := mustOpen(t, s, img(4), "ip:reader"); !bytes.Equal(got, body) {
		t.Fatal("an image past the cap was not served")
	}
	if src.count("/4.png") != 2 || len(neb.keys()) != 3 {
		t.Fatalf("%d fetches, nebula %v", src.count("/4.png"), neb.keys())
	}

	// The same image fetched again (its object was lost) is counted once.
	lost := objectKey(s.hash(img(0)), "png")
	neb.drop(lost)
	mustOpen(t, s, img(0), "ip:reader")
	if got := cachedBytes(t, s, rdb); got != cfg.Media.CacheMaxBytes || len(neb.keys()) != 3 {
		t.Fatalf("after a re-fetch: %d bytes counted, nebula %v", got, neb.keys())
	}

	// Swept, the bytes are given back and the next image is kept again.
	s.now = func() time.Time { return time.Now().Add(cfg.Media.CacheTTL + time.Minute) }
	s.sweep(ctx)
	if got := cachedBytes(t, s, rdb); got != 0 || len(neb.keys()) != 0 {
		t.Fatalf("after the sweep: %d bytes counted, nebula %v", got, neb.keys())
	}
	mustOpen(t, s, img(4), "ip:reader")
	if got := cachedBytes(t, s, rdb); got != int64(len(body)) || len(neb.keys()) != 1 {
		t.Fatalf("after room was made: %d bytes counted, nebula %v", got, neb.keys())
	}
}

// An upload that fails gives its bytes back at the next sweep, not after MEDIA_PROXY_CACHE_TTL.
func TestAFailedUploadIsNotCountedForLong(t *testing.T) {
	cfg := testConfig()
	s, neb, rdb, _ := cachedService(t, cfg)
	s.nebula.UploadSecret = "wrong"
	src := newSource(t)
	ctx := context.Background()
	if _, _, ok := s.Warm(ctx, src.serve("/a.png", answer{body: pngOf(t, 2, 2)})); !ok {
		t.Fatal("warm failed")
	}
	if len(neb.keys()) != 0 {
		t.Fatal("stored without the secret")
	}
	s.sweep(ctx)
	if got := cachedBytes(t, s, rdb); got != 0 {
		t.Fatalf("%d bytes still counted for an upload that failed", got)
	}
}

// A backlog larger than one batch is cleared in one sweep; a delete that fails is stepped
// over (left for next time) rather than retried forever.
func TestTheSweepClearsABacklog(t *testing.T) {
	cfg := testConfig()
	s, neb, rdb, fr := cachedService(t, cfg)
	ctx := context.Background()
	due := float64(time.Now().Add(-time.Hour).Unix())
	const n = 2*sweepBatch + 7
	stuck := map[string]bool{}
	for i := 0; i < n; i++ {
		h := fmt.Sprintf("%032x", i)
		neb.put(objectKey(h, "png"), []byte("x"))
		if err := rdb.ZAdd(ctx, s.indexKey(), redis.Z{Score: due, Member: entry(h, "png", 10)}).Err(); err != nil {
			t.Fatal(err)
		}
		if i%(sweepBatch/2) == 0 {
			stuck[objectKey(h, "png")] = true
		}
	}
	if err := rdb.Set(ctx, s.bytesKey(), 10*n, 0).Err(); err != nil {
		t.Fatal(err)
	}
	neb.refuseDelete = func(key string) bool { return stuck[key] }

	s.sweep(ctx)
	if left := neb.keys(); len(left) != len(stuck) {
		t.Fatalf("%d objects left after the sweep, want the %d whose delete failed", len(left), len(stuck))
	}
	if got, want := cachedBytes(t, s, rdb), int64(10*len(stuck)); got != want {
		t.Fatalf("%d bytes counted, want %d", got, want)
	}
	if left := fr.SortedSet(s.indexKey()); len(left) != len(stuck) {
		t.Fatalf("%d index entries left", len(left))
	}
	if calls := neb.deleteCalls(); calls != n {
		t.Fatalf("%d deletes for %d objects", calls, n)
	}
}

func TestWithoutNebulaImagesStreamThrough(t *testing.T) {
	rdb, _ := redistest.New()
	s := newTestService(t, testConfig(), rdb)
	src := newSource(t)
	ctx := context.Background()
	u := src.serve("/dog.png", answer{body: pngOf(t, 5, 6)})
	if w, h, ok := s.Warm(ctx, u); !ok || w != 5 || h != 6 {
		t.Fatalf("warm: %dx%d %v", w, h, ok)
	}
	// The size is remembered (a second warm costs nothing); the bytes are fetched per view.
	if _, _, ok := s.Warm(ctx, u); !ok || src.count("/dog.png") != 1 {
		t.Fatalf("warm again fetched: %d", src.count("/dog.png"))
	}
	mustOpen(t, s, u, "ip:reader")
	if src.count("/dog.png") != 2 {
		t.Fatalf("%d fetches", src.count("/dog.png"))
	}
}

func TestFailuresAreRemembered(t *testing.T) {
	rdb, fr := redistest.New()
	s := newTestService(t, testConfig(), rdb)
	src := newSource(t)
	ctx := context.Background()
	for path, c := range map[string]struct {
		a    answer
		want *Error
	}{
		"/gone":   {answer{status: http.StatusNotFound}, ErrNotFound},
		"/page":   {answer{body: []byte("<html></html>")}, ErrUnsupported},
		"/broken": {answer{status: http.StatusBadGateway}, ErrUpstream},
	} {
		u := src.serve(path, c.a)
		for i := 0; i < 3; i++ {
			if _, err := s.Open(ctx, u, "ip:reader"); !errors.Is(err, c.want) {
				t.Errorf("%s view %d: %v", path, i, err)
			}
		}
		if _, _, ok := s.Warm(ctx, u); ok {
			t.Errorf("%s warmed", path)
		}
		if n := src.count(path); n != 1 {
			t.Errorf("%s fetched %d times", path, n)
		}
		if ttl := fr.TTL(s.failureKey(s.hash(u))); ttl != failureTTL {
			t.Errorf("%s remembered for %v", path, ttl)
		}
	}
}

func TestMissesAreLimitedPerClient(t *testing.T) {
	rdb, _ := redistest.New()
	neb := newFakeNebula(t)
	cfg := testConfig()
	cfg.Nebula.BaseURL = neb.srv.URL
	cfg.Nebula.UploadSecret = testSecret
	s := newTestService(t, cfg, rdb)
	s.misses = newWindowLimiter(2, time.Minute)
	src := newSource(t)
	ctx := context.Background()
	img := func(n int) string { return src.serve("/"+strconv.Itoa(n)+".png", answer{body: pngOf(t, 1, 1)}) }

	for i := 0; i < 2; i++ {
		mustOpen(t, s, img(i), "ip:a")
	}
	if _, err := s.Open(ctx, img(2), "ip:a"); !errors.Is(err, ErrBusy) {
		t.Fatalf("third fetch in a minute: %v", err)
	}
	if src.count("/2.png") != 0 {
		t.Fatal("a refused miss was fetched")
	}
	// What is cached costs nothing; another reader has their own budget; the server's own
	// warming has none; the next minute starts afresh.
	mustOpen(t, s, img(0), "ip:a")
	mustOpen(t, s, img(3), "ip:b")
	if _, _, ok := s.Warm(ctx, img(4)); !ok {
		t.Fatal("warming was limited")
	}
	s.now = func() time.Time { return time.Now().Add(time.Minute) }
	mustOpen(t, s, img(2), "ip:a")
}

// However many readers there are, only missFetches images are fetched for views at once;
// past that a view is refused with ErrFull rather than queued, and a reader of an image
// already being fetched shares its fetch.
func TestMissFetchesAreBounded(t *testing.T) {
	s := newTestService(t, testConfig(), nil)
	s.fetching = make(chan struct{}, 1)
	src := newSource(t)
	ctx := context.Background()
	slow := src.serve("/slow.png", answer{body: pngOf(t, 2, 2), delay: 300 * time.Millisecond})
	other := src.serve("/other.png", answer{body: pngOf(t, 2, 2)})

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			img, err := s.Open(ctx, slow, "ip:"+strconv.Itoa(i))
			if err == nil {
				img.Body.Close()
			}
			done <- err
		}()
	}
	for deadline := time.Now().Add(2 * time.Second); src.count("/slow.png") == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the slow fetch never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.Open(ctx, other, "ip:c"); !errors.Is(err, ErrFull) {
		t.Fatalf("a second fetch while the only slot is taken: %v", err)
	}
	if src.count("/other.png") != 0 {
		t.Fatal("a refused view was fetched")
	}
	// The server's own warming is not a view and has its own bound.
	if _, _, ok := s.Warm(ctx, src.serve("/warm.png", answer{body: pngOf(t, 1, 1)})); !ok {
		t.Fatal("warming was refused")
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("a reader of the slow image: %v", err)
		}
	}
	if src.count("/slow.png") != 1 {
		t.Fatalf("the slow image was fetched %d times", src.count("/slow.png"))
	}
	// Refusing it remembered nothing about the source: once the slot is free it is fetched.
	mustOpen(t, s, other, "ip:c")
}

// ---- the route ----------------------------------------------------------------------------------

func TestHandler(t *testing.T) {
	s := newTestService(t, testConfig(), nil)
	s.misses = newWindowLimiter(1, time.Minute)
	src := newSource(t)
	app := fiber.New()
	app.Get(RoutePrefix+":sig/:src", NewHandler(s, func(fiber.Ctx) string { return "ip:test" }).External)
	get := func(u string) *http.Response {
		t.Helper()
		res, err := app.Test(httptest.NewRequest(http.MethodGet, strings.TrimPrefix(u, testPublic), nil), fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	safety := func(name string, res *http.Response) {
		t.Helper()
		for h, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"Content-Security-Policy":      "default-src 'none'; sandbox",
			"Referrer-Policy":              "no-referrer",
			"Cross-Origin-Resource-Policy": "cross-origin",
		} {
			if got := res.Header.Get(h); got != want {
				t.Errorf("%s: %s = %q", name, h, got)
			}
		}
	}

	body := pngOf(t, 2, 2)
	res := get(s.ProxyURL(src.serve("/ok.png", answer{contentType: "text/html", body: body})))
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, body) || res.Header.Get("Content-Type") != "image/png" ||
		res.Header.Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("ok: %d %q %q (%d bytes)", res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Cache-Control"), len(got))
	}
	safety("ok", res)

	// A forged address is refused before anything is fetched.
	forged := src.serve("/forged.png", answer{body: body})
	sig, _, _ := strings.Cut(strings.TrimPrefix(s.ProxyURL(src.srv.URL+"/ok.png"), testPublic+RoutePrefix), "/")
	res = get(testPublic + RoutePrefix + sig + "/" + base64.RawURLEncoding.EncodeToString([]byte(forged)))
	if res.StatusCode != http.StatusBadRequest || res.Header.Get("Cache-Control") != "no-store" || src.count("/forged.png") != 0 {
		t.Fatalf("forged: %d %q, %d fetches", res.StatusCode, res.Header.Get("Cache-Control"), src.count("/forged.png"))
	}
	safety("forged", res)

	// Over the reader's budget (one miss a minute here): 429 with a Retry-After.
	res = get(s.ProxyURL(src.serve("/more.png", answer{body: body})))
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("busy: %d %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	s.misses = newWindowLimiter(100, time.Minute)
	// Every fetch slot taken: 503, with a Retry-After, never cached.
	s.fetching = make(chan struct{})
	res = get(s.ProxyURL(src.serve("/full.png", answer{body: body})))
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("full: %d %q %q", res.StatusCode, res.Header.Get("Retry-After"), res.Header.Get("Cache-Control"))
	}
	safety("full", res)
	s.fetching = make(chan struct{}, missFetches)
	for path, c := range map[string]struct {
		a    answer
		want int
	}{
		"/missing.png": {answer{status: http.StatusNotFound}, http.StatusNotFound},
		"/huge.png":    {answer{body: append(pngOf(t, 1, 1), make([]byte, s.cfg.MaxBytes)...)}, http.StatusRequestEntityTooLarge},
		"/page.png":    {answer{body: []byte("<html></html>")}, http.StatusUnsupportedMediaType},
		"/down.png":    {answer{status: http.StatusServiceUnavailable}, http.StatusBadGateway},
	} {
		res := get(s.ProxyURL(src.serve(path, c.a)))
		if res.StatusCode != c.want || res.Header.Get("Cache-Control") != "public, max-age=900" {
			t.Errorf("%s: %d %q, want %d", path, res.StatusCode, res.Header.Get("Cache-Control"), c.want)
		}
		safety(path, res)
	}
}

func mustOpen(t *testing.T, s *Service, src, client string) []byte {
	t.Helper()
	img, err := s.Open(context.Background(), src, client)
	return readImage(t, img, err)
}
