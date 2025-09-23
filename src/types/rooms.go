package types

import (
	"time"
)

// Room types
const (
	RoomTypePM           = 0
	RoomTypeGroupPM      = 1
	RoomTypeTextRoom     = 2
	RoomTypeVoiceRoom    = 3
	RoomTypeSpaceSection = 4
)

type Room struct {
	ID            string    `json:"id"`
	Creator       *string   `json:"creator,omitempty"`   // null if DM, set if group
	Recipients    []string  `json:"recipients"`          // array of user IDs
	Type          int       `json:"type"`                // 0 = DM, 1 = Group DM, 2 = Text Room, 3 = Voice Room, 4 = Space Section
	SpaceID       *int64    `json:"space_id,omitempty,string"`  // space ID for space rooms/sections
	ParentID      *string   `json:"parent_id,omitempty"` // parent section ID if this room belongs to a section
	Name          *string   `json:"name,omitempty"`      // optional name for group PMs
	Topic         *string   `json:"topic,omitempty"`     // optional topic for group PMs
	Icon          *string   `json:"icon,omitempty"`      // optional icon for group PMs
	LastMessageId string    `json:"last_message_id,omitempty"`
	Position      *int      `json:"position,omitempty"` // position for ordering rooms
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

type CreateRoomInput struct {
	Recipients []string `json:"recipients" validate:"required,min=1"`
	IsGroup    bool     `json:"is_group"   validate:"opitional"`
}

type GetRoomsByRecipientInput struct {
	RecipientID string `json:"recipient_id" validate:"required,string"`
	Limit       int    `json:"limit"        validate:"required,min=1,max=100"`
	Cursor      string `json:"cursor"       validate:"omitempty,uuid"`
}
