package types

import "github.com/gocql/gocql"

type User struct {
	ID    gocql.UUID `json:"id"`
	Name  string     `json:"name"`
	Email string     `json:"email"`
}
