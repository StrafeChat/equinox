package gifs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/logger"
)

const (
	upstreamTimeout = 5 * time.Second
	// A page of 50 GIPHY items is well under 1 MiB; the cap is there so a misbehaving
	// upstream cannot make this process buffer something unbounded.
	maxBodyBytes = 4 << 20
	// A fixed identity: the provider learns that a StrafeChat instance asked, nothing about
	// which user did.
	userAgent = "Strafe/1.0 (+https://strafe.chat)"
	// Trending is the same for everyone, and is what every picker opens on, so a short
	// cache saves most of this instance's provider quota. Searches are never cached.
	trendingTTL = 120 * time.Second

	maxQueryRunes = 50 // GIPHY's own limit on q
	defaultLimit  = 24
	maxLimit      = 50
	maxOffset     = 4999 // GIPHY refuses offsets past this
)

// Provider ids as the API takes them in ?provider= and lists them on GET /.
const (
	ProviderGiphy    = "giphy"
	ProviderHeypster = "heypster"
)

// providerOrder is the order GET / lists configured providers in; giphy is the default.
var providerOrder = []string{ProviderGiphy, ProviderHeypster}

// Heypster speaks the GIPHY API under its own base, so both share one request and parser.
var providerBase = map[string]string{
	ProviderGiphy:    "https://api.giphy.com/v1",
	ProviderHeypster: "https://heypster-gif.com/giphy/v1",
}

func providerKeys(cfg config.GifsConfig) map[string]string {
	return map[string]string{
		ProviderGiphy:    cfg.GiphyAPIKey,
		ProviderHeypster: cfg.HeypsterAPIKey,
	}
}

// ConfiguredProviders lists the providers this instance can search - those it has a key
// for - in a fixed order. It is what GET / advertises; never the keys themselves.
func ConfiguredProviders(cfg config.GifsConfig) []string {
	keys := providerKeys(cfg)
	out := []string{}
	for _, id := range providerOrder {
		if keys[id] != "" {
			out = append(out, id)
		}
	}
	return out
}

// pageCache holds trending pages. An interface so tests can watch what is cached.
type pageCache interface {
	get(ctx context.Context, key string) ([]byte, bool)
	set(ctx context.Context, key string, val []byte, ttl time.Duration)
}

type redisCache struct{ rdb *redis.Client }

func (r redisCache) get(ctx context.Context, key string) ([]byte, bool) {
	b, err := r.rdb.Get(ctx, key).Bytes()
	return b, err == nil
}

// set is best effort: a cache that cannot be written only costs a provider call later.
func (r redisCache) set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	r.rdb.Set(ctx, key, val, ttl)
}

type Service struct {
	keys   map[string]string
	bases  map[string]string
	client *http.Client
	cache  pageCache
	prefix string
}

