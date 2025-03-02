package models

import (
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

type MessagesByRoom struct {
	RoomID    string    `db:"room_id" json:"room_id"`
	ID        string    `db:"id" json:"id"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

var MessagesByRoomMeta = table.Metadata{
	Name:    "messages_by_room",
	Columns: []string{"room_id", "id", "created_at"},
	PartKey: []string{"room_id"},
	SortKey: []string{"id"},
}

var MessagesByRoomTable = table.New(MessagesByRoomMeta)

func (m *MessagesByRoom) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS messages_by_room (
			room_id bigint,
			id bigint,
			created_at timestamp,
			PRIMARY KEY (room_id, id)
		) WITH CLUSTERING ORDER BY (id DESC);`,
	}
}
