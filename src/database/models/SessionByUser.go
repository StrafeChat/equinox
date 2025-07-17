package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

type SessionByUser struct {
	UserId    string    `db:"user_id" json:"user_id"`
	Token     string    `db:"session_token" json:"token"`
	IP        string    `db:"ip" json:"ip"`
	UserAgent string    `db:"user_agent" json:"user_agent"`
	Trusted   bool      `db:"trusted" json:"trusted"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"`
}

var sessionByUserMeta = table.Metadata{
	Name:    "sessions_by_user",
	Columns: []string{"user_id", "session_token", "ip", "user_agent", "trusted", "created_at", "expires_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"session_token"},
}

var SessionByUserTable = table.New(sessionByUserMeta)

func (u *SessionByUser) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS sessions_by_user (
			user_id text,
			session_token text,
			ip text,
			user_agent text,
			trusted boolean,
			created_at timestamp,
			expires_at timestamp,
			PRIMARY KEY (user_id, session_token)
		);`,
	}
}
