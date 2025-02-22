package models

import (
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

var roomMeta = table.Metadata{
	Name:    "rooms",
	Columns: []string{"id", "creator", "recipients", "last_message_id", "created_at", "updated_at"},
	PartKey: []string{"id"},
	SortKey: []string{"last_message_id"},
}

var RoomTable = table.New(roomMeta)

type Room struct {
	ID            string    `db:"id" json:"id"`
	Creator       *string   `db:"creator" json:"creator"`
	Recipients    []string  `db:"recipients" json:"recipients"`
	LastMessageId *string   `db:"last_message_id" json:"last_message_id"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
}

func (r *Room) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS rooms (
			id bigint,
			creator bigint,
			recipients list<bigint>,
			last_message_id bigint,
			created_at timestamp,
			updated_at timestamp,
			PRIMARY KEY (id, last_message_id)
		)
		WITH CLUSTERING ORDER BY (last_message_id DESC);`,
	}
}
