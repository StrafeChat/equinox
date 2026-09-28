package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
)

// InstanceInfo is what an instance publishes at /.well-known/strafe (and mirrors at
// /federation/v1/instance): enough for a peer to find its federation endpoint and verify
// its signatures, and for a client to discover where to connect.
type InstanceInfo struct {
	Domain        string       `json:"domain"`
	Version       int          `json:"version"`
	APIURL        string       `json:"api_url"`
	GatewayURL    string       `json:"gateway_url,omitempty"`
	FederationURL string       `json:"federation_url"`
	KeyID         string       `json:"key_id"`
	PublicKey     string       `json:"public_key"`
	Software      SoftwareInfo `json:"software"`
}

type SoftwareInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// WellKnownPath is where InstanceInfo is served, relative to https://<domain>.
const WellKnownPath = "/.well-known/strafe"

const (
	discoveryTTL = time.Hour
	// discoveryNegativeTTL is how long a failed lookup is remembered. The peer domain on
	// an inbound request is attacker-controlled until its signature verifies, so without
	// this every unauthenticated request could make us open a fresh outbound connection.
	discoveryNegativeTTL = time.Minute
)

// domainRe is a hostname (RFC 1123 labels) with an optional port - the only shape a
// peer domain may take before it is used to build a URL.
var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*(:[0-9]{1,5})?$`)

// ValidPeerDomain reports whether a domain may be looked up at all. With AllowInsecure
// off (production) it also refuses IP literals and loopback/link-local names, so a
// forged X-Strafe-Instance header cannot point discovery at this host's own network
// (cloud metadata endpoints, internal services).
func ValidPeerDomain(cfg config.FederationConfig, domain string) bool {
	if len(domain) > 253 || !domainRe.MatchString(domain) {
		return false
	}
	if cfg.AllowInsecure {
		return true
	}
	if _, ok := cfg.StaticPeers[domain]; ok {
		return true
	}
	host := domain
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if net.ParseIP(host) != nil {
		return false
	}
	switch {
	case host == "localhost", strings.HasSuffix(host, ".localhost"),
		strings.HasSuffix(host, ".local"), strings.HasSuffix(host, ".internal"),
		strings.HasSuffix(host, ".home.arpa"):
		return false
	}
	return true
}

type cachedInfo struct {
	info      *InstanceInfo
	fetchedAt time.Time
}

// Discovery resolves a peer domain to its InstanceInfo, with an in-memory cache.
type Discovery struct {
	cfg    config.FederationConfig
	http   *http.Client
	mu     sync.Mutex
	c      map[string]cachedInfo
	failed map[string]time.Time
}

func NewDiscovery(cfg config.FederationConfig) *Discovery {
	return &Discovery{
		cfg: cfg,
		http: &http.Client{
			Timeout: 10 * time.Second,
			// The document must live at the advertised location; following a redirect
			// would let a peer bounce us to any URL, including internal ones.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		c:      map[string]cachedInfo{},
		failed: map[string]time.Time{},
	}
}

// wellKnownURL is where a peer's /.well-known/strafe lives: normally https://<domain>,
// overridable per domain through FEDERATION_STATIC_PEERS for local development.
func (d *Discovery) wellKnownURL(domain string) string {
	if base, ok := d.cfg.StaticPeers[domain]; ok {
		return base + WellKnownPath
	}
	return "https://" + domain + WellKnownPath
}

// Lookup returns the peer's instance info, from cache unless `force` (used after a
// signature failure, in case the peer rotated its key).
func (d *Discovery) Lookup(ctx context.Context, domain string, force bool) (*InstanceInfo, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" || !ValidPeerDomain(d.cfg, domain) {
		return nil, ErrInvalidFID
	}
	d.mu.Lock()
	if !force {
		if hit, ok := d.c[domain]; ok && time.Since(hit.fetchedAt) < discoveryTTL {
			d.mu.Unlock()
			return hit.info, nil
		}
		if at, ok := d.failed[domain]; ok && time.Since(at) < discoveryNegativeTTL {
			d.mu.Unlock()
			return nil, fmt.Errorf("%w: %s (recent lookup failed)", ErrRemoteUnavailable, domain)
		}
	}
	d.mu.Unlock()

	info, err := d.fetch(ctx, domain)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.failed[domain] = time.Now()
		return nil, err
	}
	delete(d.failed, domain)
	d.c[domain] = cachedInfo{info: info, fetchedAt: time.Now()}
	return info, nil
}

func (d *Discovery) fetch(ctx context.Context, domain string) (*InstanceInfo, error) {
	url := d.wellKnownURL(domain)
	if !d.cfg.AllowInsecure && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("federation: refusing insecure discovery URL %s", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s returned %s", ErrRemoteUnavailable, url, res.Status)
	}
	var info InstanceInfo
	if err := json.NewDecoder(io.LimitReader(res.Body, 64*1024)).Decode(&info); err != nil {
		return nil, fmt.Errorf("federation: bad instance document from %s: %w", domain, err)
	}
	info.Domain = strings.ToLower(strings.TrimSpace(info.Domain))
	if info.Domain != domain {
		return nil, fmt.Errorf("federation: %s claims to be %q", domain, info.Domain)
	}
	info.FederationURL = strings.TrimRight(strings.TrimSpace(info.FederationURL), "/")
	if info.FederationURL == "" || info.PublicKey == "" {
		return nil, fmt.Errorf("federation: %s publishes an incomplete instance document", domain)
	}
	if !d.cfg.AllowInsecure && !strings.HasPrefix(info.FederationURL, "https://") {
		return nil, fmt.Errorf("federation: %s advertises an insecure federation URL", domain)
	}
	return &info, nil
}
