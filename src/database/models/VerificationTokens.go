package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

type VerificationToken struct {
	Token     string    `db:"verification_token" json:"token"`
	UserID    string    `db:"user_id" json:"user_id"`
	Email     string    `db:"email" json:"email"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"`
}

var verificationTokenMetadata = table.Metadata{
	Name:    "verification_tokens",
	Columns: []string{"verification_token", "user_id", "email", "created_at", "expires_at"},
	PartKey: []string{"verification_token"},
}

var VerificationTokenTable = table.New(verificationTokenMetadata)

func (vt *VerificationToken) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS verification_tokens (
            verification_token text PRIMARY KEY,
            user_id bigint,
            email text,
            created_at timestamp,
            expires_at timestamp
        );`,
	}
}
