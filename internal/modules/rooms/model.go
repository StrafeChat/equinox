package rooms

import (
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// RoomType discriminator for polymorphic rooms.
const (
	TypePM          = 1
	TypeGroupPM     = 2
	TypeSpaceText   = 3
	TypeSpaceVoice  = 4
	TypeRoomSection = 5
	TypeThread      = 6
)

// Room is a polymorphic container: PM, Group PM, Space text/voice, room section, or thread.
// For TypeGroupPM, CreatorID is the user who created the group (only they can remove members or rename).
// E2EEEnabled: when false, messages are stored as plaintext. For PM/GroupPM nil/true = E2EE on (default).
// For space rooms (TypeSpaceText, TypeSpaceVoice), E2EE is off by default (scale; a future space-scale
// E2EE protocol, e.g. MLS or sender-keys-style for thousands of members, could be added later).
type Room struct {
	ID             int64     `db:"id" json:"id"`
	Type           int       `db:"type" json:"type"`
	SpaceID        *int64    `db:"space_id" json:"space_id,omitempty"`
	ParentID       *int64    `db:"parent_id" json:"parent_id,omitempty"`
	Name           string    `db:"name" json:"name,omitempty"`
	Topic          string    `db:"topic" json:"topic,omitempty"`
	SlowmodeSeconds int      `db:"slowmode_seconds" json:"slowmode_seconds,omitempty"`
	Position       int       `db:"position" json:"position"`
	CreatorID      int64     `db:"creator_id" json:"creator_id,omitempty"`
	E2EEEnabled    *bool     `db:"e2ee_enabled" json:"e2ee_enabled,omitempty"`
	LastMessageID  *int64    `db:"last_message_id" json:"last_message_id,omitempty"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

// Participant is a minimal user for room display.
type Participant struct {
	ID            string              `json:"id"`
	Username      string              `json:"username"`
	Discriminator int                 `json:"discriminator"`
	DisplayName   string              `json:"display_name"`
	Avatar        string              `json:"avatar,omitempty"`
	Presence      auth.PublicPresence `json:"presence"`
}

// RoomWithParticipants extends Room with participant IDs and optional details.
type RoomWithParticipants struct {
	Room
	ParticipantIDs    []int64       `json:"recipients,omitempty"`
	Participants      []Participant `json:"participants,omitempty"`
	LastReadMessageID *int64        `json:"last_read_message_id,omitempty"`
	MentionCount      int           `json:"mention_count,omitempty"`
}

// RoomRow is rooms_by_user row.
type RoomRow struct {
	UserID             int64     `db:"user_id"`
	RoomID             int64     `db:"room_id"`
	LastMessageID      *int64    `db:"last_message_id"`
	LastReadMessageID  *int64    `db:"last_read_message_id"`
	MentionCount       int       `db:"mention_count"`
	JoinedAt           time.Time `db:"joined_at"`
}

// PMRoomsRow is pm_rooms lookup row.
type PMRoomsRow struct {
	UserAID   int64     `db:"user_a_id"`
	UserBID   int64     `db:"user_b_id"`
	RoomID    int64     `db:"room_id"`
	CreatedAt time.Time `db:"created_at"`
}

// RoomBySpaceRow is rooms_by_space row.
type RoomBySpaceRow struct {
	SpaceID   int64     `db:"space_id"`
	RoomID    int64     `db:"room_id"`
	Position  int       `db:"position"`
	CreatedAt time.Time `db:"created_at"`
}
