package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/imageprobe"
)

const (
	fetchTimeout = 10 * time.Second
	maxRedirects = 5
	// maxSide is the largest width or height served, and maxPixels the largest area: past
	// either an image is more likely a decompression bomb aimed at readers' devices than a
	// picture (16384 x 16384 would be a gigabyte of pixels to every reader, from a file of a
	// few kilobytes).
	maxSide   = 16384
	maxPixels = 50_000_000
	// missesPerMinute is how many images one client address may make the proxy fetch (not
	// already cached) a minute. Cached images and remembered failures are free.
	missesPerMinute = 60
	// missFetches is how many images readers' views may have this process fetching at once,
	// whoever they are: each holds up to MEDIA_PROXY_MAX_MB in memory until it is kept, so
	// past it a view is refused (ErrFull) rather than queued. The server's own warming has
	// its own bound (messages.warmFetches).
	missFetches = 32
	// userAgent is the same for every fetch: nothing about the reader is passed on.
	userAgent = "Mozilla/5.0 (compatible; StrafeChat-MediaProxy/1.0; +https://strafe.chat)"
)

// Error is a refusal, with the HTTP status the proxy answers it with.
type Error struct {
	Status int
	Reason string
}

func (e *Error) Error() string { return "media: " + e.Reason }

var (
	ErrBadURL      = &Error{http.StatusBadRequest, "invalid media address"}
	ErrNotFound    = &Error{http.StatusNotFound, "image not found"}
	ErrTooLarge    = &Error{http.StatusRequestEntityTooLarge, "image too large"}
	ErrUnsupported = &Error{http.StatusUnsupportedMediaType, "not a supported image"}
	ErrUpstream    = &Error{http.StatusBadGateway, "could not fetch the image"}
	ErrBusy        = &Error{http.StatusTooManyRequests, "too many images fetched, try again shortly"}
	ErrFull        = &Error{http.StatusServiceUnavailable, "the image proxy is busy, try again shortly"}
)

// errorFor is the refusal a remembered status stands for.
func errorFor(status int) *Error {
	for _, e := range []*Error{ErrNotFound, ErrTooLarge, ErrUnsupported, ErrUpstream} {
		if e.Status == status {
			return e
		}
	}
	return ErrUpstream
}

// imageTypes are the formats served, by sniffed type, with the extension a cached object
// gets. No SVG (it is a document that can run script), no HTML, nothing else.
var imageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// fetched is one image fetched from its source and checked.
type fetched struct {
	data          []byte
	contentType   string
	ext           string
	width, height int
}

// Image is what the handler sends: the body is the caller's to close.
type Image struct {
	ContentType string
	Size        int64
	Body        io.ReadCloser
}

// Open returns the image a verified source URL names: from the cache when it is there,
// else fetched now (one fetch however many readers ask at once). client is the reader's
// rate-limit key; a fetch it causes counts against it.
func (s *Service) Open(ctx context.Context, src, client string) (*Image, error) {
	if s == nil || !proxiable(src) {
		return nil, ErrBadURL
	}
	h := s.hash(src)
	if status := s.failure(ctx, h); status != 0 {
		return nil, errorFor(status)
	}
	if rec := s.record(ctx, h); rec != nil && rec.Stored {
		if body, size, err := s.openStored(h, rec); err == nil {
			return &Image{ContentType: rec.ContentType, Size: size, Body: body}, nil
		}
		// The object is gone (swept, or Nebula lost it): forget it and fetch again.
		s.forget(ctx, h)
	}
	if !s.misses.allow(client, s.now()) {
		return nil, ErrBusy
	}
	f, err := s.fetchShared(src, h, s.fetching)
	if err != nil {
		return nil, err
	}
	return &Image{ContentType: f.contentType, Size: int64(len(f.data)), Body: io.NopCloser(bytes.NewReader(f.data))}, nil
}

