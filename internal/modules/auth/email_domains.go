package auth

import (
	_ "embed"
	"strings"
	"sync"
)

// disposableDomainsList is the bundled set of throwaway-mail providers (one domain per
// line, "#" comments). It is a curated list of the long-lived, widely known services, not
// an attempt at completeness: new ones appear weekly, and an operator adds what they see
// through EMAIL_BLOCKED_DOMAINS rather than waiting for a release.
//
//go:embed disposable_domains.txt
var disposableDomainsList string

var (
	disposableOnce sync.Once
	disposableSet  map[string]struct{}
)

func disposableDomains() map[string]struct{} {
	disposableOnce.Do(func() {
		disposableSet = map[string]struct{}{}
		for _, line := range strings.Split(disposableDomainsList, "\n") {
			d := strings.ToLower(strings.TrimSpace(line))
			if d == "" || strings.HasPrefix(d, "#") {
				continue
			}
			disposableSet[d] = struct{}{}
		}
	})
	return disposableSet
}

// emailDomain is the lowercased part after the last "@", or "" when there is none.
func emailDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

// domainBlocked reports whether domain, or any parent of it, is in set - so a listed
// "yopmail.com" also covers "mail.yopmail.com".
func domainBlocked(domain string, set map[string]struct{}) bool {
	for domain != "" {
		if _, ok := set[domain]; ok {
			return true
		}
		dot := strings.IndexByte(domain, '.')
		if dot < 0 {
			return false
		}
		domain = domain[dot+1:]
	}
	return false
}

// emailDomainRefused is the registration check: the bundled disposable list when the
// instance has it on, plus the operator's own blocklist always.
func (s *service) emailDomainRefused(email string) bool {
	domain := emailDomain(email)
	if domain == "" {
		return false
	}
	if s.cfg.Flags.BlockDisposableEmail && domainBlocked(domain, disposableDomains()) {
		return true
	}
	if len(s.cfg.Flags.BlockedEmailDomains) > 0 {
		own := make(map[string]struct{}, len(s.cfg.Flags.BlockedEmailDomains))
		for _, d := range s.cfg.Flags.BlockedEmailDomains {
			own[d] = struct{}{}
		}
		return domainBlocked(domain, own)
	}
	return false
}
