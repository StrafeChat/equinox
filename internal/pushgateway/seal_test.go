package pushgateway

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	s, err := NewSealer(randomKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := Registration{Platform: PlatformAPNs, Token: "ab12", Slot: "acc_1", Sandbox: true, Fallback: "New message"}
	sealed, err := s.Seal(reg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got.Platform != reg.Platform || got.Token != reg.Token || got.Slot != reg.Slot ||
		got.Sandbox != reg.Sandbox || got.Fallback != reg.Fallback {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if r, err := b64.DecodeString(got.Random); err != nil || len(r) != 8 {
		t.Fatalf("random r = %q, want 8 bytes base64url", got.Random)
	}

	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || raw[0] != 0x01 {
		t.Fatalf("sealed must be unpadded base64url starting with version 0x01")
	}

	again, _ := s.Seal(reg)
	if again == sealed {
		t.Fatal("two registrations of the same device must get distinct endpoints")
	}
}

func TestSealKeyRotation(t *testing.T) {
	oldKey, newKey := randomKey(t), randomKey(t)
	before, _ := NewSealer(oldKey, nil)
	sealed, err := before.Seal(Registration{Platform: PlatformFCM, Token: "tok", Slot: "s"})
	if err != nil {
		t.Fatal(err)
	}

	rotated, _ := NewSealer(newKey, [][]byte{oldKey})
	if _, err := rotated.Open(sealed); err != nil {
		t.Fatalf("an endpoint sealed under a retired key must still open: %v", err)
	}
	fresh, _ := rotated.Seal(Registration{Platform: PlatformFCM, Token: "tok", Slot: "s"})
	if _, err := before.Open(fresh); !errors.Is(err, ErrBadEndpoint) {
		t.Fatal("new endpoints must be sealed with the current key, not the old one")
	}

	dropped, _ := NewSealer(newKey, nil)
	if _, err := dropped.Open(sealed); !errors.Is(err, ErrBadEndpoint) {
		t.Fatalf("once the old key is gone its endpoints must not open, got %v", err)
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	s, _ := NewSealer(randomKey(t), nil)
	sealed, _ := s.Seal(Registration{Platform: PlatformFCM, Token: "tok", Slot: "s"})
	raw, _ := base64.RawURLEncoding.DecodeString(sealed)

	cases := map[string]string{
		"flipped ciphertext byte": func() string {
			b := bytes.Clone(raw)
			b[len(b)/2] ^= 0x01
			return base64.RawURLEncoding.EncodeToString(b)
		}(),
		"flipped nonce byte": func() string {
			b := bytes.Clone(raw)
			b[3] ^= 0x80
			return base64.RawURLEncoding.EncodeToString(b)
		}(),
		"wrong version": func() string {
			b := bytes.Clone(raw)
			b[0] = 0x02
			return base64.RawURLEncoding.EncodeToString(b)
		}(),
		"truncated":      sealed[:len(sealed)-4],
		"padded base64":  sealed + "==",
		"not base64":     "!!!!",
		"empty":          "",
		"absurdly large": string(bytes.Repeat([]byte("A"), maxSealedLen+1)),
	}
	for name, v := range cases {
		if _, err := s.Open(v); !errors.Is(err, ErrBadEndpoint) {
			t.Errorf("%s: got %v, want ErrBadEndpoint", name, err)
		}
	}
}

func TestNewSealerRejectsShortKeys(t *testing.T) {
	if _, err := NewSealer(make([]byte, 16), nil); err == nil {
		t.Fatal("a 16-byte key must be refused (AES-256 needs 32)")
	}
	if _, err := NewSealer(randomKey(t), [][]byte{make([]byte, 31)}); err == nil {
		t.Fatal("a bad old key must be refused too")
	}
}
