package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

var spaceRoleMeta = table.Metadata{
	Name:    "space_roles",
	Columns: []string{"space_id", "role_id", "name", "color", "permissions", "position", "mentionable", "hoist", "created_at", "updated_at"},
	PartKey: []string{"space_id", "role_id"},
}

var SpaceRoleTable = table.New(spaceRoleMeta)

type SpaceRole struct {
	SpaceID     int64     `db:"space_id" json:"space_id"`
	RoleID      string    `db:"role_id" json:"role_id"`
	Name        string    `db:"name" json:"name"`
	Color       *string   `db:"color" json:"color"`
	Permissions []string  `db:"permissions" json:"permissions"`
	Position    int       `db:"position" json:"position"`
	Mentionable bool      `db:"mentionable" json:"mentionable"`
	Hoist       bool      `db:"hoist" json:"hoist"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

func (sr *SpaceRole) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS space_roles (
			space_id bigint,
			role_id text,
			name text,
			color text,
			permissions list<text>,
			position int,
			mentionable boolean,
			hoist boolean,
			created_at timestamp,
			updated_at timestamp,
			PRIMARY KEY (space_id, role_id)
		);`,
	}
}

// GetTableMeta returns the table metadata for space roles
func (sr *SpaceRole) GetTableMeta() table.Metadata {
	return spaceRoleMeta
}
