// Package oauth is the OAuth2 authorization server: the authorize (consent) and token
// endpoints of the authorization-code grant, plus refresh. It shares the applications
// package's client store (an application is the OAuth2 client) and keeps its own grant
// tables. Access tokens it issues are resolved by the auth middleware's OAuthResolver.
package oauth

import (
	"errors"
	"time"
)

// Scopes an application may request. Kept small and explicit; an unknown scope is rejected
// at authorize time so a token can never carry one nothing enforces.
const (
	ScopeIdentify = "identify" // basic account: id, username, avatar
	ScopeEmail    = "email"    // the account's email
	ScopeGuilds   = "guilds"   // the spaces the account is in
)

const (
	CodeTTL    = 10 * time.Minute
	AccessTTL  = 7 * 24 * time.Hour
	MaxScopes  = 10
	TokenBytes = 32
)

var (
	ErrInvalidClient   = errors.New("invalid_client")
	ErrInvalidGrant    = errors.New("invalid_grant")
	ErrInvalidScope    = errors.New("invalid_scope")
	ErrInvalidRedirect = errors.New("invalid redirect_uri")
	ErrUnsupported     = errors.New("unsupported_grant_type")
)

func knownScope(s string) bool {
	switch s {
	case ScopeIdentify, ScopeEmail, ScopeGuilds:
		return true
	}
	return false
}

// Grant is what a user has authorised for one application, for the "authorized apps" list.
type Grant struct {
	ApplicationID int64     `db:"application_id" json:"application_id,string"`
	Scopes        []string  `db:"scopes" json:"scopes"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
}
