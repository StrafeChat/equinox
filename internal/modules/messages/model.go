package messages

import "time"

// Message is an E2EE message: server stores ciphertext only.
type Message struct {
	RoomID          int64     `db:"room_id" json:"room_id"`
	ID              int64     `db:"id" json:"id"`
	SenderID        int64     `db:"sender_id" json:"sender_id"`
	SenderDeviceID  int64     `db:"sender_device_id" json:"sender_device_id"`
	Ciphertext      string    `db:"ciphertext" json:"ciphertext"`
	ReplyToID       *int64    `db:"reply_to_id" json:"reply_to_id,omitempty"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
	DeletedAt       *time.Time `db:"deleted_at" json:"deleted_at,omitempty"`
}

// CreateMessageInput is the payload for POST.
type CreateMessageInput struct {
	SenderDeviceID int64   `json:"sender_device_id"`
	Ciphertext     string  `json:"ciphertext"`
	ReplyToID      *int64  `json:"reply_to_id,omitempty"`
}

// EditMessageInput is the payload for PATCH.
type EditMessageInput struct {
	Ciphertext string `json:"ciphertext"`
}
