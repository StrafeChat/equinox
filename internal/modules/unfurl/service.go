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
	"regexp"
	"strconv"
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
	// Present as Discordbot: image/video-sharing sites (and many others) serve their richest
	// Open Graph / oEmbed specifically to it, and gate or strip it for unknown crawlers.
	userAgent = "Mozilla/5.0 (compatible; Discordbot/2.0; +https://strafe.chat)"
)

// Media is an image or video rendition with its intrinsic size when the page declared one, so
// the client can reserve the right box and not jump when it loads.
type Media struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// Metadata is the link-preview card the client renders, modelled on Discord's rich embed so a
// card can carry an author, title, description, a provider/footer, an image (large or a small
// thumbnail) and a video. Every field is best-effort; an all-empty Metadata means the link had
// nothing worth previewing.
type Metadata struct {
	// Type is og:type (article, website, video.*, image.*), a hint for how to lay the card out.
	Type        string `json:"type,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// URL is the final URL after redirects (or the canonical one), so the card links to where
	// it actually landed.
	URL string `json:"url,omitempty"`
	// SiteName is the provider (Discord shows it as the footer / above-title provider line).
	SiteName string `json:"site_name,omitempty"`
	// Author is a byline when the page declares one (article:author / author / twitter:creator).
	Author    string `json:"author,omitempty"`
	AuthorURL string `json:"author_url,omitempty"`
	// Color is theme-color, used for the card's accent edge.
	Color string `json:"color,omitempty"`
	// Icon is the favicon, shown next to the site name.
	Icon string `json:"icon,omitempty"`
	// Image is og:image / twitter:image. ImageLarge is true for a full-width image
	// (twitter:card=summary_large_image, or the default), false for a small right-hand thumbnail.
	Image      *Media `json:"image,omitempty"`
	ImageLarge bool   `json:"image_large,omitempty"`
	// Video is og:video. VideoType distinguishes a directly-playable file ("video/mp4",
	// "video/webm") from an embed page ("text/html", e.g. a YouTube player) the client can't
	// inline - it falls back to the image with a play affordance for those.
	Video     *Media `json:"video,omitempty"`
	VideoType string `json:"video_type,omitempty"`
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
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/*;q=0.8,*/*;q=0.7")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := s.client.Do(req)
	if err != nil {
		return Metadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Metadata{}, errors.New("unfurl: bad status")
	}
	final := resp.Request.URL // the URL after any redirects
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	// A URL that is itself an image/video - some hosts serve the file at a media-looking URL -
	// is described directly rather than parsed as HTML.
	switch {
	case strings.HasPrefix(ct, "image/"):
		return Metadata{Type: "image", URL: final.String(), SiteName: final.Hostname(), Image: &Media{URL: final.String()}, ImageLarge: true}, nil
	case strings.HasPrefix(ct, "video/"):
		return Metadata{Type: "video", URL: final.String(), SiteName: final.Hostname(), Video: &Media{URL: final.String()}, VideoType: ct}, nil
	case ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "xml"):
		return Metadata{}, errors.New("unfurl: unsupported content type")
	}
	meta, oEmbedURL := extractMetadata(io.LimitReader(resp.Body, maxHeadBytes), final)
	// Many image/video-sharing hosts (and YouTube, Twitter, …) expose the image/thumbnail only
	// through oEmbed, not Open Graph - follow it and fill in what the page left out.
	if oEmbedURL != "" {
		s.mergeOEmbed(ctx, &meta, oEmbedURL)
	}
	if meta.URL == "" { // prefer the page's own og:url/canonical; fall back to where we landed
		meta.URL = final.String()
	}
	return meta, nil
}

// oEmbed is the subset of an oEmbed response (https://oembed.com) we use.
type oEmbed struct {
	Type         string `json:"type"`
	Title        string `json:"title"`
	AuthorName   string `json:"author_name"`
	AuthorURL    string `json:"author_url"`
	ProviderName string `json:"provider_name"`
	ProviderURL  string `json:"provider_url"`
	URL          string `json:"url"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	ThumbnailURL string `json:"thumbnail_url"`
	ThumbnailW   int    `json:"thumbnail_width"`
	ThumbnailH   int    `json:"thumbnail_height"`
}

