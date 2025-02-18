package types

import (
	"time"

	"github.com/gocql/gocql"
)

type Room struct {
	ID         gocql.UUID  `json:"id"`
	Creator    *string     `json:"creator,omitempty"` // null if DM, set if group
	Recipients []string    `json:"recipients"`        // array of user IDs
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  *time.Time  `json:"updated_at,omitempty"`
	DeletedAt  *time.Time  `json:"deleted_at,omitempty"`
}

type CreateRoomInput struct {
	Recipients []string `json:"recipients" validate:"required,min=1"`
	IsGroup    bool     `json:"is_group"    validate:"required"`
}

type GetRoomsByRecipientInput struct {
	RecipientID string `json:"recipient_id" validate:"required,uuid"`
	Limit       int    `json:"limit"        validate:"required,min=1,max=100"`
	Cursor      string `json:"cursor"       validate:"omitempty,uuid"`
}