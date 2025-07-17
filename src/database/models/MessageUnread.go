package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

type MessageUnread struct {
	UserID    string    `db:"user_id" json:"user_id"`
	RoomID    string    `db:"room_id" json:"room_id"`
	MessageID string    `db:"message_id" json:"message_id"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

var MessageUnreadMeta = table.Metadata{
	Name:    "message_unreads",
	Columns: []string{"user_id", "room_id", "message_id", "created_at"},
	PartKey: []string{"user_id", "room_id"},
	SortKey: []string{"message_id"},
}

var MessageUnreadTable = table.New(MessageUnreadMeta)

func (m *MessageUnread) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS message_unreads (
			user_id bigint,
			room_id bigint,
			message_id bigint,
			created_at timestamp,
			PRIMARY KEY ((user_id, room_id), message_id)
		) WITH CLUSTERING ORDER BY (message_id DESC);`,
	}
}
