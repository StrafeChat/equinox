package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

type MessageMentionUnread struct {
	UserID    string    `db:"user_id" json:"user_id"`
	RoomID    string    `db:"room_id" json:"room_id"`
	MessageID string    `db:"message_id" json:"message_id"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

var MessageMentionUnreadMeta = table.Metadata{
	Name:    "message_mention_unreads",
	Columns: []string{"user_id", "room_id", "message_id", "created_at"},
	PartKey: []string{"user_id", "room_id"},
	SortKey: []string{"message_id"},
}

var MessageMentionUnreadTable = table.New(MessageMentionUnreadMeta)

func (m *MessageMentionUnread) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS message_mention_unreads (
			user_id text,
			room_id text,
			message_id text,
			created_at timestamp,
			PRIMARY KEY ((user_id, room_id), message_id)
		) WITH CLUSTERING ORDER BY (message_id DESC);`,
	}
}