package spaces

import "testing"

func TestParseInviteCode(t *testing.T) {
	cases := []struct {
		in           string
		code, domain string
		ok           bool
	}{
		{"abcDEF12", "abcDEF12", "", true},
		{"  abcDEF12 ", "abcDEF12", "", true},
		{"abcDEF12@chat.example.com", "abcDEF12", "chat.example.com", true},
		{"abcDEF12@Chat.Example.COM", "abcDEF12", "chat.example.com", true},
		{"abcDEF12@b.local:4100", "abcDEF12", "b.local:4100", true},
		{"", "", "", false},
		{"@chat.example.com", "", "", false},
		{"abc def@chat.example.com", "", "", false},
		{"abcDEF12@", "", "", false},
		{"abcDEF12@bad domain", "", "", false},
		{"../etc@chat.example.com", "", "", false},
	}
	for _, tc := range cases {
		code, domain, err := ParseInviteCode(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("%q: ok=%v, want %v (err %v)", tc.in, err == nil, tc.ok, err)
			continue
		}
		if code != tc.code || domain != tc.domain {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", tc.in, code, domain, tc.code, tc.domain)
		}
	}
	if got := FormatInviteCode("abc", "x.org"); got != "abc@x.org" {
		t.Errorf("FormatInviteCode: %q", got)
	}
	if got := FormatInviteCode("abc", ""); got != "abc" {
		t.Errorf("FormatInviteCode local: %q", got)
	}
}
