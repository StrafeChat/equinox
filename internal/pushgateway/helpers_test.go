package pushgateway

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable time source shared by the limiter, VAPID checks and the token
// caches under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_760_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func newP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func uncompressed(t *testing.T, k *ecdsa.PublicKey) []byte {
	t.Helper()
	pub, err := k.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	return pub.Bytes()
}

// vapidHeader builds `vapid t=<jwt>, k=<key>` the way an instance does (RFC 8292).
func vapidHeader(t *testing.T, key *ecdsa.PrivateKey, aud string, exp time.Time, sub string) string {
	t.Helper()
	claims := map[string]any{"aud": aud, "exp": exp.Unix()}
	if sub != "" {
		claims["sub"] = sub
	}
	jwt, err := signES256(key, map[string]string{"typ": "JWT", "alg": "ES256"}, claims)
	if err != nil {
		t.Fatal(err)
	}
	return "vapid t=" + jwt + ", k=" + b64.EncodeToString(uncompressed(t, &key.PublicKey))
}

// fakeAES128GCM returns a body with a well-formed RFC 8188/8291 header (fresh salt,
// rs 4096, a real 65-byte P-256 key id) followed by random "ciphertext" of n bytes. The
// gateway never decrypts, so the record itself only has to look like one.
func fakeAES128GCM(t *testing.T, n int) []byte {
	t.Helper()
	sender, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 16, 21+65+n)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	body = binary.BigEndian.AppendUint32(body, 4096)
	body = append(body, 65)
	body = append(body, sender.PublicKey().Bytes()...)
	ct := make([]byte, n)
	_, _ = rand.Read(ct)
	return append(body, ct...)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := parsePublicURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
