package auth

import (
	netmail "net/mail"
	"strings"
	"testing"

	"github.com/StrafeChat/equinox/internal/config"
)

func TestNormalizeEmail(t *testing.T) {
	ok := map[string]string{
		" Jane@Example.COM ":     "jane@example.com",
		"a@b.co":                 "a@b.co",
		"first.last+tag@x.org":   "first.last+tag@x.org", // no plus or dot stripping
		"\tSam@Mail.Example.de ": "sam@mail.example.de",
	}
	for in, want := range ok {
		got, err := NormalizeEmail(in)
		if err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"x@yopmail.com.",     // trailing dot: also slipped past the disposable list
		`"a b"@example.com`,  // quoted local part zog accepts
		"a@b..com",           // empty label
		"Name <a@b.com>",     // a display name is not an address
		"a@@b.com",           //
		"",                   //
		"   ",                //
		"no-at-sign.example", //
		"@example.com",       //
		"a@",                 //
		strings.Repeat("a", 243) + "@example.com", // 255 characters
	}
	for _, in := range bad {
		if got, err := NormalizeEmail(in); err != ErrInvalidEmail {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want ErrInvalidEmail", in, got, err)
		}
	}
	if n := len(strings.Repeat("a", 243) + "@example.com"); n != 255 {
		t.Fatalf("length case is %d characters, want 255", n)
	}
}

// Where zog's email check and net/mail disagree about an address, NormalizeEmail must
// refuse it: an address passes only when both accept it.
func TestNormalizeEmailRefusesWhereValidatorsDisagree(t *testing.T) {
	candidates := []string{
		"x@yopmail.com.", `"a b"@example.com`, `"quoted"@example.com`, "a@b", "a@localhost",
		"a.@example.com", ".a@example.com", "a..b@example.com", "a@-example.com", "a@example-.com",
		"a@[127.0.0.1]", "a(comment)@example.com", "a@example.com (c)", "a b@example.com",
		"a@exa mple.com", "ü@example.com", "a@exämple.com", "a@example.c", "a@1.2.3.4",
		"a+b@example.com", "a_b@example.com", "a/b@example.com", "a'b@example.com",
	}
	disagreements := 0
	for _, in := range candidates {
		var out string
		zogOK := len(emailShape.Parse(in, &out)) == 0
		addr, err := netmail.ParseAddress(in)
		netOK := err == nil && addr.Address == in
		if zogOK == netOK {
			continue
		}
		disagreements++
		if got, err := NormalizeEmail(in); err == nil {
			t.Errorf("%q: zog=%v net/mail=%v, yet NormalizeEmail accepted it as %q", in, zogOK, netOK, got)
		}
	}
	if disagreements == 0 {
		t.Error("expected at least one form the two validators disagree on (the trailing dot, the quoted local part)")
	}
}

func TestEmailDomainDropsTrailingDot(t *testing.T) {
	if got := emailDomain("x@yopmail.com."); got != "yopmail.com" {
		t.Errorf("emailDomain = %q, want yopmail.com", got)
	}
}

func TestDisposableBlocklistOnNormalisedAddress(t *testing.T) {
	s := &service{cfg: &config.Config{}}
	s.cfg.Flags.BlockDisposableEmail = true
	email, err := NormalizeEmail(" Someone@YOPMAIL.com ")
	if err != nil {
		t.Fatal(err)
	}
	if !s.emailDomainRefused(email) {
		t.Errorf("%q should be refused by the disposable list", email)
	}
	if s.emailDomainRefused("someone@example.com") {
		t.Error("a permanent address must not be refused")
	}
	// Even if a trailing-dot spelling got this far, the domain check now sees through it.
	if !s.emailDomainRefused("someone@yopmail.com.") {
		t.Error("the trailing-dot spelling must be refused too")
	}
}

func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"jane.doe@gmail.com": "j***@g***.com",
		"a@b.co.uk":          "a***@b***.uk",
		"ö@mail.example.de":  "ö***@m***.de",
		"no-at-sign":         "***",
	}
	for in, want := range cases {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

// A relay resolves the recipient domain through IDNA's lookup mapping, so a spelling that the
// mapping folds onto another domain reaches that domain's inbox. Each of these is delivered to
// yopmail.com (or is a second spelling of one IDN) and must be refused, or it walks past the
// disposable list and claims an index key of its own for an inbox that already has one.
func TestNormalizeEmailRefusesNonCanonicalDomains(t *testing.T) {
	bad := []string{
		"x@ｙｏｐｍａｉｌ.com",       // fullwidth letters
		"x@ⓨopmail.com",       // an enclosed letter
		"x@yopmail.com​",      // a zero-width space, dropped by the mapping
		"x@yopmail.c​om",      //
		"x@yopmail.com­",      // a soft hyphen, likewise
		"x@yopmail.com。",      // an ideographic full stop reads as a trailing dot
		"x@yopmail.com.。",     //
		"x@xn--exmple-cua.de", // the A-label of exämple.de: one inbox, one spelling
	}
	for _, in := range bad {
		if got, err := NormalizeEmail(in); err != ErrInvalidEmail {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want ErrInvalidEmail", in, got, err)
		}
	}
	good := map[string]string{
		"ö@Mail.Example.de": "ö@mail.example.de", // a Unicode local part is the receiver's business
		"x@Exämple.de":      "x@exämple.de",      // an IDN in its Unicode form
		"x@ab--cd.example":  "x@ab--cd.example",  // hyphens in the third and fourth place are fine in ASCII
	}
	for in, want := range good {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// An operator may list an IDN in either spelling; the check matches both.
func TestBlockedDomainsMatchBothIDNSpellings(t *testing.T) {
	s := &service{cfg: &config.Config{}}
	s.cfg.Flags.BlockedEmailDomains = []string{"xn--exmple-cua.de"}
	if !s.emailDomainRefused("x@exämple.de") {
		t.Error("the Unicode spelling of a domain listed as an A-label must be refused")
	}
	s.cfg.Flags.BlockedEmailDomains = []string{"exämple.de"}
	if !s.emailDomainRefused("x@mail.exämple.de") {
		t.Error("a subdomain of a listed Unicode domain must be refused")
	}
	if s.emailDomainRefused("x@example.de") {
		t.Error("an unrelated domain must not be refused")
	}
}
