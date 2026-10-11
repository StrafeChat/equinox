package pushgateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Platforms a registration can name.
const (
	PlatformFCM   = "fcm"
	PlatformAPNs  = "apns"
	PlatformDebug = "debug"
)

const (
	sealVersion = 0x01
	sealAAD     = "strafe-pushgw/1"
	// maxSealedLen bounds what Open will even try to decode. The largest registration (a
	// 4096-byte FCM token plus a 120-rune fallback) seals to well under this.
	maxSealedLen = 8192
)

// ErrBadEndpoint is returned for every endpoint that does not open: malformed, tampered,
// or sealed under a key this gateway no longer holds. The handler answers 404 for all of
// them alike, which is also what tells an instance to forget the subscription.
var ErrBadEndpoint = errors.New("unknown push endpoint")

// Registration is everything the gateway needs to deliver to one device, carried inside
// the endpoint URL itself so the gateway stores nothing.
type Registration struct {
	Platform string `json:"p"`
	Token    string `json:"t"`
	Slot     string `json:"s"`
	Sandbox  bool   `json:"x"`
	Fallback string `json:"f"`
	// Random is 8 fresh random bytes per registration. It makes every endpoint distinct
	// even for the same device, so two instances given endpoints for one phone cannot tell
	// that they are talking to the same person by comparing them.
	Random string `json:"r"`
}

// Sealer seals registrations into endpoint path segments and opens them again.
type Sealer struct {
	current cipher.AEAD
	old     []cipher.AEAD
}

// NewSealer builds a Sealer from the current 32-byte key and any retired keys that should
// still open endpoints issued before a rotation.
func NewSealer(key []byte, old [][]byte) (*Sealer, error) {
	cur, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	s := &Sealer{current: cur}
	for _, k := range old {
		aead, err := newGCM(k)
		if err != nil {
			return nil, err
		}
		s.old = append(s.old, aead)
	}
	return s, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("seal key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal returns base64url-no-padding(0x01 || nonce || AES-256-GCM(registration JSON)),
// filling in reg.Random.
func (s *Sealer) Seal(reg Registration) (string, error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	reg.Random = base64.RawURLEncoding.EncodeToString(r[:])
	plain, err := json.Marshal(reg)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.current.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plain)+s.current.Overhead())
	out = append(append(out, sealVersion), nonce...)
	out = s.current.Seal(out, nonce, plain, []byte(sealAAD))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// Open reverses Seal, trying the current key first and then each retired one.
func (s *Sealer) Open(sealed string) (*Registration, error) {
	if len(sealed) == 0 || len(sealed) > maxSealedLen {
		return nil, ErrBadEndpoint
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(sealed)
	if err != nil || len(raw) < 1+12+16 || raw[0] != sealVersion {
		return nil, ErrBadEndpoint
	}
	nonce, ct := raw[1:13], raw[13:]
	for _, aead := range append([]cipher.AEAD{s.current}, s.old...) {
		plain, err := aead.Open(nil, nonce, ct, []byte(sealAAD))
		if err != nil {
			continue
		}
		var reg Registration
		if json.Unmarshal(plain, &reg) != nil || reg.Platform == "" || reg.Token == "" {
			return nil, ErrBadEndpoint
		}
		return &reg, nil
	}
	return nil, ErrBadEndpoint
}
