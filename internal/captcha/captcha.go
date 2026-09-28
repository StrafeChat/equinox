// Package captcha verifies a client-supplied challenge answer at registration, so an
// instance that is open to the public isn't trivially farmed for accounts.
//
// Four providers, chosen per instance with CAPTCHA_PROVIDER:
//
//   - turnstile: Cloudflare Turnstile. Hosted; the browser loads Cloudflare's script.
//   - friendly:  Friendly Captcha v2. Hosted API, but the widget is bundled with the client
//     (no third-party script), and the puzzle is proof-of-work rather than tracking.
//   - altcha:    ALTCHA. Entirely self-hosted: this server issues an HMAC-signed
//     proof-of-work challenge and checks the answer itself. No third party, no keys to
//     register, no request ever leaves the instance - the natural default for a project
//     whose first principle is privacy.
//   - cap:       Cap (https://trycap.dev). Also self-hosted, but as its own service rather
//     than inside this one: a Cap container issues and grades proof-of-work challenges and
//     keeps its own stats dashboard, and this server only asks it whether a token is good.
//     Choose it over altcha when you want that dashboard, or one Cap shared by several
//     apps; altcha stays the default because it needs no extra container at all.
//
// It is opt-in (CAPTCHA=true); instances that leave it off behave exactly as before.
package captcha

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	altcha "github.com/altcha-org/altcha-lib-go"
)

var (
	// ErrMissingToken is returned when the client sent no captcha token at all.
	ErrMissingToken = errors.New("captcha token is required")
	// ErrFailed is returned when the provider rejected the token (expired, reused, forged).
	ErrFailed = errors.New("captcha verification failed")
	// ErrUnavailable means the provider could not be reached or answered unusably. It is
	// deliberately distinct from ErrFailed: a caller must decide whether an outage should
	// block registration, not silently treat it as a pass.
	ErrUnavailable = errors.New("captcha provider unavailable")
)

// Verifier checks one token on behalf of one registration attempt.
type Verifier interface {
	Verify(ctx context.Context, token, remoteIP string) error
}

// Challenger is implemented by providers whose challenge is issued by this server rather
// than by a third party. The auth routes expose Challenge() to the client when present.
type Challenger interface {
	Challenge(ctx context.Context) (any, error)
}

// Short timeout on purpose: registration is an interactive request, and a hung provider
// must surface as an error rather than holding the connection open.
const verifyTimeout = 10 * time.Second

// --- Cloudflare Turnstile -------------------------------------------------------------

const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

type turnstile struct {
	secret string
	client *http.Client
	url    string
}

// NewTurnstile builds a verifier for Cloudflare Turnstile.
func NewTurnstile(secret string) Verifier {
	return &turnstile{
		secret: secret,
		client: &http.Client{Timeout: verifyTimeout},
		url:    turnstileVerifyURL,
	}
}

type turnstileResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

func (t *turnstile) Verify(ctx context.Context, token, remoteIP string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissingToken
	}
	form := url.Values{}
	form.Set("secret", t.secret)
	form.Set("response", token)
	// Optional, and only meaningful when the address is actually the client's - behind an
	// untrusted proxy c.IP() is the proxy, which would make every attempt look identical.
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(form.Encode()))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := t.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	var out turnstileResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return ErrUnavailable
	}
	if !out.Success {
		return ErrFailed
	}
	return nil
}

// --- Friendly Captcha v2 --------------------------------------------------------------

const friendlyVerifyURL = "https://global.frcapi.com/api/v2/captcha/siteverify"

type friendly struct {
	apiKey  string
	siteKey string
	client  *http.Client
	url     string
}

// NewFriendlyCaptcha builds a verifier for Friendly Captcha v2. The site key is sent along
// so the API also confirms the answer was produced for *this* site's widget.
func NewFriendlyCaptcha(apiKey, siteKey string) Verifier {
	return &friendly{
		apiKey:  apiKey,
		siteKey: siteKey,
		client:  &http.Client{Timeout: verifyTimeout},
		url:     friendlyVerifyURL,
	}
}

