package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

var spaceInviteMeta = table.Metadata{
	Name:    "space_invites",
	Columns: []string{"id", "space_id", "code", "inviter_id", "max_uses", "uses", "expires_at", "created_at"},
	PartKey: []string{"id"},
}

var SpaceInviteTable = table.New(spaceInviteMeta)

var spaceInviteByCodeMeta = table.Metadata{
	Name:    "space_invites_by_code",
	Columns: []string{"code", "id", "space_id", "inviter_id", "max_uses", "uses", "expires_at", "created_at"},
	PartKey: []string{"code"},
}

var SpaceInviteByCodeTable = table.New(spaceInviteByCodeMeta)

var spaceInviteBySpaceMeta = table.Metadata{
	Name:    "space_invites_by_space",
	Columns: []string{"space_id", "id", "code", "inviter_id", "max_uses", "uses", "expires_at", "created_at"},
	PartKey: []string{"space_id"},
	SortKey: []string{"created_at", "id"},
}

var SpaceInviteBySpaceTable = table.New(spaceInviteBySpaceMeta)

type SpaceInvite struct {
	ID        string     `db:"id" json:"id"`
	SpaceID   int64      `db:"space_id" json:"space_id"`
	Code      string     `db:"code" json:"code"`
	InviterID string     `db:"inviter_id" json:"inviter_id"`
	MaxUses   *int       `db:"max_uses" json:"max_uses,omitempty"`
	Uses      int        `db:"uses" json:"uses"`
	ExpiresAt *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
}

type SpaceInviteByCode struct {
	Code      string     `db:"code" json:"code"`
	ID        string     `db:"id" json:"id"`
	SpaceID   int64      `db:"space_id" json:"space_id"`
	InviterID string     `db:"inviter_id" json:"inviter_id"`
	MaxUses   *int       `db:"max_uses" json:"max_uses,omitempty"`
	Uses      int        `db:"uses" json:"uses"`
	ExpiresAt *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
}

type SpaceInviteBySpace struct {
	SpaceID   int64      `db:"space_id" json:"space_id"`
	ID        string     `db:"id" json:"id"`
	Code      string     `db:"code" json:"code"`
	InviterID string     `db:"inviter_id" json:"inviter_id"`
	MaxUses   *int       `db:"max_uses" json:"max_uses,omitempty"`
	Uses      int        `db:"uses" json:"uses"`
	ExpiresAt *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
}

func (si *SpaceInvite) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS space_invites (
			id text PRIMARY KEY,
			space_id bigint,
			code text,
			inviter_id text,
			max_uses int,
			uses int,
			expires_at timestamp,
			created_at timestamp
		);`,
	}
}

func (sibc *SpaceInviteByCode) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS space_invites_by_code (
			code text PRIMARY KEY,
			id text,
			space_id bigint,
			inviter_id text,
			max_uses int,
			uses int,
			expires_at timestamp,
			created_at timestamp
		);`,
	}
}

func (sibs *SpaceInviteBySpace) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS space_invites_by_space (
			space_id bigint,
			id text,
			code text,
			inviter_id text,
			max_uses int,
			uses int,
			expires_at timestamp,
			created_at timestamp,
			PRIMARY KEY (space_id, created_at, id)
		) WITH CLUSTERING ORDER BY (created_at DESC, id ASC);`,
	}
}

func (si *SpaceInvite) GetTableMeta() table.Metadata {
	return spaceInviteMeta
}

func (sibc *SpaceInviteByCode) GetTableMeta() table.Metadata {
	return spaceInviteByCodeMeta
}

func (sibs *SpaceInviteBySpace) GetTableMeta() table.Metadata {
	return spaceInviteBySpaceMeta
}
