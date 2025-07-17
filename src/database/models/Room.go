package models

import (
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

var roomMeta = table.Metadata{
	Name:    "rooms",
	Columns: []string{"id", "creator", "recipients", "type", "space_id", "parent_id", "name", "topic", "icon", "last_message_id", "position", "created_at", "updated_at"},
	PartKey: []string{"id"},
}

var RoomTable = table.New(roomMeta)

type Room struct {
	ID            int64     `db:"id" json:"id"`
	Creator       *int64    `db:"creator" json:"creator"`
	Recipients    []int64   `db:"recipients" json:"recipients"`
	Type          int       `db:"type" json:"type"`
	SpaceID       *int64    `db:"space_id" json:"space_id"`
	ParentID      *int64    `db:"parent_id" json:"parent_id"`
	Name          *string   `db:"name" json:"name"`
	Topic         *string   `db:"topic" json:"topic"`
	Icon          *string   `db:"icon" json:"icon"`
	LastMessageId *int64    `db:"last_message_id" json:"last_message_id"`
	Position      *int      `db:"position" json:"position"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time `db:"updated_at" json:"updated_at"`
}

func (r *Room) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS rooms (
				id bigint PRIMARY KEY,
				creator bigint,
				recipients list<bigint>,
				type int,
				space_id bigint,
				parent_id text,
				name text,
				topic text,
				icon text,
				last_message_id bigint,
				position int,
				created_at timestamp,
				updated_at timestamp
			);`,
	}
}
