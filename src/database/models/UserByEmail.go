package models

import "github.com/scylladb/gocqlx/v3/table"

var userByEmailTableMetadata = table.Metadata{
	Name:    "users_by_email",
	Columns: []string{"email", "id"},
	PartKey: []string{"email"},
}

var UserByEmailTable = table.New(userByEmailTableMetadata)

type UserByEmail struct {
	Email string `json:"email" db:"email"`
	ID    string `json:"id" db:"id"`
}

func (u *UserByEmail) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users_by_email (
            email text PRIMARY KEY,
            id bigint
        );`,
	}
}