// NewService builds the provider client. rdb may be nil (trending is then fetched every
// time); cachePrefix is the instance's Redis key prefix.
func NewService(cfg config.GifsConfig, rdb *redis.Client, cachePrefix string) *Service {
	s := &Service{
		keys:  providerKeys(cfg),
		bases: maps.Clone(providerBase),
		client: &http.Client{
			Timeout: upstreamTimeout,
			// The providers' APIs do not redirect. Following one would re-send the request,
			// key included, wherever a Location header pointed; a 3xx is treated as a failure.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		prefix: cachePrefix,
	}
	if rdb != nil {
		s.cache = redisCache{rdb: rdb}
	}
	return s
}

// Search returns one page of results for the user's query. The query goes to the provider
// and nowhere else: it is not cached and not logged.
func (s *Service) Search(ctx context.Context, provider, query string, limit, offset int) (Page, error) {
	p, err := resolveProvider(provider)
	if err != nil {
		return Page{}, err
	}
	q := normalizeQuery(query)
	if q == "" {
		return Page{}, ErrQueryRequired
	}
	if s.keys[p] == "" {
		return Page{}, ErrNotConfigured
	}
	return s.fetch(ctx, p, "search", q, clampLimit(limit), clampOffset(offset))
}

// Trending returns one page of the provider's trending GIFs, from the cache when another
// user asked for the same page in the last two minutes.
func (s *Service) Trending(ctx context.Context, provider string, limit, offset int) (Page, error) {
	p, err := resolveProvider(provider)
	if err != nil {
		return Page{}, err
	}
	if s.keys[p] == "" {
		return Page{}, ErrNotConfigured
	}
	limit, offset = clampLimit(limit), clampOffset(offset)
	key := fmt.Sprintf("%sgifs:trending:%s:%d:%d", s.prefix, p, limit, offset)
	if s.cache != nil {
		if b, ok := s.cache.get(ctx, key); ok {
			var page Page
			if json.Unmarshal(b, &page) == nil && page.Results != nil {
				return page, nil
			}
		}
	}
	page, err := s.fetch(ctx, p, "trending", "", limit, offset)
	if err != nil {
		return Page{}, err
	}
	if s.cache != nil {
		if b, err := json.Marshal(page); err == nil {
			s.cache.set(ctx, key, b, trendingTTL)
		}
	}
	return page, nil
}

// fetch makes the one upstream call. Every failure is logged here, without the key, and
// handed back as a sentinel that carries no upstream text, so nothing the provider or
// net/http said can reach the client.
func (s *Service) fetch(ctx context.Context, provider, kind, q string, limit, offset int) (Page, error) {
	endpoint := s.bases[provider] + "/gifs/" + kind
	params := url.Values{}
	params.Set("api_key", s.keys[provider])
	if q != "" {
		params.Set("q", q)
	}
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", strconv.Itoa(offset))
	params.Set("rating", "pg-13")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	if err != nil {
		s.logFailure(provider, endpoint, 0, s.scrub(err))
		return Page{}, ErrUpstream
	}
	// A request built from nothing but these headers: the user's address, cookies, headers
	// and account never reach the provider.
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	res, err := s.client.Do(req)
	if err != nil {
		s.logFailure(provider, endpoint, 0, s.scrub(err))
		return Page{}, ErrUpstream
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		// Drain a little so the connection can be reused; the body itself is not logged,
		// since a provider's error page may echo the request back.
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		s.logFailure(provider, endpoint, res.StatusCode, "unexpected status")
		if res.StatusCode == http.StatusTooManyRequests {
			return Page{}, ErrUpstreamThrottled
		}
		return Page{}, ErrUpstream
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes+1))
	if err != nil {
		s.logFailure(provider, endpoint, res.StatusCode, "reading the body: "+s.scrub(err))
		return Page{}, ErrUpstream
	}
	if len(body) > maxBodyBytes {
		s.logFailure(provider, endpoint, res.StatusCode, "body larger than the cap")
		return Page{}, ErrUpstream
	}
	page, err := normalize(body, offset)
	if err != nil {
		s.logFailure(provider, endpoint, res.StatusCode, "body is not GIPHY-shaped JSON")
		return Page{}, ErrUpstream
	}
	return page, nil
}

// logFailure tells the operator a provider call failed: which provider, which endpoint
// (without its query string, which holds the key and the user's search) and the status,
// 0 when no response came back at all.
func (s *Service) logFailure(provider, endpoint string, status int, cause string) {
	if status == 0 {
		logger.Warn("gifs", "%s: %s failed: %s", provider, endpoint, cause)
		return
	}
	logger.Warn("gifs", "%s: %s answered %d: %s", provider, endpoint, status, cause)
}

// queryParam matches the parameters a request URL carries that must not be logged.
var queryParam = regexp.MustCompile(`([?&](?:api_key|q)=)[^&\s"']*`)

// scrub reduces an error to text that is safe to log. net/http reports a failed request as
// a *url.Error whose message is the whole URL - API key, search text and all - so the URL is
// dropped and only the underlying cause kept. The key, and any api_key=/q= parameter that
// still turns up, are then masked anyway, so a cause that quotes the URL cannot leak it.
func (s *Service) scrub(err error) string {
	for {
		var ue *url.Error
		if !errors.As(err, &ue) || ue.Err == nil {
			break
		}
		err = ue.Err
	}
	msg := err.Error()
	for _, k := range s.keys {
		if k == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, k, "REDACTED")
		if esc := url.QueryEscape(k); esc != k {
			msg = strings.ReplaceAll(msg, esc, "REDACTED")
		}
	}
	return queryParam.ReplaceAllString(msg, "${1}REDACTED")
}

// resolveProvider maps ?provider= to a provider id; empty means the default, giphy.
func resolveProvider(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	if p == "" {
		return ProviderGiphy, nil
	}
	if _, ok := providerBase[p]; !ok {
		return "", ErrUnknownProvider
	}
	return p, nil
}

// normalizeQuery trims the search text and cuts it to GIPHY's 50-character limit rather
// than refusing it: a long paste should still search for something.
func normalizeQuery(q string) string {
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) > maxQueryRunes {
		q = strings.TrimSpace(string([]rune(q)[:maxQueryRunes]))
	}
	return q
}

func clampLimit(n int) int  { return min(max(n, 1), maxLimit) }
func clampOffset(n int) int { return min(max(n, 0), maxOffset) }
