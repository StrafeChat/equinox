package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

var passwordResetMeta = table.Metadata{
	Name:    "password_resets",
	Columns: []string{"code", "user_id", "ip", "user_agent", "created_at", "expires_at"},
	PartKey: []string{"code"},
}

var PasswordResetTable = table.New(passwordResetMeta)

// Define a secondary view for querying by user_id
var passwordResetByUserMeta = table.Metadata{
	Name:    "password_resets_by_user",
	Columns: []string{"user_id", "code", "created_at", "expires_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"created_at"},
}

var PasswordResetByUserTable = table.New(passwordResetByUserMeta)

// Define a secondary view for querying by expiration
var passwordResetByExpirationMeta = table.Metadata{
	Name:    "password_resets_by_expiration",
	Columns: []string{"expires_at", "code", "user_id"},
	PartKey: []string{"expires_at"},
}

var PasswordResetByExpirationTable = table.New(passwordResetByExpirationMeta)

type PasswordReset struct {
	UserId    string    `db:"user_id" json:"user_id"`
	Code      string    `db:"code" json:"code"`
	Ip        string    `db:"ip" json:"ip"`
	UserAgent string    `db:"user_agent" json:"user_agent"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"`
}

func (u *PasswordReset) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS password_resets (
			code text PRIMARY KEY,
			user_id text,
			ip text, 
			user_agent text,
			created_at timestamp,
			expires_at timestamp
		);`,
		`CREATE TABLE IF NOT EXISTS password_resets_by_user (
			user_id text,
			code text,
			created_at timestamp,
			expires_at timestamp,
			PRIMARY KEY (user_id, created_at)
		);`,
		`CREATE TABLE IF NOT EXISTS password_resets_by_expiration (
			expires_at timestamp,
			code text,
			user_id text,
			PRIMARY KEY (expires_at, code)
		);`,
	}
}
