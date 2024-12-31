package models

import "github.com/scylladb/gocqlx/v3/table"

var userByUsernameAndDiscriminatorMeta = table.Metadata{
	Name:    "users_by_username_and_discriminator",
	Columns: []string{"username", "discriminator", "id"},
	PartKey: []string{"username", "discriminator"},
}

var UserByUsernameAndDiscriminatorTable = table.New(userByUsernameAndDiscriminatorMeta)

type UserByUsernameAndDiscriminator struct {
	Username      string `json:"username" db:"username"`
	Discriminator int    `json:"discriminator" db:"discriminator"`
	ID            string `json:"id" db:"id"`
}

func (u *UserByUsernameAndDiscriminator) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users_by_username_and_discriminator (
            username text,
            discriminator int,
            id bigint,
            PRIMARY KEY ((username, discriminator))
        );`,
	}
}
