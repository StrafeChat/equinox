package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestVAPIDJWT(t *testing.T) {
	v := testVAPID(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	v.now = func() time.Time { return now }
	endpoint, _ := url.Parse("https://push.example.com:8443/v1/push/xyz?q=1")
	authz, err := v.Authorization(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyVAPID(t, authz, v.PublicKey())
	if claims["aud"] != "https://push.example.com:8443" {
		t.Errorf("aud = %v, want the endpoint's origin only", claims["aud"])
	}
	if int64(claims["exp"].(float64)) != now.Add(12*time.Hour).Unix() {
		t.Errorf("exp = %v, want now + 12h", claims["exp"])
	}
	if claims["sub"] != "mailto:ops@example.com" {
		t.Errorf("sub = %v", claims["sub"])
	}
	if raw, _ := decodeB64(v.PublicKey()); len(raw) != 65 || raw[0] != 4 {
		t.Error("public key is not an uncompressed point")
	}

	// Reused for the same audience within the hour, re-signed after it; a different
	// audience gets its own token.
	again, _ := v.Authorization(endpoint)
	if again != authz {
		t.Error("token not reused within the hour")
	}
	other, _ := url.Parse("https://other.example.com/p")
	if o, _ := v.Authorization(other); o == authz {
		t.Error("two audiences share a token")
	}
	now = now.Add(61 * time.Minute)
	if later, _ := v.Authorization(endpoint); later == authz {
		t.Error("token reused past the hour")
	}
}

func TestLoadVAPID(t *testing.T) {
	repo := newMemRepo()
	ctx := context.Background()
	// No key configured: one is generated and stored, and every later load (another
	// replica, a restart) gets the same one.
	a, err := LoadVAPID(ctx, "", "https://chat.example.com", repo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadVAPID(ctx, "", "https://chat.example.com", repo)
	if err != nil {
		t.Fatal(err)
	}
	if a.PublicKey() != b.PublicKey() || repo.key == "" {
		t.Error("generated key not shared through the store")
	}
	// A configured key wins and never touches the store.
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	c, err := LoadVAPID(ctx, b64(k.Bytes()), "https://chat.example.com", newMemRepo())
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicKey() != b64(k.PublicKey().Bytes()) {
		t.Error("configured key not used")
	}
	if _, err := LoadVAPID(ctx, "not-a-key", "https://x", repo); err == nil || !strings.Contains(err.Error(), "PUSH_VAPID_PRIVATE_KEY") {
		t.Errorf("bad configured key: %v", err)
	}
}