type friendlyResponse struct {
	Success bool `json:"success"`
	Error   *struct {
		Code   string `json:"error_code"`
		Detail string `json:"detail"`
	} `json:"error"`
}

func (f *friendly) Verify(ctx context.Context, token, remoteIP string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissingToken
	}
	body, err := json.Marshal(map[string]string{"response": token, "sitekey": f.siteKey})
	if err != nil {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", f.apiKey)

	res, err := f.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer res.Body.Close()
	// The API answers 200 for both a valid and an invalid response and says which in the
	// body; anything else (401 bad key, 400 malformed, 5xx) is the *service* not answering
	// the question, which the caller reports as an outage rather than as a bot.
	if res.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	var out friendlyResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return ErrUnavailable
	}
	if !out.Success {
		return ErrFailed
	}
	return nil
}

// --- Cap (self-hosted, https://trycap.dev) ----------------------------------------------

type capProvider struct {
	// baseURL is how *this server* reaches the Cap instance; inside compose that is the
	// service name, which never leaves the private network. The browser is told a
	// different, public URL (config.CaptchaConfig.CapPublicURL).
	baseURL string
	siteKey string
	secret  string
	client  *http.Client
	// Cap cannot distinguish a spent token from bad credentials (see Verify); warn once
	// so an operator whose keys are wrong has something to find.
	warnOnce sync.Once
}

// NewCap builds a verifier for a Cap standalone server. baseURL is the instance root
// (e.g. http://cap:3000), siteKey identifies which of that instance's sites this is, and
// secret is the site's secret key - server-side only.
func NewCap(baseURL, siteKey, secret string) Verifier {
	return &capProvider{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		siteKey: strings.Trim(strings.TrimSpace(siteKey), "/"),
		secret:  secret,
		client:  &http.Client{Timeout: verifyTimeout},
	}
}

// capResponse is Cap's siteverify answer. Only `success` decides; the rest is for logs.
type capResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

func (p *capProvider) Verify(ctx context.Context, token, _ string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissingToken
	}
	body, err := json.Marshal(map[string]string{"secret": p.secret, "response": token})
	if err != nil {
		return ErrUnavailable
	}
	// POST <instance>/<site key>/siteverify - the site key is part of the path, which is
	// how one Cap instance serves several sites.
	endpoint := p.baseURL + "/" + p.siteKey + "/siteverify"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := p.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer res.Body.Close()
	// Cap answers 200 {"success":true} for a good token, 400 for one it cannot parse, and
	// 404 "Invalid site key or secret" for one it does not know - which includes the
	// ordinary cases of a token that has expired or already been spent. All three are Cap
	// answering the question, so they are a failed challenge rather than an outage; a
	// status that means Cap did not answer (5xx, an unreachable container) is
	// ErrUnavailable, which the caller surfaces differently because it needs an operator.
	//
	// The wrinkle is that 404 is *also* what a wrong CAP_SITE_KEY or CAP_SECRET_KEY
	// produces, with the same body - Cap gives us no way to tell those apart. Reading it
	// as a failure is the right call for visitors (an expired token is common; the form
	// re-arms and says so, instead of claiming the server is broken) and costs nothing in
	// safety, since either way nobody is registered. It would hide a misconfiguration,
	// though, so say so in the log the first time.
	switch res.StatusCode {
	case http.StatusOK, http.StatusBadRequest:
	case http.StatusNotFound:
		p.warnOnce.Do(func() {
			logger.Warn("captcha", "Cap rejected a token as unknown (expired, already used, or wrong credentials). "+
				"If every registration is failing, check CAP_SITE_KEY and CAP_SECRET_KEY against the Cap dashboard.")
		})
	default:
		return ErrUnavailable
	}
	var out capResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return ErrUnavailable
	}
	if !out.Success {
		return ErrFailed
	}
	return nil
}

