package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

var spaceMemberRoleMeta = table.Metadata{
	Name:    "space_member_roles",
	Columns: []string{"space_id", "user_id", "role_id", "assigned_by", "assigned_at"},
	PartKey: []string{"space_id", "user_id"},
	SortKey: []string{"role_id"},
}

var SpaceMemberRoleTable = table.New(spaceMemberRoleMeta)

type SpaceMemberRole struct {
	SpaceID    int64     `db:"space_id" json:"space_id"`
	UserID     string    `db:"user_id" json:"user_id"`
	RoleID     string    `db:"role_id" json:"role_id"`
	AssignedAt time.Time `db:"assigned_at" json:"assigned_at"`
	AssignedBy string    `db:"assigned_by" json:"assigned_by"`
}

func (smr *SpaceMemberRole) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS space_member_roles (
			space_id bigint,
			user_id text,
			role_id text,
			assigned_by text,
			assigned_at timestamp,
			PRIMARY KEY ((space_id, user_id), role_id)
		);`,
	}
}

// SpaceMemberRolesByRole table for efficient role-based queries (e.g., when deleting a role)
var spaceMemberRolesByRoleMeta = table.Metadata{
	Name:    "space_member_roles_by_role",
	Columns: []string{"space_id", "role_id", "user_id", "assigned_by", "assigned_at"},
	PartKey: []string{"space_id", "role_id"},
	SortKey: []string{"user_id"},
}

var SpaceMemberRolesByRoleTable = table.New(spaceMemberRolesByRoleMeta)

type SpaceMemberRolesByRole struct {
	SpaceID    int64     `db:"space_id" json:"space_id"`
	RoleID     string    `db:"role_id" json:"role_id"`
	UserID     string    `db:"user_id" json:"user_id"`
	AssignedAt time.Time `db:"assigned_at" json:"assigned_at"`
	AssignedBy string    `db:"assigned_by" json:"assigned_by"`
}

func (smrbr *SpaceMemberRolesByRole) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS space_member_roles_by_role (
			space_id bigint,
			role_id text,
			user_id text,
			assigned_by text,
			assigned_at timestamp,
			PRIMARY KEY ((space_id, role_id), user_id)
		);`,
	}
}

func (smr *SpaceMemberRole) GetTableMeta() table.Metadata {
	return spaceMemberRoleMeta
}

func (smrbr *SpaceMemberRolesByRole) GetTableMeta() table.Metadata {
	return spaceMemberRolesByRoleMeta
}
