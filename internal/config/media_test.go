package config

import (
	"strings"
	"testing"
	"time"
)

// Nothing set: the proxy is on with an 8 MB cap and a week's cache of at most 5 GB, and
// validates - an instance gets it without configuring anything.
func TestMediaProxyDefaults(t *testing.T) {
	m := loadMediaProxyConfig()
	if !m.Enabled || m.Key != "" || m.MaxBytes != 8*1024*1024 || m.CacheTTL != 7*24*time.Hour || m.CacheMaxBytes != 5<<30 || m.AllowPrivate {
		t.Fatalf("unexpected defaults: %+v", m)
	}
	if err := validateMediaProxy(m); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestMediaProxyEnv(t *testing.T) {
	t.Setenv("MEDIA_PROXY_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("MEDIA_PROXY_MAX_MB", "2")
	t.Setenv("MEDIA_PROXY_CACHE_TTL", "12h")
	t.Setenv("MEDIA_PROXY_ALLOW_PRIVATE", "true")
	t.Setenv("MEDIA_PROXY_CACHE_MAX_GB", "20")
	m := loadMediaProxyConfig()
	if m.MaxBytes != 2*1024*1024 || m.CacheTTL != 12*time.Hour || m.CacheMaxBytes != 20<<30 || !m.AllowPrivate || m.Key == "" {
		t.Fatalf("env not applied: %+v", m)
	}
	if err := validateMediaProxy(m); err != nil {
		t.Errorf("valid settings refused: %v", err)
	}
}

func TestMediaProxyValidation(t *testing.T) {
	base := loadMediaProxyConfig()
	for name, mutate := range map[string]func(*MediaProxyConfig){
		"short key":     func(m *MediaProxyConfig) { m.Key = "too-short" },
		"no size":       func(m *MediaProxyConfig) { m.MaxBytes = 0 },
		"huge size":     func(m *MediaProxyConfig) { m.MaxBytes = 500 * 1024 * 1024 },
		"bad cache ttl": func(m *MediaProxyConfig) { m.CacheTTL = 0 },
		"no cache size": func(m *MediaProxyConfig) { m.CacheMaxBytes = 0 },
		"tiny cache":    func(m *MediaProxyConfig) { m.CacheMaxBytes = 100 << 20 },
	} {
		m := base
		mutate(&m)
		if validateMediaProxy(m) == nil {
			t.Errorf("%s: accepted %+v", name, m)
		}
	}
	// Switched off, nothing else is checked.
	off := base
	off.Enabled, off.Key = false, "x"
	if err := validateMediaProxy(off); err != nil {
		t.Errorf("disabled proxy refused: %v", err)
	}
}

// A cache size that is not a positive whole number of gigabytes is refused by name, not read
// as something else.
func TestMediaProxyCacheSizeEnv(t *testing.T) {
	for _, raw := range []string{"0", "-3", "1.5", "lots", "99999999999"} {
		t.Setenv("MEDIA_PROXY_CACHE_MAX_GB", raw)
		m := loadMediaProxyConfig()
		if err := validateMediaProxy(m); err == nil || !strings.Contains(err.Error(), "MEDIA_PROXY_CACHE_MAX_GB") {
			t.Errorf("%q: read as %d bytes, %v", raw, m.CacheMaxBytes, err)
		}
	}
}

func TestParseDays(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"7d": 7 * 24 * time.Hour, "1d": 24 * time.Hour, "90m": 90 * time.Minute, "36h": 36 * time.Hour,
		"0d": 0, "-1d": 0, "xd": 0, "soon": 0, "-5m": 0,
	} {
		if got, _ := parseDays(raw); got != want {
			t.Errorf("parseDays(%q) = %v, want %v", raw, got, want)
		}
	}
}
