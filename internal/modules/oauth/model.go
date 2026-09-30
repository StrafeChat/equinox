// Package oauth is the OAuth2 authorization server: the authorize (consent) and token
// endpoints of the authorization-code grant, plus refresh and revocation, and the `bot`
// scope that installs an application's bot into a space. It shares the applications
// package's client store (an application is the OAuth2 client) and keeps its own grant
// tables. Access tokens it issues are resolved by the auth middleware's OAuthResolver.
package oauth

import (
	"errors"
	"time"
)

// Scopes an application may request. Kept small and explicit; an unknown scope is rejected
// at authorize time so a token can never carry one nothing enforces. Each is enforced at
// the endpoint by middleware.RequireAuthScoped, or by the handler for a single field.
const (
	ScopeIdentify   = "identify"    // GET /users/@me without the email
	ScopeEmail      = "email"       // ...and the account's email
	ScopeSpaces     = "spaces"      // GET /users/@me/spaces: the spaces the account is in
	ScopeSpacesJoin = "spaces.join" // a bot may add the account to a space (PUT /spaces/:id/members/:user_id)
	ScopeBot        = "bot"         // install the application's bot into a space the user manages
)

// AllScopes is every scope, in the order the URL generator lists them.
var AllScopes = []string{ScopeIdentify, ScopeEmail, ScopeSpaces, ScopeSpacesJoin, ScopeBot}

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
	// Bot installs.
	ErrNoBot       = errors.New("this application has no bot")
	ErrBotPrivate  = errors.New("this bot is private: only the application's owner can add it")
	ErrNoSpace     = errors.New("choose a space to add the bot to")
	ErrInvalidBody = errors.New("invalid_request")
)

func knownScope(s string) bool {
	for _, k := range AllScopes {
		if k == s {
			return true
		}
	}
	return false
}

// Grant is what a user has authorised for one application, for the "authorized apps" list.
type Grant struct {
	ApplicationID int64     `db:"application_id" json:"application_id,string"`
	Scopes        []string  `db:"scopes" json:"scopes"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
}
