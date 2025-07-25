package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

var spaceMeta = table.Metadata{
	Name:    "spaces",
	Columns: []string{"id", "name", "name_acronym", "description", "icon", "banner", "owner_id", "verification_level", "default_message_notifications", "explicit_content_filter", "features", "afk_room_id", "afk_timeout", "system_room_id", "system_room_flags", "rules_room_id", "max_presences", "max_members", "vanity_url_code", "preferred_locale", "public_updates_room_id", "max_video_room_users", "nsfw_level", "created_at", "updated_at"},
	PartKey: []string{"id"},
}

var SpaceTable = table.New(spaceMeta)

type Space struct {
	ID                          int64     `db:"id" json:"id,string"`
	Name                        string    `db:"name" json:"name"`
	NameAcronym                 string    `db:"name_acronym" json:"name_acronym"`
	Description                 *string   `db:"description" json:"description,omitempty"`
	Icon                        *string   `db:"icon" json:"icon,omitempty"`
	Banner                      *string   `db:"banner" json:"banner,omitempty"`
	OwnerID                     string    `db:"owner_id" json:"owner_id"`
	VerificationLevel           int       `db:"verification_level" json:"verification_level"`
	DefaultMessageNotifications int       `db:"default_message_notifications" json:"default_message_notifications"`
	ExplicitContentFilter       int       `db:"explicit_content_filter" json:"explicit_content_filter"`
	Features                    []string  `db:"features" json:"features"`
	AfkRoomID                   *string   `db:"afk_room_id" json:"afk_room_id,omitempty"`
	AfkTimeout                  int       `db:"afk_timeout" json:"afk_timeout"`
	SystemRoomID                *string   `db:"system_room_id" json:"system_room_id,omitempty"`
	SystemRoomFlags             int       `db:"system_room_flags" json:"system_room_flags"`
	RulesRoomID                 *string   `db:"rules_room_id" json:"rules_room_id,omitempty"`
	MaxPresences                *int      `db:"max_presences" json:"max_presences,omitempty"`
	MaxMembers                  *int      `db:"max_members" json:"max_members,omitempty"`
	VanityUrlCode               *string   `db:"vanity_url_code" json:"vanity_url_code,omitempty"`
	PreferredLocale             string    `db:"preferred_locale" json:"preferred_locale"`
	PublicUpdatesRoomID         *string   `db:"public_updates_room_id" json:"public_updates_room_id,omitempty"`
	MaxVideoRoomUsers           *int      `db:"max_video_room_users" json:"max_video_room_users,omitempty"`
	NsfwLevel                   int       `db:"nsfw_level" json:"nsfw_level"`
	CreatedAt                   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt                   time.Time `db:"updated_at" json:"updated_at"`
}

func (s *Space) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS spaces (
			id bigint PRIMARY KEY,
			name text,
			name_acronym text,
			description text,
			icon text,
			banner text,
			owner_id text,
			verification_level int,
			default_message_notifications int,
			explicit_content_filter int,
			features list<text>,
			afk_room_id text,
			afk_timeout int,
			system_room_id text,
			system_room_flags int,
			rules_room_id text,
			max_presences int,
			max_members int,
			vanity_url_code text,
			preferred_locale text,
			public_updates_room_id text,
			max_video_room_users int,
			nsfw_level int,
			created_at timestamp,
			updated_at timestamp
		);`,
	}
}
