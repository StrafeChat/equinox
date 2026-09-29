// Package unfurl fetches link-preview metadata (Open Graph / Twitter card / <meta>) for a URL
// so the client can render a preview card. It is deliberately SSRF-hardened: the app makes an
// outbound request to a URL a user chose, so it must never be tricked into reaching this host's
// own network. Results are cached in Redis (failures too), and only the page <head> is read.
package unfurl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"golang.org/x/net/html"
)

const (
	maxRedirects     = 5
	fetchTimeout     = 6 * time.Second
	maxHeadBytes     = 300 * 1024
	cacheTTL         = 6 * time.Hour
	negativeCacheTTL = 30 * time.Minute
	maxFieldLen      = 500
	userAgent        = "Mozilla/5.0 (compatible; StrafeBot/1.0; +https://strafe.chat)"
)

// Metadata is the link-preview card the client renders. Every field is best-effort and may be
// empty; an all-empty Metadata means the link had nothing worth previewing.
type Metadata struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	Icon        string `json:"icon,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	ThemeColor  string `json:"theme_color,omitempty"`
	// URL is the final URL after redirects, so the client links to where it actually landed.
	URL string `json:"url,omitempty"`
}

var errBlockedAddress = errors.New("unfurl: address is not public")

type Service struct {
	redis  *redis.Client
	client *http.Client
}

func NewService(rdb *redis.Client) *Service {
	dialer := &net.Dialer{Timeout: fetchTimeout, Control: safeDialControl}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   fetchTimeout,
		ExpectContinueTimeout: time.Second,
		DisableKeepAlives:     true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("unfurl: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errBlockedAddress
			}
			return nil
		},
	}
	return &Service{redis: rdb, client: client}
}

// safeDialControl runs after DNS resolution, immediately before the socket connects - so it
// also covers redirect targets and DNS-rebinding, which a one-shot pre-flight lookup would
// miss. It refuses any non-public address.
func safeDialControl(network, address string, _ syscall.RawConn) error {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return errBlockedAddress
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !isPublicIP(ip) {
		return errBlockedAddress
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// To4() unwraps IPv4-mapped IPv6 (::ffff:a.b.c.d) too, so these checks cover both.
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 { // 100.64.0.0/10 (CGNAT)
			return false
		}
		if ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0 { // 192.0.0.0/24 (IETF)
			return false
		}
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) { // 198.18.0.0/15 (benchmark)
			return false
		}
	}
	return true
}

// ParseURL validates that a string is an http(s) URL with a host. Exported for the handler.
func ParseURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, false
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, false
	}
	return u, true
}

// Unfurl returns metadata for a URL, from cache or by fetching. Failures are negative-cached so
// a broken or hostile link isn't refetched on every message render.
func (s *Service) Unfurl(ctx context.Context, rawURL string) (Metadata, error) {
	key := cacheKey(rawURL)
	if cached, err := s.redis.Get(ctx, key).Result(); err == nil {
		var m Metadata
		if json.Unmarshal([]byte(cached), &m) == nil {
			return m, nil
		}
	}
	meta, err := s.fetch(ctx, rawURL)
	if err != nil {
		s.redis.Set(ctx, key, "{}", negativeCacheTTL)
		return Metadata{}, err
	}
	if b, e := json.Marshal(meta); e == nil {
		s.redis.Set(ctx, key, string(b), cacheTTL)
	}
	return meta, nil
}

func cacheKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return "unfurl:" + hex.EncodeToString(sum[:16])
}

func (s *Service) fetch(ctx context.Context, rawURL string) (Metadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Metadata{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := s.client.Do(req)
	if err != nil {
		return Metadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Metadata{}, errors.New("unfurl: bad status")
	}
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); ct != "" && !strings.Contains(ct, "html") {
		return Metadata{}, errors.New("unfurl: not html")
	}
	final := resp.Request.URL // the URL after any redirects
	meta := extractMetadata(io.LimitReader(resp.Body, maxHeadBytes), final)
	meta.URL = final.String()
	return meta, nil
}

func extractMetadata(r io.Reader, base *url.URL) Metadata {
	var m Metadata
	var titleTag, ogTitle, twTitle, desc, ogDesc, twDesc string

	z := html.NewTokenizer(r)
	for {
		switch z.Next() {
		case html.ErrorToken:
			goto done
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "title":
				if titleTag == "" && z.Next() == html.TextToken {
					titleTag = clean(string(z.Text()))
				}
			case "meta":
				if !hasAttr {
					continue
				}
				attrs := readAttrs(z)
				content := clean(attrs["content"])
				if content == "" {
					continue
				}
				switch key := lower(firstNonEmpty(attrs["property"], attrs["name"], attrs["http-equiv"])); {
				case key == "og:title":
					setIfEmpty(&ogTitle, content)
				case key == "twitter:title":
					setIfEmpty(&twTitle, content)
				case key == "og:description":
					setIfEmpty(&ogDesc, content)
				case key == "twitter:description":
					setIfEmpty(&twDesc, content)
				case key == "description":
					setIfEmpty(&desc, content)
				case key == "og:site_name":
					setIfEmpty(&m.SiteName, content)
				case key == "theme-color":
					setIfEmpty(&m.ThemeColor, content)
				case isImageKey(key):
					if m.Image == "" {
						if ref := resolveRef(content, base); ref != "" {
							m.Image = ref
						}
					}
				}
			case "link":
				if !hasAttr {
					continue
				}
				attrs := readAttrs(z)
				if m.Icon == "" && attrs["href"] != "" && isIconRel(attrs["rel"]) {
					if ref := resolveRef(attrs["href"], base); ref != "" {
						m.Icon = ref
					}
				}
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "head" {
				goto done // everything we want lives in <head>; stop before the body
			}
		}
	}
done:
	m.Title = firstNonEmpty(ogTitle, twTitle, titleTag)
	m.Description = firstNonEmpty(ogDesc, twDesc, desc)
	return m
}

func readAttrs(z *html.Tokenizer) map[string]string {
	attrs := make(map[string]string, 4)
	for {
		k, v, more := z.TagAttr()
		key := lower(string(k))
		if _, ok := attrs[key]; !ok {
			attrs[key] = string(v)
		}
		if !more {
			break
		}
	}
	return attrs
}

func clean(s string) string {
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ") // collapse all runs of whitespace to one space
	if utf8.RuneCountInString(s) > maxFieldLen {
		s = string([]rune(s)[:maxFieldLen])
	}
	return strings.TrimSpace(s)
}

func resolveRef(ref string, base *url.URL) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(u)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return ""
	}
	return resolved.String()
}

func isImageKey(key string) bool {
	switch key {
	case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src":
		return true
	}
	return false
}

func isIconRel(rel string) bool {
	rel = lower(rel)
	for _, tok := range strings.Fields(rel) {
		if tok == "icon" {
			return true
		}
	}
	return strings.Contains(rel, "apple-touch")
}

func lower(s string) string { return strings.ToLower(s) }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func setIfEmpty(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}