// Warm fetches src into the cache ahead of anyone viewing it and reports its size. Used
// right after a message with embeds is written, so the site behind an image sees one fetch
// from this instance at post time rather than one per reader. Best-effort.
func (s *Service) Warm(ctx context.Context, src string) (int, int, bool) {
	if s == nil || !proxiable(src) {
		return 0, 0, false
	}
	h := s.hash(src)
	if s.failure(ctx, h) != 0 {
		return 0, 0, false
	}
	if rec := s.record(ctx, h); rec != nil {
		return rec.Width, rec.Height, rec.Width > 0 && rec.Height > 0
	}
	f, err := s.fetchShared(src, h, nil)
	if err != nil {
		return 0, 0, false
	}
	return f.width, f.height, true
}

// fetchShared fetches src once for everyone asking at the same time, keeps the result, and
// remembers a failure so a broken or hostile source is not fetched again on every view. The
// fetch runs on its own deadline: the first reader hanging up must not fail the others. With
// slots, the fetch takes one for as long as it holds the image, or fails with ErrFull when
// none is free (never remembered: it says nothing about the source); readers joining a fetch
// already under way share its slot.
func (s *Service) fetchShared(src, h string, slots chan struct{}) (*fetched, error) {
	v, err, _ := s.flight.Do(h, func() (any, error) {
		if slots != nil {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				return nil, ErrFull
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout+storeTimeout)
		defer cancel()
		f, err := s.fetch(ctx, src)
		if err != nil {
			var e *Error
			if errors.As(err, &e) {
				s.remember(ctx, h, e.Status)
			}
			return nil, err
		}
		s.keep(ctx, h, f)
		return f, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*fetched), nil
}

// fetch GETs src with nothing of the reader's (no cookies, no referer, a fixed user agent)
// and checks what comes back: an image of an allowed type and size, judged by its bytes,
// never by what the server says it is.
func (s *Service) fetch(ctx context.Context, src string) (*fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, ErrBadURL
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/webp,image/png,image/jpeg,image/gif;q=0.9,image/*;q=0.5")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, ErrUpstream
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, ErrUpstream
	}
	limit := int64(s.cfg.MaxBytes)
	if resp.ContentLength > limit {
		return nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, ErrUpstream
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return inspect(data)
}

// inspect sniffs an image's type from its bytes and reads its size from its header.
func inspect(data []byte) (*fetched, error) {
	ct := http.DetectContentType(data)
	ext, ok := imageTypes[ct]
	if !ok {
		return nil, ErrUnsupported
	}
	w, h, ok := imageprobe.Size(data)
	if !ok || w > maxSide || h > maxSide || int64(w)*int64(h) > maxPixels {
		return nil, ErrUnsupported
	}
	return &fetched{data: data, contentType: ct, ext: ext, width: w, height: h}, nil
}

// windowLimiter counts events per key in fixed windows, in process: the brake on one client
// making the proxy fetch a stream of new URLs. An empty key (the server's own warming) is
// never limited.
type windowLimiter struct {
	mu     sync.Mutex
	max    int
	per    time.Duration
	start  time.Time
	counts map[string]int
}

func newWindowLimiter(max int, per time.Duration) *windowLimiter {
	return &windowLimiter{max: max, per: per, counts: map[string]int{}}
}

func (l *windowLimiter) allow(key string, now time.Time) bool {
	if l == nil || key == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= l.per {
		l.start = now
		clear(l.counts)
	}
	if l.counts[key] >= l.max {
		return false
	}
	l.counts[key]++
	return true
}

// retryAfter is the Retry-After for a refusal that passes, in seconds: ErrBusy when the
// reader's minute is up, ErrFull once a fetch under way has had its time.
func retryAfter(e *Error) string {
	if e == ErrFull {
		return strconv.Itoa(int(fetchTimeout / time.Second))
	}
	return strconv.Itoa(int(time.Minute / time.Second))
}
