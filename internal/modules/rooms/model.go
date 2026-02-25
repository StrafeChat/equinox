package rooms

import "time"

// RoomType discriminator for polymorphic rooms.
const (
	TypePM            = 1
	TypeGroupPM       = 2
	TypeSpaceText     = 3
	TypeSpaceVoice    = 4
	TypeSpaceCategory = 5
	TypeThread        = 6
)

// Room is a polymorphic channel: PM, Group PM, Space text/voice, category, or thread.
type Room struct {
	ID             int64     `db:"id" json:"id"`
	Type           int       `db:"type" json:"type"`
	SpaceID        *int64    `db:"space_id" json:"space_id,omitempty"`
	ParentID       *int64    `db:"parent_id" json:"parent_id,omitempty"`
	Name           string    `db:"name" json:"name,omitempty"`
	Topic          string    `db:"topic" json:"topic,omitempty"`
	Position       int       `db:"position" json:"position"`
	LastMessageID  *int64    `db:"last_message_id" json:"last_message_id,omitempty"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

// RoomWithParticipants extends Room with participant IDs.
type RoomWithParticipants struct {
	Room
	ParticipantIDs []int64 `json:"recipients,omitempty"`
}

// RoomRow is rooms_by_user row.
type RoomRow struct {
	UserID        int64     `db:"user_id"`
	RoomID        int64     `db:"room_id"`
	LastMessageID *int64    `db:"last_message_id"`
	JoinedAt      time.Time `db:"joined_at"`
}

// PMRoomsRow is pm_rooms lookup row.
type PMRoomsRow struct {
	UserAID   int64     `db:"user_a_id"`
	UserBID   int64     `db:"user_b_id"`
	RoomID    int64     `db:"room_id"`
	CreatedAt time.Time `db:"created_at"`
}
