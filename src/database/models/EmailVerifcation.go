package models

import "github.com/scylladb/gocqlx/v3/table"

var emailVerifcationTableMeta = table.Metadata{
	Name:    "email_verifcations",
	Columns: []string{"user_id", "code"},
	PartKey: []string{"user_id"},
}

var EmailVerifcationTable = table.New(emailVerifcationTableMeta)

type EmailVerifcation struct {
	UserId string `db:"user_id" json:"user_id"`
	Code   string `db:"code" json:"code"`
}

func (u *EmailVerifcation) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS email_verifcations (
			user_id bigint PRIMARY KEY,
            code text,
		);`,
	}
}
