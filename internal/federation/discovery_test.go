package federation

import (
	"testing"

	"github.com/StrafeChat/equinox/internal/config"
)

func TestValidPeerDomain(t *testing.T) {
	strict := config.FederationConfig{Enabled: true, Domain: "here.example"}
	dev := config.FederationConfig{Enabled: true, Domain: "a.local", AllowInsecure: true,
		StaticPeers: map[string]string{"b.local": "http://127.0.0.1:4100"}}

	cases := []struct {
		cfg    config.FederationConfig
		domain string
		want   bool
	}{
		{strict, "chat.example.com", true},
		{strict, "chat.example.com:8443", true},
		{strict, "sub-domain.example.co.uk", true},
		// Shapes that could never be a public instance but would make discovery open a
		// connection to this host's own network.
		{strict, "127.0.0.1", false},
		{strict, "169.254.169.254", false},
		{strict, "10.0.0.5:4000", false},
		{strict, "localhost", false},
		{strict, "scylla.internal", false},
		{strict, "printer.local", false},
		// Not a hostname at all.
		{strict, "", false},
		{strict, "evil.example/path", false},
		{strict, "user@evil.example", false},
		{strict, "-bad.example", false},
		{strict, "UPPER.example", false},
		// Development: static peers and insecure names are explicitly allowed.
		{dev, "b.local", true},
		{dev, "localhost:4100", true},
	}
	for _, c := range cases {
		if got := ValidPeerDomain(c.cfg, c.domain); got != c.want {
			t.Errorf("ValidPeerDomain(%q) = %v, want %v", c.domain, got, c.want)
		}
	}
}

func TestClampProfile(t *testing.T) {
	long := make([]rune, 0, 300)
	for i := 0; i < 300; i++ {
		long = append(long, 'x')
	}
	p := clampProfile(Profile{
		FID:         "@1:other.example",
		Username:    string(long),
		DisplayName: "  padded  ",
		Bio:         string(long),
		Avatar:      "javascript:alert(1)",
		Banner:      "https://cdn.other.example/v1/banners/1/x.png",
	})
	if len([]rune(p.Username)) != 32 || len([]rune(p.Bio)) != 190 {
		t.Errorf("username/bio not clamped: %d/%d", len([]rune(p.Username)), len([]rune(p.Bio)))
	}
	if p.DisplayName != "padded" {
		t.Errorf("display name not trimmed: %q", p.DisplayName)
	}
	if p.Avatar != "" {
		t.Errorf("non-http avatar kept: %q", p.Avatar)
	}
	if p.Banner == "" {
		t.Error("https banner dropped")
	}
}
