package pushgateway

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/StrafeChat/equinox/internal/logger"
)

const (
	logTag = "pushgateway"

	// maxPushBody is the Web Push body limit (RFC 8030 push services must accept 4096).
	maxPushBody = 4096
	// minPushBody is the smallest aes128gcm body worth forwarding: the 21-byte header
	// (salt, record size, key id length) plus a 65-byte uncompressed P-256 key id.
	minPushBody = 86
	// maxTTL is the longest either platform will hold a message (FCM: 28 days).
	maxTTL = 28 * 24 * 60 * 60
	// deliverTimeout bounds one forward to Google/Apple, OAuth exchange included.
	deliverTimeout = 10 * time.Second
	// maxRegisterBody is generous for the largest valid registration (a 4096-byte token).
	maxRegisterBody = 16 << 10

	defaultFallback = "New notification"
	maxFallbackRune = 120

	// Rate limits, per minute. Registration is per client IP; pushes are per endpoint (one
	// device) and per VAPID key (one sending instance).
	registerPerMinute = 30
	endpointPerMinute = 120
	senderPerMinute   = 20000
)

var (
	slotRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	debugTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	apnsTokenRe  = regexp.MustCompile(`^[0-9a-fA-F]{64,200}$`)
	topicRe      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

// Options are the injection points tests use; production leaves them zero.
type Options struct {
	// HTTPClient talks to FCM and Google's OAuth2 token endpoint.
	HTTPClient *http.Client
	// APNsHTTPClient talks to APNs, which only speaks HTTP/2 (negotiated over TLS ALPN).
	APNsHTTPClient *http.Client
	FCMBaseURL     string
	APNsURL        string
	APNsSandboxURL string
	Now            func() time.Time
}

// Server is the gateway's HTTP surface.
type Server struct {
	cfg      *Config
	sealer   *Sealer
	audience string
	prefix   string
	endpoint string // PUBLIC_URL + "/v1/push/"

	transports map[string]transport
	debug      *debugStore
	proxies    *proxyList

	registerLimit *limiter
	endpointLimit *limiter
	senderLimit   *limiter

	now     func() time.Time
	handler http.Handler
	stats   stats
}

// NewServer wires the configured platforms behind the HTTP handlers.
func NewServer(cfg *Config, opts Options) (*Server, error) {
	sealer, err := NewSealer(cfg.SealKey, cfg.OldSealKeys)
	if err != nil {
		return nil, err
	}
	proxies, err := newProxyList(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Server{
		cfg:           cfg,
		sealer:        sealer,
		audience:      normalizeOrigin(cfg.PublicURL.String()),
		prefix:        cfg.PublicURL.Path,
		endpoint:      cfg.PublicURL.String() + "/v1/push/",
		transports:    map[string]transport{},
		proxies:       proxies,
		registerLimit: newLimiter(registerPerMinute, now),
		endpointLimit: newLimiter(endpointPerMinute, now),
		senderLimit:   newLimiter(senderPerMinute, now),
		now:           now,
	}

	if cfg.FCM != nil {
		client := opts.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: deliverTimeout}
		}
		s.transports[PlatformFCM] = newFCMClient(cfg.FCM, cmp.Or(opts.FCMBaseURL, DefaultFCMBaseURL), client, now)
	}
	if cfg.APNs != nil {
		client := opts.APNsHTTPClient
		if client == nil {
			// Setting TLSClientConfig switches off the transport's automatic HTTP/2;
			// ForceAttemptHTTP2 keeps it, and APNs accepts nothing else.
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.ForceAttemptHTTP2 = true
			tr.MaxIdleConnsPerHost = 16
			client = &http.Client{Transport: tr, Timeout: deliverTimeout}
		}
		s.transports[PlatformAPNs] = newAPNsClient(cfg.APNs,
			cmp.Or(opts.APNsURL, DefaultAPNsURL), cmp.Or(opts.APNsSandboxURL, DefaultAPNsSandboxURL), client, now)
	}
	if cfg.Debug {
		s.debug = &debugStore{}
		s.transports[PlatformDebug] = s.debug
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/register", s.handleRegister)
	mux.HandleFunc("POST /v1/push/{sealed}", s.handlePush)
	if cfg.Debug {
		mux.HandleFunc("GET /v1/debug/pushes", s.handleDebugPushes)
	}
	s.handler = mux
	if s.prefix != "" {
		// PUBLIC_URL may live under a path (https://example.com/push). Serve both the
		// prefixed paths and the bare ones, so it works whether or not the reverse proxy
		// strips the prefix.
		stripped := http.StripPrefix(s.prefix, mux)
		s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, s.prefix+"/") {
				stripped.ServeHTTP(w, r)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Platforms lists the platforms this gateway can deliver to, in a stable order.
func (s *Server) Platforms() []string {
	var out []string
	for _, p := range []string{PlatformFCM, PlatformAPNs, PlatformDebug} {
		if s.transports[p] != nil {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	platforms := strings.Join(s.Platforms(), ",")
	if platforms == "" {
		platforms = "none"
	}
	_, _ = io.WriteString(w, "ok\nplatforms: "+platforms+"\n")
}

type registerRequest struct {
	Platform string `json:"platform"`
	Token    string `json:"token"`
	Slot     string `json:"slot"`
	Sandbox  bool   `json:"sandbox"`
	Fallback string `json:"fallback"`
}

// handleRegister turns a device's platform token into a Web Push endpoint. Nothing is
// stored: the endpoint is the registration, sealed.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.registerLimit.allow(s.proxies.clientIP(r)); !ok {
		s.stats.throttled.Add(1)
		writeRetryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many registrations, slow down")
		return
	}
	var req registerRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegisterBody))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	reg, msg := s.validateRegistration(&req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	sealed, err := s.sealer.Seal(*reg)
	if err != nil {
		logger.Error(logTag, "seal failed: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.stats.registered.Add(1)
	logger.Debug(logTag, "register platform=%s", reg.Platform)
	writeJSON(w, http.StatusOK, map[string]string{"endpoint": s.endpoint + sealed})
}

// validateRegistration checks a request against the platform's token format and returns
// the registration to seal, or a message for a 400.
func (s *Server) validateRegistration(req *registerRequest) (*Registration, string) {
	switch req.Platform {
	case PlatformFCM, PlatformAPNs:
	case PlatformDebug:
		if !s.cfg.Debug {
			return nil, "invalid platform"
		}
	default:
		return nil, "invalid platform"
	}
	if s.transports[req.Platform] == nil {
		return nil, "platform not configured"
	}
	reg := &Registration{Platform: req.Platform, Token: req.Token, Slot: req.Slot}
	switch req.Platform {
	case PlatformAPNs:
		if !apnsTokenRe.MatchString(req.Token) {
			return nil, "invalid token: APNs tokens are 64-200 hex characters"
		}
		reg.Token = strings.ToLower(req.Token)
		// Only APNs has separate development and production gateways.
		reg.Sandbox = req.Sandbox
	case PlatformFCM:
		if !validFCMToken(req.Token) {
			return nil, "invalid token: FCM tokens are 1-4096 printable ASCII characters"
		}
	case PlatformDebug:
		if !debugTokenRe.MatchString(req.Token) {
			return nil, "invalid token: debug tokens are 1-64 of [A-Za-z0-9_-]"
		}
	}
	if !slotRe.MatchString(req.Slot) {
		return nil, "invalid slot: 1-32 of [A-Za-z0-9_-]"
	}
	fallback := strings.TrimSpace(req.Fallback)
	if fallback == "" {
		fallback = defaultFallback
	}
	if !utf8.ValidString(fallback) || utf8.RuneCountInString(fallback) > maxFallbackRune ||
		strings.IndexFunc(fallback, unicode.IsControl) >= 0 {
		return nil, "invalid fallback: at most 120 characters of plain text"
	}
	reg.Fallback = fallback
	return reg, ""
}

func validFCMToken(t string) bool {
	if len(t) == 0 || len(t) > 4096 {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] < 0x21 || t[i] > 0x7e {
			return false
		}
	}
	return true
}

// handlePush is the Web Push receiver (RFC 8030 §5). The body is ciphertext the gateway
// cannot read; it checks the envelope, the sender's VAPID signature and the endpoint, and
// hands the bytes on unchanged.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	s.stats.received.Add(1)
	status, msg, retry, ttl := s.receivePush(r)
	if retry > 0 {
		writeRetryAfter(w, retry)
	}
	switch {
	case status == http.StatusCreated:
		s.stats.delivered.Add(1)
		// RFC 8030 §5.2: tell the sender the TTL actually applied (it may have been capped).
		w.Header().Set("TTL", strconv.Itoa(ttl))
		w.WriteHeader(status)
		return
	case status == http.StatusGone:
		s.stats.gone.Add(1)
	case status == http.StatusTooManyRequests:
		s.stats.throttled.Add(1)
	case status >= 500:
		s.stats.failed.Add(1)
	default:
		s.stats.rejected.Add(1)
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "vapid")
	}
	writeError(w, status, msg)
}

// receivePush runs every check and the forward, returning the status to answer, the
// error message for a failure, a Retry-After when throttled and the TTL applied. The
// order matters: cheap envelope checks first, then the signature, and only then anything
// keyed by the sender.
func (s *Server) receivePush(r *http.Request) (status int, msg string, retry time.Duration, ttl int) {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "aes128gcm") {
		return http.StatusUnsupportedMediaType, "Content-Encoding must be aes128gcm", 0, 0
	}
	ttlHeader := strings.TrimSpace(r.Header.Get("TTL"))
	if ttlHeader == "" {
		return http.StatusBadRequest, "TTL header required", 0, 0
	}
	ttl, err := strconv.Atoi(ttlHeader)
	if err != nil || ttl < 0 {
		return http.StatusBadRequest, "TTL must be a non-negative integer", 0, 0
	}
	ttl = min(ttl, maxTTL)
	urgency := strings.ToLower(strings.TrimSpace(r.Header.Get("Urgency")))
	switch urgency {
	case "":
		urgency = "normal"
	case "very-low", "low", "normal", "high":
	default:
		return http.StatusBadRequest, "Urgency must be very-low, low, normal or high", 0, ttl
	}
	topic := r.Header.Get("Topic")
	if topic != "" && !topicRe.MatchString(topic) {
		return http.StatusBadRequest, "Topic must be at most 32 of [A-Za-z0-9_-]", 0, ttl
	}

	if r.ContentLength > maxPushBody {
		return http.StatusRequestEntityTooLarge, "body exceeds 4096 bytes", 0, ttl
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPushBody+1))
	if err != nil {
		return http.StatusBadRequest, "could not read body", 0, ttl
	}
	if len(body) > maxPushBody {
		return http.StatusRequestEntityTooLarge, "body exceeds 4096 bytes", 0, ttl
	}
	if !plausibleAES128GCM(body) {
		return http.StatusBadRequest, "body is not an aes128gcm message", 0, ttl
	}

	senderKey, err := VerifyVAPID(r.Header.Get("Authorization"), s.audience, s.now())
	if err != nil {
		return http.StatusUnauthorized, err.Error(), 0, ttl
	}
	sealed := r.PathValue("sealed")
	reg, err := s.sealer.Open(sealed)
	if err != nil {
		return http.StatusNotFound, "no such endpoint", 0, ttl
	}

	if ok, wait := s.senderLimit.allow(string(senderKey)); !ok {
		return http.StatusTooManyRequests, "too many pushes from this VAPID key", wait, ttl
	}
	endpointKey := sha256.Sum256([]byte(sealed))
	if ok, wait := s.endpointLimit.allow(string(endpointKey[:])); !ok {
		return http.StatusTooManyRequests, "too many pushes to this endpoint", wait, ttl
	}

	t := s.transports[reg.Platform]
	if t == nil {
		if reg.Platform == PlatformDebug {
			// A development endpoint reaching a gateway that is not in debug mode will
			// never be deliverable here.
			return http.StatusGone, "debug platform not enabled", 0, ttl
		}
		// The operator switched the platform off (or broke its config). That is not the
		// device's fault, so not 410: the instance retries and keeps the subscription.
		return http.StatusServiceUnavailable, "platform not configured", 0, ttl
	}

	ctx, cancel := context.WithTimeout(r.Context(), deliverTimeout)
	defer cancel()
	start := s.now()
	err = t.Send(ctx, &Message{Reg: reg, Body: body, TTL: ttl, Urgency: urgency, Topic: topic})
	if err == nil {
		logger.Debug(logTag, "push platform=%s urgency=%s status=201 took=%s", reg.Platform, urgency, s.now().Sub(start).Round(time.Millisecond))
		return http.StatusCreated, "", 0, ttl
	}
	var de *DeliveryError
	if !errors.As(err, &de) {
		logger.Error(logTag, "push platform=%s: %v", reg.Platform, err)
		return http.StatusInternalServerError, "internal error", 0, ttl
	}
	status = de.httpStatus()
	if status == http.StatusBadGateway {
		logger.Warn(logTag, "push platform=%s upstream=%d reason=%s", reg.Platform, de.Upstream, de.Reason)
	} else {
		logger.Debug(logTag, "push platform=%s status=%d upstream=%d reason=%s", reg.Platform, status, de.Upstream, de.Reason)
	}
	return status, "upstream: " + de.Reason, de.RetryAfter, ttl
}

