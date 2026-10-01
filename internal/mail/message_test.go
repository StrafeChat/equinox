package mail

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/StrafeChat/equinox/internal/config"
)

// Gmail rejects a message whose Message-ID is not a valid RFC 5322 msg-id with
// "550 5.7.1 ... missing a valid Message-ID header". go-mail's SetMessageIDWithValue adds
// the angle brackets itself, so the value handed to it must not already carry them, or the
// header comes out "<<id@domain>>" and every message to Gmail bounces.
func TestBuildMessageID(t *testing.T) {
	s, err := NewSMTP(config.MailConfig{
		Enabled: true, Host: "mail", Port: 587, TLS: "none",
		From: "noreply@strafe.chat", FromName: "StrafeChat",
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.build(Message{To: "someone@gmail.com", Subject: "Verify your email", Text: "hi\n", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := m.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	header := buf.String()
	if i := bytes.Index(buf.Bytes(), []byte("\r\n\r\n")); i >= 0 {
		header = header[:i]
	}

	// Exactly one Message-ID, bracketed once, no nesting, under the sender's domain.
	midRe := regexp.MustCompile(`(?mi)^Message-ID:\s*(.+)$`)
	found := midRe.FindAllStringSubmatch(header, -1)
	if len(found) != 1 {
		t.Fatalf("want exactly one Message-ID header, got %d:\n%s", len(found), header)
	}
	mid := strings.TrimSpace(found[0][1])
	if !regexp.MustCompile(`^<[^<>@\s]+@strafe\.chat>$`).MatchString(mid) {
		t.Errorf("Message-ID is not a valid single-bracketed msg-id under the domain: %q", mid)
	}

	// A From and a Date are the other two headers receivers now expect; go-mail adds Date.
	for _, h := range []string{"From:", "Date:"} {
		if !regexp.MustCompile(`(?mi)^` + h).MatchString(header) {
			t.Errorf("missing %s header:\n%s", h, header)
		}
	}
}

// A message missing a required field is refused before it reaches the relay.
func TestBuildRejectsIncomplete(t *testing.T) {
	s, err := NewSMTP(config.MailConfig{Enabled: true, Host: "mail", Port: 587, TLS: "none", From: "noreply@strafe.chat", FromName: "StrafeChat"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []Message{
		{Subject: "s", Text: "t"},
		{To: "a@b.c", Text: "t"},
		{To: "a@b.c", Subject: "s"},
	} {
		if _, err := s.build(m); err == nil {
			t.Errorf("build(%+v) = nil error, want refusal", m)
		}
	}
}
