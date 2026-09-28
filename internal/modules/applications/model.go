// Package applications is the developer platform: OAuth2 client applications, the bot
// account each may own, and the tokens both use. OAuth2 grant flows live in the sibling
// oauth package, which shares this package's repository.
package applications

import (
	"errors"
	"time"
)

const (
	MaxName        = 80
	MaxDescription = 400
	MaxRedirects   = 10
	MaxAppsPerUser = 25
)

var (
	ErrNotFound        = errors.New("application not found")
	ErrNotOwner        = errors.New("you do not own this application")
	ErrTooMany         = errors.New("you have reached the application limit")
	ErrInvalidName     = errors.New("name must be 1-80 characters")
	ErrInvalidRedirect = errors.New("a redirect uri must be an absolute http(s) url, at most 10 of them")
	ErrHasBot          = errors.New("this application already has a bot")
	ErrNoBot           = errors.New("this application has no bot")
	ErrInvalidField    = errors.New("invalid field")
)

// Application is an OAuth2 client owned by a user. Secret and bot token are never stored in
// the clear; only their sha-256 hashes are kept, and the raw values are returned once.
type Application struct {
	ID           int64     `db:"id" json:"id,string"`
	OwnerID      int64     `db:"owner_id" json:"owner_id,string"`
	Name         string    `db:"name" json:"name"`
	Description  string    `db:"description" json:"description"`
	Icon         string    `db:"icon" json:"icon"`
	SecretHash   string    `db:"secret_hash" json:"-"`
	BotUserID    int64     `db:"bot_user_id" json:"bot_user_id,omitempty,string"`
	RedirectURIs []string  `db:"redirect_uris" json:"redirect_uris"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

func (a *Application) HasBot() bool { return a.BotUserID != 0 }

// UpdateInput is a partial update; a nil field is left unchanged.
type UpdateInput struct {
	Name         *string   `json:"name"`
	Description  *string   `json:"description"`
	Icon         *string   `json:"icon"`
	RedirectURIs *[]string `json:"redirect_uris"`
}
