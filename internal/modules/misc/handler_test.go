package misc

import (
	"testing"

	"github.com/StrafeChat/equinox/internal/config"
)

// A fully configured instance: required services (gateway, web) appear URL-only with no
// enabled flag; optional services (cdn, voice) appear with enabled=true and their URL.
func TestServicesFullyConfigured(t *testing.T) {
	cfg := &config.Config{}
	cfg.Stargate.PublicURL = "wss://chat.example.com/gateway/events"
	cfg.App.WebURL = "https://web.strafe.chat"
	cfg.Nebula.BaseURL = "http://nebula:4010"
	cfg.Nebula.PublicURL = "https://cdn.example.com"
	cfg.Nebula.UploadSecret = "secret"
	cfg.Voice.Enabled = true
	cfg.Voice.PublicURL = "wss://chat.example.com/livekit"

	s := services(cfg)

	if s.Stargate == nil || s.Stargate.URL != "wss://chat.example.com/gateway/events" {
		t.Fatalf("stargate url wrong: %+v", s.Stargate)
	}
	if s.Stargate.Enabled != nil {
		t.Error("stargate is a required service and must not carry an enabled flag")
	}
	if s.Web == nil || s.Web.URL != "https://web.strafe.chat" {
		t.Fatalf("web url wrong: %+v", s.Web)
	}
	if s.Web.Enabled != nil {
		t.Error("web is a required service and must not carry an enabled flag")
	}
	if s.CDN.Enabled == nil || !*s.CDN.Enabled {
		t.Fatalf("cdn should be enabled: %+v", s.CDN)
	}
	// The CDN advertises the browser-facing public URL, not the internal base.
	if s.CDN.URL != "https://cdn.example.com" {
		t.Errorf("cdn url = %q, want the public url", s.CDN.URL)
	}
	if s.Voice.Enabled == nil || !*s.Voice.Enabled {
		t.Fatalf("voice should be enabled: %+v", s.Voice)
	}
	if s.Voice.URL != "wss://chat.example.com/livekit" {
		t.Errorf("voice url = %q, want the public wss url", s.Voice.URL)
	}
}

// A bare instance: gateway/web omitted entirely; cdn/voice present but disabled with no URL.
func TestServicesUnconfigured(t *testing.T) {
	s := services(&config.Config{})

	if s.Stargate != nil {
		t.Errorf("stargate should be omitted when no url is configured: %+v", s.Stargate)
	}
	if s.Web != nil {
		t.Errorf("web should be omitted when no url is configured: %+v", s.Web)
	}
	if s.CDN.Enabled == nil || *s.CDN.Enabled {
		t.Errorf("cdn should be present and disabled: %+v", s.CDN)
	}
	if s.CDN.URL != "" {
		t.Errorf("disabled cdn should carry no url, got %q", s.CDN.URL)
	}
	if s.Voice.Enabled == nil || *s.Voice.Enabled {
		t.Errorf("voice should be present and disabled: %+v", s.Voice)
	}
	if s.Voice.URL != "" {
		t.Errorf("disabled voice should carry no url, got %q", s.Voice.URL)
	}
}

// The gateway URL falls back to the federation gateway URL when no explicit public URL is set.
func TestServicesStargateFallsBackToFederationGateway(t *testing.T) {
	cfg := &config.Config{}
	cfg.Federation.GatewayURL = "wss://a.local/gateway/events"

	s := services(cfg)
	if s.Stargate == nil || s.Stargate.URL != "wss://a.local/gateway/events" {
		t.Fatalf("stargate should fall back to the federation gateway url: %+v", s.Stargate)
	}

	// An explicit public URL wins over the federation gateway URL.
	cfg.Stargate.PublicURL = "wss://explicit.example.com/events"
	if got := services(cfg).Stargate.URL; got != "wss://explicit.example.com/events" {
		t.Errorf("explicit STARGATE_PUBLIC_URL should win, got %q", got)
	}
}

// The CDN needs both a base URL and an upload secret to count as enabled - a half
// configuration (uploads would fail) reports disabled rather than advertising a dead URL.
func TestServicesCDNNeedsBaseAndSecret(t *testing.T) {
	cfg := &config.Config{}
	cfg.Nebula.BaseURL = "http://nebula:4010" // upload secret missing

	s := services(cfg)
	if s.CDN.Enabled == nil || *s.CDN.Enabled {
		t.Errorf("cdn should be disabled without an upload secret: %+v", s.CDN)
	}
	if s.CDN.URL != "" {
		t.Errorf("half-configured cdn should carry no url, got %q", s.CDN.URL)
	}
}

// Email is advertised as on only when an SMTP host is configured, and "verification
// required" never shows without it - config.Load refuses that combination, but the
// feature map must not depend on that having run.
func TestEmailFeature(t *testing.T) {
	cfg := &config.Config{}
	if f := emailFeature(cfg); f.Enabled || f.VerificationRequired {
		t.Errorf("no SMTP host: want all off, got %+v", f)
	}
	cfg.Mail.VerificationRequired = true
	if f := emailFeature(cfg); f.Enabled || f.VerificationRequired {
		t.Errorf("verification without mail must read as off, got %+v", f)
	}
	cfg.Mail.Enabled = true
	if f := emailFeature(cfg); !f.Enabled || !f.VerificationRequired {
		t.Errorf("mail on + verification: want both on, got %+v", f)
	}
	cfg.Mail.VerificationRequired = false
	if f := emailFeature(cfg); !f.Enabled || f.VerificationRequired {
		t.Errorf("mail on, verification off: got %+v", f)
	}
}
