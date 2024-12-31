package models

import "github.com/scylladb/gocqlx/v3/table"

var passwordResetMeta = table.Metadata{
	Name:    "password_resets",
	Columns: []string{"user_id", "code", "ip", "user_agent"},
	PartKey: []string{"user_id"},
}

var PasswordResetTable = table.New(passwordResetMeta)

type PasswordReset struct {
	UserId    string `db:"user_id" json:"user_id"`
	Code      string `db:"code" json:"code"`
	Ip        string `db:"ip" json:"ip"`
	UserAgent string `db:"user_agent" json:"user_agent"`
}

func (u *PasswordReset) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS password_resets (
			user_id text PRIMARY KEY,
            code text,
			ip text, 
			user_agent text
		);`,
	}
}
