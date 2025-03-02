package models

import (
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

var roomsByUserMeta = table.Metadata{
	Name:    "rooms_by_user",
	Columns: []string{"user_id", "room_id", "last_message_id", "created_at", "updated_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"updated_at"}, // Changed back to updated_at for sorting
}

var RoomsByUserTable = table.New(roomsByUserMeta)

type RoomsByUser struct {
	UserId        string    `db:"user_id" json:"user_id"`
	RoomId        string    `db:"room_id" json:"room_id"`
	LastMessageId *string   `db:"last_message_id" json:"last_message_id"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time `db:"updated_at" json:"updated_at"`
}

func (r *RoomsByUser) SchemaDefinition() []string {
	return []string{
		`CREATE MATERIALIZED VIEW IF NOT EXISTS rooms_by_user AS
			SELECT user_id, room_id, last_message_id, created_at, updated_at
			FROM rooms
			WHERE user_id IS NOT NULL AND room_id IS NOT NULL AND updated_at IS NOT NULL
			PRIMARY KEY ((user_id), updated_at, room_id)
		WITH CLUSTERING ORDER BY (updated_at DESC);`,
	}
}

// GetRoomsForUser retrieves all rooms for a specific user
// Returns rooms sorted by most recent message
func GetRoomsForUser(userId string) ([]RoomsByUser, error) {
	var rooms []RoomsByUser
	// Query would be implemented with the session from your database package
	// Example: err := session.Query(RoomsByUserTable.SelectQuery()).BindMap(qb.M{"user_id": userId}).SelectRelease(&rooms)
	return rooms, nil
}
