package messages

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateContent(t *testing.T) {
	if err := validateContent(strings.Repeat("é", MaxPlaintextRunes), ""); err != nil {
		t.Errorf("plaintext at the limit (counted in runes) rejected: %v", err)
	}
	if err := validateContent(strings.Repeat("a", MaxPlaintextRunes+1), ""); !errors.Is(err, ErrContentTooLong) {
		t.Errorf("plaintext over the limit accepted: %v", err)
	}
	if err := validateContent("", strings.Repeat("A", MaxCiphertextBytes+1)); !errors.Is(err, ErrContentTooLong) {
		t.Errorf("ciphertext over the limit accepted: %v", err)
	}
}

func TestSlowmodeErrorIs(t *testing.T) {
	var err error = &SlowmodeError{}
	if !errors.Is(err, ErrSlowmode) {
		t.Error("SlowmodeError should match ErrSlowmode")
	}
	var se *SlowmodeError
	if !errors.As(err, &se) {
		t.Error("errors.As should recover the *SlowmodeError")
	}
}

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":  "passwd",
		`C:\Users\x\r.png`:  "r.png",
		"my photo (1).jpeg": "my_photo_1_.jpeg",
		"..":                "file",
		"":                  "file",
	}
	for in, want := range cases {
		if got := safeFilename(in); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMentions(t *testing.T) {
	users, roles, everyone := parseMentions("hi <@1> and <@!2> <@&3> mail bob@everyone.works @here")
	if len(users) != 2 || users[0] != 1 || users[1] != 2 {
		t.Errorf("users = %v", users)
	}
	if len(roles) != 1 || roles[0] != 3 {
		t.Errorf("roles = %v", roles)
	}
	if !everyone {
		t.Error("@here should count as everyone")
	}
	if _, _, ev := parseMentions("bob@everyone.works"); ev {
		t.Error("email-like text must not trigger @everyone")
	}
}
