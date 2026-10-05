package auth

import (
	"testing"

	"github.com/StrafeChat/equinox/internal/config"
)

func TestDisposableDomainsParse(t *testing.T) {
	set := disposableDomains()
	if len(set) < 100 {
		t.Fatalf("bundled list looks truncated: %d domains", len(set))
	}
	for _, want := range []string{"mailinator.com", "yopmail.com", "guerrillamail.com", "temp-mail.org", "10minutemail.com"} {
		if _, ok := set[want]; !ok {
			t.Errorf("%s missing from the bundled list", want)
		}
	}
	if _, ok := set["# throwaway / disposable email providers refused at registration when"]; ok {
		t.Error("comment lines must not be parsed as domains")
	}
}

func TestEmailDomainRefused(t *testing.T) {
	svc := &service{cfg: &config.Config{}}
	svc.cfg.Flags.BlockDisposableEmail = true
	svc.cfg.Flags.BlockedEmailDomains = []string{"spammers.example"}

	cases := map[string]bool{
		"alice@example.com":             false,
		"bob@gmail.com":                 false,
		"eve@mailinator.com":            true,
		"eve@MAILINATOR.COM":            true,
		"eve@mail.yopmail.com":          true,  // subdomain of a listed domain
		"eve@notyopmail.com":            false, // suffix match must be on a label boundary
		"eve@spammers.example":          true,  // operator's own list
		"eve@deep.sub.spammers.example": true,
		"nodomain":                      false,
		"trailing@":                     false,
	}
	for email, want := range cases {
		if got := svc.emailDomainRefused(email); got != want {
			t.Errorf("%s: refused=%v, want %v", email, got, want)
		}
	}

	// Switching the bundled list off leaves only the operator's own domains.
	svc.cfg.Flags.BlockDisposableEmail = false
	if svc.emailDomainRefused("eve@mailinator.com") {
		t.Error("bundled list applied with EMAIL_BLOCK_DISPOSABLE off")
	}
	if !svc.emailDomainRefused("eve@spammers.example") {
		t.Error("operator blocklist must apply regardless of the bundled list")
	}
}
