package config

import (
	"strings"
	"testing"
)

// Nothing set: mail is off and that is not an error.
func TestMailDisabledByDefault(t *testing.T) {
	m := loadMailConfig("https://chat.example.com", "chat.example.com")
	if m.Enabled {
		t.Fatal("mail must be off without SMTP_HOST")
	}
	if err := validateMail(m); err != nil {
		t.Errorf("disabled mail must validate: %v", err)
	}
}

// EMAIL_VERIFICATION without a way to send is refused at boot, not at the first signup.
func TestMailVerificationNeedsHost(t *testing.T) {
	t.Setenv("EMAIL_VERIFICATION", "true")
	m := loadMailConfig("https://chat.example.com", "chat.example.com")
	err := validateMail(m)
	if err == nil || !strings.Contains(err.Error(), "SMTP_HOST") {
		t.Errorf("want an SMTP_HOST error, got %v", err)
	}
}

// Only the host set: every other value falls back to something that works for the compose
// deployment - submission port, STARTTLS, a noreply@ sender under the instance's own
// domain, and the domain as the instance's display name.
func TestMailDefaults(t *testing.T) {
	t.Setenv("SMTP_HOST", "mail")
	m := loadMailConfig("https://chat.example.com/", "chat.example.com")
	if !m.Enabled {
		t.Fatal("SMTP_HOST set: mail must be on")
	}
	if m.Port != 587 || m.TLS != "starttls" {
		t.Errorf("defaults: port=%d tls=%q, want 587/starttls", m.Port, m.TLS)
	}
	if m.From != "noreply@chat.example.com" || m.FromName != "StrafeChat" {
		t.Errorf("sender: %q <%s>", m.FromName, m.From)
	}
	if m.InstanceName != "chat.example.com" {
		t.Errorf("instance name = %q", m.InstanceName)
	}
	if m.LinkBase != "https://chat.example.com" {
		t.Errorf("link base must drop the trailing slash, got %q", m.LinkBase)
	}
	if err := validateMail(m); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

// Without a federation domain the sender and name come from the web URL's host; implicit
// TLS picks 465 unless a port is given.
func TestMailDerivesFromWebURL(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.provider.example")
	t.Setenv("SMTP_TLS", "TLS")
	m := loadMailConfig("https://app.example.org", "")
	if m.From != "noreply@app.example.org" || m.InstanceName != "app.example.org" {
		t.Errorf("from=%q name=%q", m.From, m.InstanceName)
	}
	if m.Port != 465 || m.TLS != "tls" {
		t.Errorf("implicit tls: port=%d tls=%q", m.Port, m.TLS)
	}
}

func TestMailValidation(t *testing.T) {
	base := func() MailConfig {
		return MailConfig{Enabled: true, Host: "mail", Port: 587, TLS: "starttls", From: "noreply@chat.example.com", FromName: "StrafeChat", LinkBase: "https://chat.example.com"}
	}
	cases := []struct {
		name string
		mut  func(*MailConfig)
		want string
	}{
		{"bad tls", func(m *MailConfig) { m.TLS = "ssl" }, "SMTP_TLS"},
		{"bad port", func(m *MailConfig) { m.Port = 70000 }, "SMTP_PORT"},
		{"username alone", func(m *MailConfig) { m.Username = "u" }, "SMTP_USERNAME"},
		{"password alone", func(m *MailConfig) { m.Password = "p" }, "SMTP_USERNAME"},
		{"no from", func(m *MailConfig) { m.From = "" }, "MAIL_FROM"},
		{"from with display name", func(m *MailConfig) { m.From = "Strafe <noreply@chat.example.com>" }, "MAIL_FROM"},
		{"from not an address", func(m *MailConfig) { m.From = "noreply" }, "MAIL_FROM"},
		{"no web url", func(m *MailConfig) { m.LinkBase = "" }, "WEB_URL"},
	}
	for _, tc := range cases {
		m := base()
		tc.mut(&m)
		err := validateMail(m)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error mentioning %s, got %v", tc.name, tc.want, err)
		}
	}
	m := base()
	m.Username, m.Password = "u", "p"
	if err := validateMail(m); err != nil {
		t.Errorf("a complete config must validate: %v", err)
	}
}
