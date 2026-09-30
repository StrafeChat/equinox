package config

import "testing"

func TestValidateCaptchaPerProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     CaptchaConfig
		wantErr bool
	}{
		{"turnstile complete", CaptchaConfig{Provider: "turnstile", SiteKey: "s", SecretKey: "k"}, false},
		{"turnstile missing secret", CaptchaConfig{Provider: "turnstile", SiteKey: "s"}, true},
		{"friendly complete", CaptchaConfig{Provider: "friendly", SiteKey: "s", APIKey: "k"}, false},
		{"friendly missing api key", CaptchaConfig{Provider: "friendly", SiteKey: "s"}, true},
		{"friendly given turnstile's secret instead", CaptchaConfig{Provider: "friendly", SiteKey: "s", SecretKey: "k"}, true},
		{"altcha strong key", CaptchaConfig{Provider: "altcha", HMACKey: "0123456789abcdef0123456789abcdef0123456789abcdef"}, false},
		{"altcha short key", CaptchaConfig{Provider: "altcha", HMACKey: "short"}, true},
		{"altcha no key", CaptchaConfig{Provider: "altcha"}, true},
		{"unknown provider", CaptchaConfig{Provider: "hcaptcha", SiteKey: "s", SecretKey: "k"}, true},
		{"empty provider", CaptchaConfig{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCaptcha(tc.cfg); (err != nil) != tc.wantErr {
				t.Fatalf("validateCaptcha(%+v) = %v, wantErr %v", tc.cfg, err, tc.wantErr)
			}
		})
	}
}

// The API's body limit is derived from the attachment cap unless set explicitly, so raising
// ATTACHMENT_MAX_MB cannot leave uploads failing with a 413 at the HTTP layer.
func TestBodyLimitFollowsAttachmentCap(t *testing.T) {
	t.Setenv("NEBULA_ATTACHMENT_MAX_MB", "100")
	t.Setenv("HTTP_BODY_LIMIT_KB", "")
	t.Setenv("PORT", "4000")
	t.Setenv("TOTP_ENCRYPTION_KEY", "457ecc2865debaaa617c916d1b9183ababb84fd3d33f909908137422de0a3e2c")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := 100*1024 + 2048; cfg.HTTP.BodyLimitKB != want {
		t.Fatalf("BodyLimitKB = %d, want %d", cfg.HTTP.BodyLimitKB, want)
	}

	t.Setenv("HTTP_BODY_LIMIT_KB", "4096")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with explicit limit: %v", err)
	}
	if cfg.HTTP.BodyLimitKB != 4096 {
		t.Fatalf("explicit HTTP_BODY_LIMIT_KB was overridden: %d", cfg.HTTP.BodyLimitKB)
	}
}
