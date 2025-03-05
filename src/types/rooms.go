package types

import (
	"time"
)

// Room types
const (
	RoomTypePM            = 0
	RoomTypeGroupPM       = 1
	RoomTypeServerChannel = 2
)

type Room struct {
	ID            string    `json:"id"`
	Creator       *string   `json:"creator,omitempty"` // null if DM, set if group
	Recipients    []string  `json:"recipients"`        // array of user IDs
	Type          int       `json:"type"`              // 0 = DM, 1 = Group DM, 2 = Server Channel
	LastMessageId string    `json:"last_message_id,omitempty"`
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