// mergeOEmbed fetches the oEmbed document (through the same SSRF-safe client) and fills in fields
// the page's own tags left blank - above all the image, which image hosts often expose only here.
func (s *Service) mergeOEmbed(ctx context.Context, m *Metadata, oembedURL string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oembedURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return
	}
	var o oEmbed
	if json.Unmarshal(body, &o) != nil {
		return
	}
	setIfEmpty(&m.Title, clean(o.Title))
	setIfEmpty(&m.Author, clean(o.AuthorName))
	setIfEmpty(&m.AuthorURL, o.AuthorURL)
	setIfEmpty(&m.SiteName, clean(o.ProviderName))
	if m.Type == "" {
		m.Type = lower(o.Type)
	}
	if m.Image == nil {
		// A "photo" oEmbed's url is the image; else the thumbnail; else a url/provider_url that
		// is itself a media file (image hosts point provider_url at the raw file).
		switch {
		case o.Type == "photo" && o.URL != "":
			m.Image = &Media{URL: o.URL, Width: o.Width, Height: o.Height}
		case o.ThumbnailURL != "":
			m.Image = &Media{URL: o.ThumbnailURL, Width: o.ThumbnailW, Height: o.ThumbnailH}
		case isMediaURL(o.URL):
			m.Image = &Media{URL: o.URL}
		case isMediaURL(o.ProviderURL):
			m.Image = &Media{URL: o.ProviderURL}
		}
		if m.Image != nil {
			m.ImageLarge = true
		}
	}
}

var mediaExtRe = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp|avif|mp4|webm|mov)$`)

// isMediaURL reports whether a URL's path ends in an image/video extension.
func isMediaURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return mediaExtRe.MatchString(u.Path)
}

// extractMetadata parses the page <head> and returns the metadata plus the oEmbed discovery URL
// (rel=alternate application/json+oembed) if the page declared one, for the caller to fetch.
func extractMetadata(r io.Reader, base *url.URL) (Metadata, string) {
	var m Metadata
	var titleTag, ogTitle, twTitle, desc, ogDesc, twDesc, ogURL, canonical, twCard, oEmbedURL string
	var imageURL, imageW, imageH string
	var videoURL, videoW, videoH string

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
				// og:image:width etc. arrive as their own tags after og:image; captured into
				// the pending image/video and stitched together at `done`.
				switch key := lower(firstNonEmpty(attrs["property"], attrs["name"], attrs["http-equiv"])); key {
				case "og:title":
					setIfEmpty(&ogTitle, content)
				case "twitter:title":
					setIfEmpty(&twTitle, content)
				case "og:description":
					setIfEmpty(&ogDesc, content)
				case "twitter:description":
					setIfEmpty(&twDesc, content)
				case "description":
					setIfEmpty(&desc, content)
				case "og:site_name", "application-name":
					setIfEmpty(&m.SiteName, content)
				case "og:url":
					if ref := resolveRef(content, base); ref != "" {
						setIfEmpty(&ogURL, ref)
					}
				case "og:type":
					setIfEmpty(&m.Type, lower(content))
				case "theme-color", "msapplication-tilecolor":
					setIfEmpty(&m.Color, content)
				case "twitter:card":
					setIfEmpty(&twCard, lower(content))
				case "author", "twitter:creator":
					setIfEmpty(&m.Author, content)
				case "article:author", "og:article:author":
					if isURLish(content) {
						setIfEmpty(&m.AuthorURL, content)
					} else {
						setIfEmpty(&m.Author, content)
					}
				case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src":
					setIfEmpty(&imageURL, content)
				case "og:image:width", "twitter:image:width":
					setIfEmpty(&imageW, content)
				case "og:image:height", "twitter:image:height":
					setIfEmpty(&imageH, content)
				case "og:video", "og:video:url", "og:video:secure_url", "twitter:player:stream":
					setIfEmpty(&videoURL, content)
				case "og:video:width", "twitter:player:width":
					setIfEmpty(&videoW, content)
				case "og:video:height", "twitter:player:height":
					setIfEmpty(&videoH, content)
				case "og:video:type", "twitter:player:stream:content_type":
					setIfEmpty(&m.VideoType, lower(content))
				}
			case "link":
				if !hasAttr {
					continue
				}
				attrs := readAttrs(z)
				rel := lower(attrs["rel"])
				href := attrs["href"]
				if href == "" {
					continue
				}
				if m.Icon == "" && isIconRel(rel) {
					if ref := resolveRef(href, base); ref != "" {
						m.Icon = ref
					}
				}
				if canonical == "" && rel == "canonical" {
					if ref := resolveRef(href, base); ref != "" {
						canonical = ref
					}
				}
				if oEmbedURL == "" && rel == "alternate" && strings.Contains(lower(attrs["type"]), "json+oembed") {
					if ref := resolveRef(href, base); ref != "" {
						oEmbedURL = ref
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
	m.URL = firstNonEmpty(ogURL, canonical)
	if img := resolveRef(imageURL, base); img != "" {
		m.Image = &Media{URL: img, Width: atoi(imageW), Height: atoi(imageH)}
		m.ImageLarge = twCard != "summary" // "summary" is the small right-hand thumbnail style
	}
	if vid := resolveRef(videoURL, base); vid != "" {
		m.Video = &Media{URL: vid, Width: atoi(videoW), Height: atoi(videoH)}
	}
	return m, oEmbedURL
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

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func isURLish(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
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
