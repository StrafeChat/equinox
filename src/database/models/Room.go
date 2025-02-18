package models

import (
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

var roomMeta = table.Metadata{
	Name:    "rooms",
	Columns: []string{"id", "creator", "recipients", "last_message_id", "created_at"},
	PartKey: []string{"id"},
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
			id uuid PRIMARY KEY,
			creator text,
			recipients list<text>,
			last_message_id bigint,
			created_at timestamp
		);`,
	}
}
