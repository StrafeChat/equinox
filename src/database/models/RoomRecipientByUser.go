package models

import (
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

type RoomRecipientByUser struct {
	UserId    string    `db:"user_id" json:"user_id"`
	RoomId    string    `db:"room_id" json:"room_id"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	LastSeen  time.Time `db:"last_seen" json:"last_seen"`
}

var RoomRecipientByUserMeta = table.Metadata{
	Name:    "room_recipients_by_user",
	Columns: []string{"user_id", "room_id", "created_at", "last_seen"},
	PartKey: []string{"user_id"},
	SortKey: []string{"created_at"},
}

var RoomRecipientByUserTable = table.New(RoomRecipientByUserMeta)

func (r *RoomRecipientByUser) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS room_recipients_by_user (
		    user_id bigint,
            room_id bigint,
            created_at timestamp,
            last_seen timestamp,
            PRIMARY KEY (user_id, created_at)
        )
		WITH CLUSTERING ORDER BY (created_at DESC);`,
	}
}
