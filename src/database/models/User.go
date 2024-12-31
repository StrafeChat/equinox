package models

import (
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

type UserFlags uint32

const (
	FOUNDER UserFlags = 1 << iota
	PLATFORM_ADMIN
	PLATFORM_MOD
	CONTRIBUTOR
	TRANSLATOR
	BUG_REPORTER
	EARLY_SUPPORTER
	SUPPORTER
	EARLY_ADOPTER
	BOT_DEVELOPER
)

type UserPresence struct {
	Online       bool    `db:"online" json:"online"`
	Status       string  `db:"status" json:"status"`
	CustomStatus *string `db:"custom_status" json:"custom_status"`
}

func (up UserPresence) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"online":        up.Online,
		"status":        up.Status,
		"custom_status": up.CustomStatus,
	}
}

type User struct {
	ID            string                 `db:"id" json:"id"`
	Email         *string                `db:"email" json:"email"`
	Password      *string                `db:"password" json:"password"`
	Username      string                 `db:"username" json:"username"`
	Discriminator int                    `db:"discriminator" json:"discriminator"`
	DisplayName   *string                `db:"display_name" json:"display_name"`
	Avatar        *string                `db:"avatar" json:"avatar"`
	Banner        *string                `db:"banner" json:"banner"`
	Bot           bool                   `db:"bot" json:"bot"`
	Bots          []string               `db:"bots" json:"bots"`
	System        bool                   `db:"system" json:"system"`
	Bio           *string                `db:"bio" json:"bio"`
	Flags         UserFlags              `db:"flags" json:"flags"`
	Relationships []string               `db:"relationships" json:"relationships"`
	DateOfBirth   time.Time              `db:"date_of_birth" json:"date_of_birth"`
	VerifiedEmail bool                   `db:"verified_email" json:"verified_email"`
	AboutMe       *string                `db:"about_me" json:"about_me"`
	AccentColor   *string                `db:"accent_color" json:"accent_color"`
	Locale        *string                `db:"locale" json:"locale"`
	Presence      map[string]interface{} `db:"presence" json:"presence"`
	CreatedAt     time.Time              `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time              `db:"updated_at" json:"updated_at"`
}

var userMetadata = table.Metadata{
	Name:    "users",
	Columns: []string{"id", "email", "password", "username", "discriminator", "display_name", "avatar", "banner", "bot", "system", "bio", "flags", "date_of_birth", "verified_email", "about_me", "accent_color", "locale", "presence", "created_at", "updated_at"},
	PartKey: []string{"id"},
}

var UserTable = table.New(userMetadata)

func (u *User) SchemaDefinition() []string {
	return []string{
		`CREATE TYPE IF NOT EXISTS user_presence (
		    online boolean,
            status text,
            custom_status text
        );`,
		`CREATE TABLE IF NOT EXISTS users (
            id bigint PRIMARY KEY,
            email text,
            password text,
            username text,
            discriminator int,
            display_name text,
            avatar text,
            banner text,
            bot boolean,
			bots set<text>,
            system boolean,
            bio text,
            flags int,
			relationships set<bigint>,
            date_of_birth timestamp,
			verified_email boolean,
            about_me text,
            accent_color text,
            locale text,
            presence frozen<user_presence>,
            created_at timestamp,
            updated_at timestamp
        );`,
	}
}
