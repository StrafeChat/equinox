package pushgateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// ErrVAPIDMissing means the request carried no RFC 8292 `vapid` authorization at all;
// every other verification failure wraps ErrVAPIDInvalid. Both answer 401.
var (
	ErrVAPIDMissing = errors.New("VAPID authorization required")
	ErrVAPIDInvalid = errors.New("invalid VAPID authorization")
)

const (
	// maxVAPIDLifetime is RFC 8292's ceiling on how far ahead `exp` may be.
	maxVAPIDLifetime = 24 * time.Hour
	// vapidClockSkew tolerates a sender whose clock runs a little ahead of ours, so a token
	// minted for exactly 24 hours is not refused for being a few seconds "too long".
	vapidClockSkew = 5 * time.Minute
)

// VerifyVAPID checks an `Authorization: vapid t=<jwt>, k=<key>` header (RFC 8292 §3)
// against the audience this gateway serves and returns the sender's raw 65-byte public
// key - the identity rate limits are keyed by.
//
// Requiring VAPID is what keeps the gateway from being an open relay into everyone's
// phones: a push must at least be signed by a stable sender key, which can be limited.
func VerifyVAPID(header, audience string, now time.Time) ([]byte, error) {
	header = strings.TrimSpace(header)
	scheme, params, _ := strings.Cut(header, " ")
	if header == "" || !strings.EqualFold(scheme, "vapid") {
		return nil, ErrVAPIDMissing
	}
	var token, key string
	for _, p := range strings.Split(params, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "t":
			token = value
		case "k":
			key = value
		}
	}
	if token == "" || key == "" {
		return nil, fmt.Errorf("%w: t and k are both required", ErrVAPIDInvalid)
	}

	// Some senders pad base64url; the key itself is unambiguous either way.
	rawKey, err := b64.DecodeString(strings.TrimRight(key, "="))
	if err != nil || len(rawKey) != 65 {
		return nil, fmt.Errorf("%w: k is not an uncompressed P-256 public key", ErrVAPIDInvalid)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), rawKey)
	if err != nil {
		return nil, fmt.Errorf("%w: k is not a point on P-256", ErrVAPIDInvalid)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: malformed JWT", ErrVAPIDInvalid)
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := decodeJWTPart(parts[0], &hdr); err != nil || hdr.Alg != "ES256" {
		return nil, fmt.Errorf("%w: JWT must be ES256", ErrVAPIDInvalid)
	}
	sig, err := b64.DecodeString(strings.TrimRight(parts[2], "="))
	if err != nil || len(sig) != 64 {
		return nil, fmt.Errorf("%w: ES256 signature must be 64 raw bytes", ErrVAPIDInvalid)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return nil, fmt.Errorf("%w: bad signature", ErrVAPIDInvalid)
	}

	var claims struct {
		Aud string      `json:"aud"`
		Exp json.Number `json:"exp"`
		Sub string      `json:"sub"`
	}
	if err := decodeJWTPart(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("%w: malformed claims", ErrVAPIDInvalid)
	}
	if normalizeOrigin(claims.Aud) != audience {
		return nil, fmt.Errorf("%w: aud must be %s", ErrVAPIDInvalid, audience)
	}
	expF, err := claims.Exp.Float64()
	if err != nil {
		return nil, fmt.Errorf("%w: exp missing", ErrVAPIDInvalid)
	}
	exp := time.Unix(int64(expF), 0)
	if !exp.After(now) {
		return nil, fmt.Errorf("%w: expired", ErrVAPIDInvalid)
	}
	if exp.After(now.Add(maxVAPIDLifetime + vapidClockSkew)) {
		return nil, fmt.Errorf("%w: exp more than 24h ahead", ErrVAPIDInvalid)
	}
	if strings.TrimSpace(claims.Sub) == "" {
		return nil, fmt.Errorf("%w: sub missing", ErrVAPIDInvalid)
	}
	return rawKey, nil
}

func decodeJWTPart(part string, v any) error {
	raw, err := b64.DecodeString(strings.TrimRight(part, "="))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	return dec.Decode(v)
}

// normalizeOrigin reduces a URL to scheme://host[:port] the way a sender computes the
// VAPID audience: lower-case, no path, default port dropped. "" when it is not a URL.
func normalizeOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}
