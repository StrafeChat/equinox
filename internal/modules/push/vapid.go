package push

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"
)

// RFC 8292 VAPID: the instance identifies itself to push services with an ES256-signed
// JWT naming the push service's origin, plus its public key. The key is the instance's
// own (PUSH_VAPID_PRIVATE_KEY, or one generated on first start and kept in the database);
// a push gateway or a UnifiedPush server may rate-limit or block per key.

const (
	// vapidTTL is the JWT lifetime; RFC 8292 caps it at 24 h and gateways may refuse more.
	vapidTTL = 12 * time.Hour
	// vapidReuse is how long a signed token is reused for the same audience - well inside
	// its lifetime, so a cached token never reaches a push service close to expiry.
	vapidReuse = time.Hour
)

// VAPID is the instance's application-server identity.
type VAPID struct {
	key       *ecdsa.PrivateKey
	publicB64 string
	subject   string

	mu    sync.Mutex
	cache map[string]vapidToken
	now   func() time.Time
}

type vapidToken struct {
	jwt     string
	expires time.Time
}

// keyStore is where a generated key is kept so every API replica uses the same one.
type keyStore interface {
	// ClaimVAPIDKey stores candidate if no key is stored yet and returns whichever key is
	// stored afterwards (candidate, or the one another replica claimed first).
	ClaimVAPIDKey(ctx context.Context, candidate string) (string, error)
}

// LoadVAPID returns the configured key, or the stored one, generating and storing a key
// the first time an instance starts without one.
func LoadVAPID(ctx context.Context, configured, subject string, store keyStore) (*VAPID, error) {
	raw := configured
	if raw == "" {
		k, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		raw, err = store.ClaimVAPIDKey(ctx, b64(k.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("push: storing the VAPID key: %w", err)
		}
	}
	v, err := NewVAPID(raw, subject)
	if err != nil && configured != "" {
		return nil, errors.New("PUSH_VAPID_PRIVATE_KEY must be base64url of a 32-byte P-256 private key")
	}
	return v, err
}

// NewVAPID builds the identity from a base64url raw 32-byte P-256 scalar.
func NewVAPID(privateB64, subject string) (*VAPID, error) {
	raw, err := decodeB64(privateB64)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("push: VAPID private key must be 32 bytes, base64url")
	}
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("push: VAPID private key: %w", err)
	}
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &VAPID{key: key, publicB64: b64(pub), subject: subject, cache: map[string]vapidToken{}, now: time.Now}, nil
}

// PublicKey is the base64url uncompressed public key (the "k" of the Authorization header,
// and what a browser's PushManager takes as applicationServerKey).
func (v *VAPID) PublicKey() string { return v.publicB64 }

// audience is the origin of an endpoint: scheme://host[:port].
func audience(endpoint *url.URL) string {
	return endpoint.Scheme + "://" + endpoint.Host
}

// Authorization returns the Authorization header value for a push to endpoint.
func (v *VAPID) Authorization(endpoint *url.URL) (string, error) {
	jwt, err := v.token(audience(endpoint))
	if err != nil {
		return "", err
	}
	return "vapid t=" + jwt + ", k=" + v.publicB64, nil
}

func (v *VAPID) token(aud string) (string, error) {
	now := v.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if t, ok := v.cache[aud]; ok && now.Before(t.expires) {
		return t.jwt, nil
	}
	jwt, err := v.sign(aud, now.Add(vapidTTL))
	if err != nil {
		return "", err
	}
	if len(v.cache) > 1000 {
		// Audiences are push services, a handful in practice; never let a stream of odd
		// endpoints grow this without bound.
		clear(v.cache)
	}
	v.cache[aud] = vapidToken{jwt: jwt, expires: now.Add(vapidReuse)}
	return jwt, nil
}

// sign makes an ES256 JWT: base64url(header).base64url(claims).base64url(r||s).
func (v *VAPID) sign(aud string, exp time.Time) (string, error) {
	header := b64([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{aud, exp.Unix(), v.subject})
	if err != nil {
		return "", err
	}
	signing := header + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS (RFC 7518 §3.4) wants the raw fixed-width pair, not the ASN.1 form.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64(sig), nil
}
