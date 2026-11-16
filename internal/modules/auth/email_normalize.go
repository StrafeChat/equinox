package auth

import (
	"errors"
	netmail "net/mail"
	"strings"
	"unicode/utf8"

	z "github.com/Oudwins/zog"
	"golang.org/x/net/idna"
)

// ErrInvalidEmail is the one answer for an address NormalizeEmail refuses, wherever it is
// asked: registration, the self-service change and the administrator's tool.
var ErrInvalidEmail = errors.New("enter a valid email address")

// maxEmailLength is RFC 5321's limit on a forward path, the most any relay will carry.
const maxEmailLength = 254

// emailShape is the same check registration's zog schema applies (schema.go).
var emailShape = z.String().Email()

// NormalizeEmail is the one validator for an address that is about to be claimed in
// users_by_email. It trims and lowercases - nothing more: no Unicode folding and no plus or
// dot stripping, because the index key has to keep matching what Login and the password
// reset compute (ToLower(TrimSpace)) - and then demands that zog's email check AND
// net/mail's parser both accept the result as a bare address. The two disagree on forms like
// `"a b"@x.com` or a trailing-dot domain (`x@yopmail.com.`, which also slipped past the
// disposable-domain blocklist); requiring both closes those.
func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > maxEmailLength || !utf8.ValidString(email) {
		return "", ErrInvalidEmail
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return "", ErrInvalidEmail
	}
	domain := email[at+1:]
	if strings.HasSuffix(email, ".") || strings.Contains(domain, "..") {
		return "", ErrInvalidEmail
	}
	var shaped string
	if issues := emailShape.Parse(email, &shaped); len(issues) > 0 {
		return "", ErrInvalidEmail
	}
	addr, err := netmail.ParseAddress(email)
	if err != nil || addr.Address != email || addr.Name != "" {
		return "", ErrInvalidEmail
	}
	if !canonicalDomain(domain) {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// canonicalDomain reports whether domain is already the one spelling mail would be delivered
// to. Relays resolve a recipient domain through IDNA's lookup mapping (UTS #46), which folds
// fullwidth and enclosed letters, drops zero-width and soft-hyphen characters, and reads an
// ideographic full stop as a dot - so "ｙｏｐｍａｉｌ.com", "yopmail.com​" and
// "yopmail.com。" all reach yopmail.com while matching nothing in the blocklist and claiming
// an index key of their own. Only a domain that the mapping leaves exactly as it is passes:
// plain ASCII, or an IDN in its Unicode form (its "xn--" spelling is refused, so one mailbox
// has one address). Folding instead of refusing would break the rule that the index key is
// what Login computes from what the person types.
func canonicalDomain(domain string) bool {
	plain := true
	for i := 0; i < len(domain); i++ {
		if domain[i] >= utf8.RuneSelf {
			plain = false
			break
		}
	}
	if plain {
		// Lowercased LDH (zog and net/mail saw to that) is left alone by the mapping. Only an
		// A-label is a second spelling; the IDNA round trip is not run on the rest because
		// its hyphen rule would refuse real hosts like "ab--cd.example".
		for _, label := range strings.Split(domain, ".") {
			if strings.HasPrefix(label, "xn--") {
				return false
			}
		}
		return true
	}
	ascii, err := idna.Lookup.ToASCII(domain)
	if err != nil || ascii == "" || strings.HasSuffix(ascii, ".") || strings.Contains(ascii, "..") {
		return false
	}
	back, err := idna.Lookup.ToUnicode(ascii)
	return err == nil && back == domain
}

// asciiDomain is domain's IDNA A-label spelling ("xn--..."), or domain itself when it has
// none - for matching blocklists an operator may have written either way.
func asciiDomain(domain string) string {
	if a, err := idna.Lookup.ToASCII(domain); err == nil && a != "" {
		return a
	}
	return domain
}

// MaskEmail shows enough of an address for its owner to recognise it and no more:
// "jane.doe@gmail.com" becomes "j***@g***.com". It is for mail bodies only (the notice to
// an account's old address names the new one this way); never log even a masked address.
func MaskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return "***"
	}
	local, domain := email[:at], email[at+1:]
	out := firstRune(local) + "***@"
	labels := strings.Split(domain, ".")
	out += firstRune(labels[0]) + "***"
	if len(labels) > 1 {
		out += "." + labels[len(labels)-1]
	}
	return out
}

func firstRune(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || r == utf8.RuneError {
		return ""
	}
	return string(r)
}