// --- ALTCHA ----------------------------------------------------------------------------

// altchaChallengeTTL bounds how long a solved challenge stays valid. Long enough to fill in
// a registration form, short enough that a harvested-and-replayed solution is worthless.
const altchaChallengeTTL = 10 * time.Minute

// altchaMaxNumber sets the proof-of-work cost: the answer is a number in [0, max] found by
// hashing candidates. 1e6 is the library default and takes a browser well under a second.
const altchaMaxNumber = 1_000_000

// ReplayGuard remembers which challenges have already been spent.
//
// Verifying an ALTCHA solution is stateless - the signature proves this server issued the
// challenge, and the hash proves the client did the work - but nothing about it stops the
// same solved payload being submitted again. Without this, one solved puzzle would cover
// unlimited registrations until it expired, which is exactly the farming the captcha is
// there to prevent. Consume returns false if id was seen before; ttl bounds how long the
// memory is kept (a challenge is worthless after it expires anyway).
type ReplayGuard interface {
	Consume(ctx context.Context, id string, ttl time.Duration) (bool, error)
}

type altchaProvider struct {
	hmacKey string
	spent   ReplayGuard
}

// NewAltcha builds a self-hosted proof-of-work verifier. hmacKey signs challenges so that
// only ones this server issued (and that have not expired) are accepted; spent stops a
// solution from being reused.
func NewAltcha(hmacKey string, spent ReplayGuard) Verifier {
	if spent == nil {
		spent = NewMemoryReplayGuard()
	}
	return &altchaProvider{hmacKey: hmacKey, spent: spent}
}

// Challenge issues a fresh, signed, expiring challenge for the widget to solve.
func (a *altchaProvider) Challenge(_ context.Context) (any, error) {
	expires := time.Now().Add(altchaChallengeTTL)
	ch, err := altcha.CreateChallenge(altcha.ChallengeOptions{
		HMACKey:   a.hmacKey,
		MaxNumber: altchaMaxNumber,
		Expires:   &expires,
	})
	if err != nil {
		return nil, err
	}
	return ch, nil
}

func (a *altchaProvider) Verify(ctx context.Context, token, _ string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissingToken
	}
	// The token is the widget's base64 JSON payload. A malformed one is a failed
	// verification, not an outage - there is no service here that could be down.
	ok, err := altcha.VerifySolutionSafe(token, a.hmacKey, true)
	if err != nil || !ok {
		return ErrFailed
	}
	// Spend it. The challenge hash identifies the puzzle; once a valid answer to it has
	// been accepted, a second copy of the same answer is a replay, not a second human.
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return ErrFailed
	}
	var p altcha.Payload
	if err := json.Unmarshal(raw, &p); err != nil || p.Challenge == "" {
		return ErrFailed
	}
	fresh, err := a.spent.Consume(ctx, p.Challenge, altchaChallengeTTL)
	if err != nil {
		return ErrUnavailable
	}
	if !fresh {
		return ErrFailed
	}
	return nil
}

// memoryReplayGuard is the single-process fallback: fine for one API instance, and what
// tests use. Multi-instance deployments pass a shared store (Redis) instead.
type memoryReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewMemoryReplayGuard returns an in-process ReplayGuard.
func NewMemoryReplayGuard() ReplayGuard {
	return &memoryReplayGuard{seen: make(map[string]time.Time)}
}

func (m *memoryReplayGuard) Consume(_ context.Context, id string, ttl time.Duration) (bool, error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Sweep expired entries opportunistically so the map cannot grow without bound.
	for k, exp := range m.seen {
		if now.After(exp) {
			delete(m.seen, k)
		}
	}
	if exp, ok := m.seen[id]; ok && now.Before(exp) {
		return false, nil
	}
	m.seen[id] = now.Add(ttl)
	return true, nil
}
