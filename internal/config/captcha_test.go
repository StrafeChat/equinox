package config

import "testing"

func baseValidCfg() *Config {
	cfg := &Config{}
	cfg.HTTP.Port = "4000"
	cfg.App.SnowflakeNode = 0
	return cfg
}

// Off is the default, and an instance with no captcha keys must start normally.
func TestValidateAllowsCaptchaDisabled(t *testing.T) {
	cfg := baseValidCfg()
	cfg.Flags.Captcha = false
	cfg.Captcha.Provider = "turnstile"
	if err := validate(cfg); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

// The important one: enabling the flag without keys must be a startup failure, not a
// silently permissive instance.
func TestValidateRejectsEnabledCaptchaWithoutKeys(t *testing.T) {
	for _, tc := range []struct {
		name   string
		site   string
		secret string
	}{
		{"no keys", "", ""},
		{"site key only", "site", ""},
		{"secret only", "", "secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidCfg()
			cfg.Flags.Captcha = true
			cfg.Captcha.Provider = "turnstile"
			cfg.Captcha.SiteKey = tc.site
			cfg.Captcha.SecretKey = tc.secret
			if err := validate(cfg); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestValidateAcceptsFullyConfiguredCaptcha(t *testing.T) {
	cfg := baseValidCfg()
	cfg.Flags.Captcha = true
	cfg.Captcha.Provider = "turnstile"
	cfg.Captcha.SiteKey = "site"
	cfg.Captcha.SecretKey = "secret"
	if err := validate(cfg); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

func TestValidateRejectsUnknownProvider(t *testing.T) {
	cfg := baseValidCfg()
	cfg.Flags.Captcha = true
	cfg.Captcha.Provider = "hcaptcha"
	cfg.Captcha.SiteKey = "site"
	cfg.Captcha.SecretKey = "secret"
	if err := validate(cfg); err == nil {
		t.Fatal("want an error for an unsupported provider, got nil")
	}
}
