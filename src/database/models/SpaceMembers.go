package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

var spaceMemberMeta = table.Metadata{
	Name:    "space_members",
	Columns: []string{"space_id", "user_id", "nick", "avatar", "joined_at", "premium_since", "deaf", "mute", "flags", "pending", "permissions", "communication_disabled_until"},
	PartKey: []string{"space_id", "user_id"},
}

var SpaceMemberTable = table.New(spaceMemberMeta)

type SpaceMember struct {
	SpaceID                    int64      `db:"space_id" json:"space_id,string"`
	UserID                     string     `db:"user_id" json:"user_id"`
	Nick                       *string    `db:"nick" json:"nick,omitempty"`
	Avatar                     *string    `db:"avatar" json:"avatar,omitempty"`
	JoinedAt                   time.Time  `db:"joined_at" json:"joined_at"`
	PremiumSince               *time.Time `db:"premium_since" json:"premium_since,omitempty"`
	Deaf                       bool       `db:"deaf" json:"deaf"`
	Mute                       bool       `db:"mute" json:"mute"`
	Flags                      int        `db:"flags" json:"flags"`
	Pending                    bool       `db:"pending" json:"pending"`
	Permissions                *string    `db:"permissions" json:"permissions,omitempty"`
	CommunicationDisabledUntil *time.Time `db:"communication_disabled_until" json:"communication_disabled_until,omitempty"`
}

func (sm *SpaceMember) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS space_members (
			space_id bigint,
			user_id text,
			nick text,
			avatar text,
			joined_at timestamp,
			premium_since timestamp,
			deaf boolean,
			mute boolean,
			flags int,
			pending boolean,
			permissions text,
			communication_disabled_until timestamp,
			PRIMARY KEY (space_id, user_id)
		);`,
	}
}

// SpaceMembersByUser table for efficient user-based queries
var spaceMembersByUserMeta = table.Metadata{
	Name:    "space_members_by_user",
	Columns: []string{"user_id", "space_id", "joined_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"space_id"},
}

var SpaceMembersByUserTable = table.New(spaceMembersByUserMeta)

type SpaceMembersByUser struct {
	UserID   string    `db:"user_id" json:"user_id"`
	SpaceID  int64     `db:"space_id" json:"space_id,string"`
	JoinedAt time.Time `db:"joined_at" json:"joined_at"`
}

func (smbu *SpaceMembersByUser) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS space_members_by_user (
			user_id text,
			space_id bigint,
			joined_at timestamp,
			PRIMARY KEY (user_id, space_id)
		);`,
	}
}
