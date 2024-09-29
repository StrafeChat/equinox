package models

import "github.com/scylladb/gocqlx/v3/table"

var UserByUsernameAndDiscriminatorTable = table.Metadata{
	Name:    "users_by_username_and_discriminator",
	Columns: []string{"username", "discriminator", "id"},
	PartKey: []string{"username", "discriminator"},
}

type UserByUsernameAndDiscriminator struct {
	Username      string `json:"username" db:"username"`
	Discriminator string `json:"discriminator" db:"discriminator"`
	ID            string `json:"id" db:"id"`
}

func (u *UserByUsernameAndDiscriminator) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users_by_username_and_discriminator (
            username text,
            discriminator text,
            id text,
            PRIMARY KEY ((username, discriminator))
        );`,
	}
}
