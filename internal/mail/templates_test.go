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

// The verify link is not burned (scanners prefetch it), so its mail must not say it works
// once.
func TestVerificationWording(t *testing.T) {
	m, err := Verification(LinkEmail{InstanceName: "x.test", Username: "sam", Link: "https://x.test/verify-email?token=t", ExpiresIn: "24 hours"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{m.Text, m.HTML} {
		if strings.Contains(body, "works once") {
			t.Errorf("verify mail still says the link works once:\n%s", body)
		}
		if !strings.Contains(body, "The link expires in 24 hours.") {
			t.Errorf("verify mail lacks the reworded expiry sentence:\n%s", body)
		}
	}
}

func TestEmailChangeConfirmRenders(t *testing.T) {
	d := LinkEmail{
		InstanceName: "chat.example.com",
		Username:     `<script>alert(1)</script>`,
		Link:         "https://chat.example.com/confirm-email?token=abc&x=1",
		ExpiresIn:    "1 hour",
	}
	m, err := EmailChangeConfirm(d)
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Confirm your new email for chat.example.com" {
		t.Errorf("subject = %q", m.Subject)
	}
	if !strings.Contains(m.Text, d.Link) || !strings.Contains(m.Text, "The link expires in 1 hour.") {
		t.Errorf("text lacks the link or the expiry:\n%s", m.Text)
	}
	if !strings.Contains(m.Text, "Settings > Security") {
		t.Errorf("text lacks the app hint:\n%s", m.Text)
	}
	if strings.Contains(m.HTML, "<script>") {
		t.Error("html did not escape the username")
	}
	if !strings.Contains(m.HTML, `href="https://chat.example.com/confirm-email?token=abc&amp;x=1"`) {
		t.Errorf("html lacks the link as an href:\n%s", m.HTML)
	}
	if !strings.Contains(m.HTML, ">Confirm new email</a>") || !strings.Contains(m.HTML, "<title>Confirm your new email</title>") {
		t.Errorf("html lacks the button or the title")
	}
	if !strings.HasSuffix(m.Text, "\n") || !strings.HasSuffix(m.Text, "- chat.example.com\n") {
		t.Errorf("text must end with the signature and a newline: %q", m.Text[len(m.Text)-30:])
	}
}

// The notices go to an account's OLD address (or, for "taken", to a stranger's), so they
// carry only the masked new address - never a full one.
func TestEmailChangeNoticesRender(t *testing.T) {
	d := NoticeEmail{
		InstanceName: "chat.example.com",
		Username:     `<script>alert(1)</script>`,
		MaskedEmail:  "j***@g***.com",
		ExpiresIn:    "1 hour",
	}
	cases := []struct {
		name    string
		render  func(NoticeEmail) (Message, error)
		subject string
		title   string
		masked  bool
		want    []string
	}{
		{"taken", EmailChangeTaken, "Email change request on chat.example.com", "Email change request", false,
			[]string{"Hello,", "it already belongs to an account there, so nothing was changed", "you can ignore this email"}},
		{"requested", EmailChangeRequested, "Your chat.example.com email is being changed", "Your email is being changed", true,
			[]string{"asked to change its email address", "the request expires in 1 hour", "cancels the change"}},
		{"done", EmailChangeDone, "Your chat.example.com email was changed", "Your email was changed", true,
			[]string{"was changed to", "no longer used to sign in", "contact the administrator of chat.example.com"}},
	}
	for _, tc := range cases {
		m, err := tc.render(d)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if m.Subject != tc.subject {
			t.Errorf("%s: subject = %q", tc.name, m.Subject)
		}
		if !strings.Contains(m.HTML, "<title>"+tc.title+"</title>") {
			t.Errorf("%s: html title is not %q", tc.name, tc.title)
		}
		for _, body := range []string{m.Text, m.HTML} {
			if strings.Contains(body, "@gmail.com") {
				t.Errorf("%s: a full address leaked into the body", tc.name)
			}
			if tc.masked != strings.Contains(body, "j***@g***.com") {
				t.Errorf("%s: masked address present = %v, want %v", tc.name, !tc.masked, tc.masked)
			}
			if strings.Contains(body, "href=") {
				t.Errorf("%s: a notice carries no link", tc.name)
			}
		}
		for _, w := range tc.want {
			if !strings.Contains(m.Text, w) {
				t.Errorf("%s: text lacks %q:\n%s", tc.name, w, m.Text)
			}
		}
		if strings.Contains(m.HTML, "<script>") {
			t.Errorf("%s: html did not escape the username", tc.name)
		}
		if !strings.HasSuffix(m.Text, "- chat.example.com\n") {
			t.Errorf("%s: text must end with the signature and a newline", tc.name)
		}
	}
}
