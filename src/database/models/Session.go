package models

import (
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

type Session struct {
	Token     string    `db:"session_token" json:"token"`
	UserId    string    `db:"user_id" json:"user_id"`
	IP        string    `db:"ip" json:"ip"`
	UserAgent string    `db:"user_agent" json:"user_agent"`
	Trusted   bool      `db:"trusted" json:"trusted"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"`
}

var sessionMeta = table.Metadata{
	Name:    "sessions",
	Columns: []string{"session_token", "user_id", "ip", "user_agent", "trusted", "created_at", "expires_at"},
	PartKey: []string{"session_token"},
}

var SessionTable = table.New(sessionMeta)

func (u *Session) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			session_token text PRIMARY KEY,
			user_id text,
			ip text,
			user_agent text,
			trusted boolean,
			created_at timestamp,
			expires_at timestamp
		);`,
		`CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);`,
	}
}
