package config

import "testing"

func federatingCfg() *Config {
	cfg := baseValidCfg()
	cfg.Federation.Enabled = true
	cfg.Federation.PublicURL = "https://chat.example.org"
	return cfg
}

// A federating instance is reachable from the open internet, and an empty origin list makes
// the gateway accept a WebSocket from any site - with the session cookie, in the user's name.
// That has to be a startup failure, not a silently open instance.
func TestValidateRequiresGatewayOriginsWhenFederating(t *testing.T) {
	cfg := federatingCfg()
	cfg.Stargate.AllowedOrigins = nil
	if err := validate(cfg); err == nil {
		t.Fatal("want an error for a federating instance with no STARGATE_ALLOWED_ORIGINS, got nil")
	}
}

func TestValidateAcceptsFederatingWithGatewayOrigins(t *testing.T) {
	cfg := federatingCfg()
	cfg.Stargate.AllowedOrigins = []string{"https://chat.example.org"}
	if err := validate(cfg); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

// Local development opts out of the https requirement with FEDERATION_ALLOW_INSECURE, and the
// same switch has to cover the origin list, or two-instance dev setups stop booting.
func TestValidateInsecureDevSkipsGatewayOriginCheck(t *testing.T) {
	cfg := federatingCfg()
	cfg.Federation.AllowInsecure = true
	cfg.Federation.PublicURL = "http://a.local:4000"
	cfg.Stargate.AllowedOrigins = nil
	if err := validate(cfg); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

// Not federating means not public by default; a single-host instance behind a firewall must
// keep working with no origin list, as it always has.
func TestValidateNonFederatingNeedsNoGatewayOrigins(t *testing.T) {
	cfg := baseValidCfg()
	cfg.Stargate.AllowedOrigins = nil
	if err := validate(cfg); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}
