package pushgateway

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

const testAud = "https://push.example.com"

func TestVerifyVAPID(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	key := newP256(t)
	good := vapidHeader(t, key, testAud, now.Add(12*time.Hour), "mailto:ops@example.com")

	got, err := VerifyVAPID(good, testAud, now)
	if err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	if !bytes.Equal(got, uncompressed(t, &key.PublicKey)) {
		t.Fatal("VerifyVAPID must return the sender's raw public key")
	}

	// Equivalent spellings of the same audience and header.
	for name, h := range map[string]string{
		"default port in aud": vapidHeader(t, key, "https://push.example.com:443", now.Add(time.Hour), "https://example.com"),
		"upper-case scheme":   strings.Replace(good, "vapid ", "Vapid ", 1),
		"padded k":            good + "=",
	} {
		if _, err := VerifyVAPID(h, testAud, now); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func TestVerifyVAPIDRejects(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	key, other := newP256(t), newP256(t)
	exp := now.Add(12 * time.Hour)

	badSig := func() string {
		// Signed by one key, presented with another's public key.
		h := vapidHeader(t, other, testAud, exp, "mailto:a@b.c")
		tok := strings.TrimPrefix(strings.Split(h, ",")[0], "vapid t=")
		return "vapid t=" + tok + ", k=" + b64.EncodeToString(uncompressed(t, &key.PublicKey))
	}()
	derSig := func() string {
		// A DER (ASN.1) signature is what crypto/ecdsa emits by default; JWS ES256 is raw
		// r||s and a DER one must not verify.
		input, _ := signingInput(map[string]string{"alg": "ES256"}, map[string]any{"aud": testAud, "exp": exp.Unix(), "sub": "mailto:a@b.c"})
		d := sha256.Sum256([]byte(input))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, d[:])
		return "vapid t=" + input + "." + b64.EncodeToString(sig) + ", k=" + b64.EncodeToString(uncompressed(t, &key.PublicKey))
	}()
	hs256 := func() string {
		input, _ := signingInput(map[string]string{"alg": "HS256"}, map[string]any{"aud": testAud, "exp": exp.Unix(), "sub": "x"})
		return "vapid t=" + input + "." + b64.EncodeToString(make([]byte, 64)) + ", k=" + b64.EncodeToString(uncompressed(t, &key.PublicKey))
	}()
	offCurve := func() string {
		k := uncompressed(t, &key.PublicKey)
		k[64] ^= 0x01
		h := vapidHeader(t, key, testAud, exp, "mailto:a@b.c")
		return h[:strings.Index(h, "k=")] + "k=" + b64.EncodeToString(k)
	}()

	cases := []struct {
		name, header string
		want         error
	}{
		{"missing", "", ErrVAPIDMissing},
		{"other scheme", "Bearer abc", ErrVAPIDMissing},
		{"no k", "vapid t=a.b.c", ErrVAPIDInvalid},
		{"wrong aud", vapidHeader(t, key, "https://evil.example.com", exp, "mailto:a@b.c"), ErrVAPIDInvalid},
		{"aud with other port", vapidHeader(t, key, "https://push.example.com:8443", exp, "mailto:a@b.c"), ErrVAPIDInvalid},
		{"expired", vapidHeader(t, key, testAud, now.Add(-time.Second), "mailto:a@b.c"), ErrVAPIDInvalid},
		{"exp now", vapidHeader(t, key, testAud, now, "mailto:a@b.c"), ErrVAPIDInvalid},
		{"exp beyond 24h", vapidHeader(t, key, testAud, now.Add(25*time.Hour), "mailto:a@b.c"), ErrVAPIDInvalid},
		{"no sub", vapidHeader(t, key, testAud, exp, ""), ErrVAPIDInvalid},
		{"bad signature", badSig, ErrVAPIDInvalid},
		{"DER signature", derSig, ErrVAPIDInvalid},
		{"not ES256", hs256, ErrVAPIDInvalid},
		{"key off the curve", offCurve, ErrVAPIDInvalid},
		{"garbage token", "vapid t=nope, k=" + b64.EncodeToString(uncompressed(t, &key.PublicKey)), ErrVAPIDInvalid},
	}
	for _, c := range cases {
		if _, err := VerifyVAPID(c.header, testAud, now); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://Push.Example.com/":      "https://push.example.com",
		"https://push.example.com:443":   "https://push.example.com",
		"http://127.0.0.1:4029/v1/push/": "http://127.0.0.1:4029",
		"http://[::1]:80":                "http://[::1]",
		"not a url":                      "",
	} {
		if got := normalizeOrigin(in); got != want {
			t.Errorf("normalizeOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}
