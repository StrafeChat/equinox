// Package media is the instance's image proxy. A bot's message embed may show pictures from
// anywhere on the web; if every reader's browser loaded them from there, the site a bot
// pointed at would learn the address of everyone reading the message, and when they read
// it. Instead the serializers hand clients a signed address on this instance
// (GET /media/v1/external/{sig}/{src}), and this instance fetches the picture itself - once,
// at post time, when it warms its cache - so the site sees the instance and nobody else.
//
// The signature is the permission: an <img> cannot send an Authorization header, so the
// route is unauthenticated, and without the key nobody can make the instance fetch an
// address of their choosing. Fetches go through safehttp's guard (never this host's own
// network), only PNG, JPEG, GIF and WebP are served (never SVG or HTML: the bytes are
// same-origin with the web client), and fetched images are kept in Nebula's media-cache/
// for MEDIA_PROXY_CACHE_TTL when Nebula is configured, else streamed through.
package media

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/safehttp"
)

const (
	// signContext and cacheContext keep the two uses of the key apart: a proxy signature is
	// never also a cache object's name.
	signContext  = "strafe-media-proxy/v1\x00"
	cacheContext = "strafe-media-cache/v1\x00"
	// RoutePrefix is where the proxy is mounted, relative to the API's base URL.
	RoutePrefix = "/media/v1/external/"
	// maxSourceLen is the longest source URL the proxy signs or serves (embeds cap theirs at
	// the same length).
	maxSourceLen = 2048
	// sigLen is the length of a signature in its base64url form (16 bytes).
	sigLen = 22
)

// Service signs proxy URLs and serves them. A nil *Service is a proxy that is switched off:
// it signs nothing and owns nothing.
type Service struct {
	cfg    config.MediaProxyConfig
	nebula config.NebulaConfig
	// publicBase is the API's browser-facing base URL ("" when unknown: proxy URLs are then
	// paths a client resolves against the API base it already uses).
	publicBase string
	redis      *redis.Client
	prefix     string
	key        []byte
	// client fetches sources (guarded); cdn reads cached objects back from Nebula's internal
	// address, which is on this deployment's own network by design.
	client *http.Client
	cdn    *http.Client
	// store: fetched images are kept in Nebula (it is configured), not only streamed.
	store  bool
	flight singleflight.Group
	misses *windowLimiter
	// fetching is the process's missFetches slots for fetches readers' views cause.
	fetching chan struct{}
	now      func() time.Time
}

// New builds the proxy, or returns nil when MEDIA_PROXY is off. Without MEDIA_PROXY_KEY the
// signing key is a random one kept in Redis, created by whichever API replica starts first
// (SETNX), so every replica signs and checks alike; an error here (Redis unreachable) leaves
// the caller to run without the proxy.
func New(ctx context.Context, cfg *config.Config, rdb *redis.Client) (*Service, error) {
	if cfg == nil || !cfg.Media.Enabled {
		return nil, nil
	}
	prefix := cfg.Database.Redis.CachePrefix
	key, err := resolveKey(ctx, rdb, prefix, cfg.Media.Key)
	if err != nil {
		return nil, err
	}
	s := &Service{
		cfg:        cfg.Media,
		nebula:     cfg.Nebula,
		publicBase: strings.TrimRight(strings.TrimSpace(cfg.Federation.PublicURL), "/"),
		redis:      rdb,
		prefix:     prefix,
		key:        key,
		client: safehttp.NewClient(safehttp.ClientOptions{
			Timeout: fetchTimeout, MaxRedirects: maxRedirects, AllowPrivate: cfg.Media.AllowPrivate,
		}),
		cdn:      &http.Client{Timeout: time.Minute},
		store:    strings.TrimSpace(cfg.Nebula.BaseURL) != "" && strings.TrimSpace(cfg.Nebula.UploadSecret) != "",
		misses:   newWindowLimiter(missesPerMinute, time.Minute),
		fetching: make(chan struct{}, missFetches),
		now:      time.Now,
	}
	return s, nil
}

// resolveKey is the configured key (64 hex characters are the 32 bytes they spell, anything
// else is used as it is), else the shared generated one.
func resolveKey(ctx context.Context, rdb *redis.Client, prefix, configured string) ([]byte, error) {
	if configured != "" {
		if len(configured) == 64 {
			if b, err := hex.DecodeString(configured); err == nil {
				return b, nil
			}
		}
		return []byte(configured), nil
	}
	if rdb == nil {
		return nil, errors.New("MEDIA_PROXY_KEY is unset and there is no Redis to keep a generated key in")
	}
	k := prefix + "media:proxykey"
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	if err := rdb.SetNX(ctx, k, hex.EncodeToString(fresh), 0).Err(); err != nil {
		return nil, err
	}
	stored, err := rdb.Get(ctx, k).Result()
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(stored)
	if err != nil || len(key) < 32 {
		return nil, errors.New("the generated media proxy key in Redis is malformed")
	}
	return key, nil
}

func (s *Service) mac(context, src string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(context))
	m.Write([]byte(src))
	return m.Sum(nil)
}

// sign is a source URL's signature: the first 16 bytes of its HMAC, base64url.
func (s *Service) sign(src string) string {
	return base64.RawURLEncoding.EncodeToString(s.mac(signContext, src)[:16])
}

// hash names a source in the cache: keyed, so nobody can work out from a Redis key or a
// Nebula object which URL this instance has fetched.
func (s *Service) hash(src string) string {
	return hex.EncodeToString(s.mac(cacheContext, src)[:16])
}

// proxiable is a URL the proxy will fetch: http(s) with a host, no credentials, not too long.
func proxiable(src string) bool {
	if src == "" || len(src) > maxSourceLen {
		return false
	}
	u, err := url.Parse(src)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// ProxyURL is src's address on this proxy: absolute when the instance knows its public API
// URL (FEDERATION_PUBLIC_URL), else a path the client resolves against its API base. ""
// for anything the proxy would not fetch, or with the proxy off.
func (s *Service) ProxyURL(src string) string {
	if s == nil || !proxiable(src) {
		return ""
	}
	return s.publicBase + RoutePrefix + s.sign(src) + "/" + base64.RawURLEncoding.EncodeToString([]byte(src))
}

// IsLocal reports whether a URL is already this instance's own - on its CDN, or already an
// address on this proxy - and so needs no proxying.
func (s *Service) IsLocal(u string) bool {
	if s == nil {
		return false
	}
	for _, base := range []string{s.nebula.PublicURL, s.nebula.BaseURL} {
		base = strings.TrimRight(strings.TrimSpace(base), "/")
		if base != "" && strings.HasPrefix(u, base+"/") {
			return true
		}
	}
	return s.publicBase != "" && strings.HasPrefix(u, s.publicBase+RoutePrefix)
}

// Source checks a proxy address's signature (in constant time) and returns the source URL
// it names; ok is false for anything this instance did not sign.
func (s *Service) Source(sig, encoded string) (string, bool) {
	if s == nil || len(sig) != sigLen || encoded == "" || len(encoded) > base64.RawURLEncoding.EncodedLen(maxSourceLen) {
		return "", false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(got) != 16 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	src := string(raw)
	if !hmac.Equal(got, s.mac(signContext, src)[:16]) || !proxiable(src) {
		return "", false
	}
	return src, true
}
