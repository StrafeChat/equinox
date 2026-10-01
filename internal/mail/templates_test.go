package mail

import (
	"strings"
	"testing"
)

func TestVerificationRendersLinkAndEscapes(t *testing.T) {
	d := LinkEmail{
		InstanceName: "chat.example.com",
		Username:     `<script>alert(1)</script>`,
		Link:         "https://chat.example.com/verify-email?token=abc&x=1",
		ExpiresIn:    "24 hours",
	}
	m, err := Verification(d)
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Verify your email for chat.example.com" {
		t.Errorf("subject = %q", m.Subject)
	}
	if !strings.Contains(m.Text, d.Link) {
		t.Errorf("text body lacks the link:\n%s", m.Text)
	}
	if !strings.Contains(m.Text, "24 hours") {
		t.Errorf("text body lacks the expiry")
	}
	// html/template must neutralise the display name and keep the URL intact (& in a
	// query string is escaped as &amp; inside an attribute, which browsers decode).
	if strings.Contains(m.HTML, "<script>") {
		t.Errorf("html body did not escape the username")
	}
	if !strings.Contains(m.HTML, `href="https://chat.example.com/verify-email?token=abc&amp;x=1"`) {
		t.Errorf("html body lacks the link as an href:\n%s", m.HTML)
	}
	// The text part is what every client can show; it must never be empty or end without
	// a newline (some relays append a trailing dot-stuffing line otherwise).
	if !strings.HasSuffix(m.Text, "\n") {
		t.Errorf("text body should end with a newline")
	}
}

func TestPasswordResetRenders(t *testing.T) {
	m, err := PasswordReset(LinkEmail{InstanceName: "x.test", Username: "sam", Link: "https://x.test/reset-password?token=t", ExpiresIn: "1 hour"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Reset your x.test password" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, want := range []string{"sam", "https://x.test/reset-password?token=t", "1 hour", "your password stays as it is"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q", want)
		}
		if !strings.Contains(m.HTML, want) {
			t.Errorf("html lacks %q", want)
		}
	}
}

func TestNewSMTPNeedsEnabledAndDomain(t *testing.T) {
	if _, err := NewSMTP(configDisabled()); err == nil {
		t.Error("disabled config must be refused")
	}
	cfg := configDisabled()
	cfg.Enabled, cfg.Host, cfg.From = true, "mail", "noreply"
	if _, err := NewSMTP(cfg); err == nil {
		t.Error("a From without a domain must be refused")
	}
	cfg.From = "noreply@chat.example.com"
	s, err := NewSMTP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.fromDomain != "chat.example.com" {
		t.Errorf("fromDomain = %q", s.fromDomain)
	}
}