// plausibleAES128GCM checks the RFC 8188 header shape only - salt(16) rs(4) idlen(1)
// keyid - with the 65-byte uncompressed key id RFC 8291 requires. The rest is ciphertext
// for the device.
func plausibleAES128GCM(body []byte) bool {
	if len(body) < minPushBody {
		return false
	}
	rs := binary.BigEndian.Uint32(body[16:20])
	return rs >= 18 && body[20] == 65 && body[21] == 0x04
}

func (s *Server) handleDebugPushes(w http.ResponseWriter, r *http.Request) {
	var after uint64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "after must be a sequence number")
			return
		}
		after = n
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"pushes": s.debug.after(after)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeRetryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(d)))
}

// stats are counts only - never anything about who or what - logged periodically so an
// operator can see the gateway working without it keeping a record of any push.
type stats struct {
	registered, received, delivered, gone, throttled, rejected, failed atomic.Uint64
}

// LogStats logs the counters accumulated over each interval until ctx ends, skipping
// intervals with no traffic.
func (s *Server) LogStats(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st := &s.stats
			reg, rec := st.registered.Swap(0), st.received.Swap(0)
			del, gone, thr, rej, fail := st.delivered.Swap(0), st.gone.Swap(0), st.throttled.Swap(0), st.rejected.Swap(0), st.failed.Swap(0)
			if reg+rec+thr == 0 {
				continue
			}
			logger.Info(logTag, "last %s: registered=%d pushes=%d delivered=%d gone=%d throttled=%d rejected=%d failed=%d",
				interval, reg, rec, del, gone, thr, rej, fail)
		}
	}
}
