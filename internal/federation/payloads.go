package federation

import (
	"encoding/json"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/messages"
)

// Wire types for /federation/v1. Ids are strings (snowflakes exceed 2^53), users are FIDs
// (@id:domain), rooms and messages are referenced by their origin identity.

// Profile is a user as one instance describes them to another. Carrying the profile
// with every reference means a receiver can create the shadow row without a round trip.
type Profile struct {
	FID           string `json:"fid"`
	Username      string `json:"username"`
	Discriminator int    `json:"discriminator"`
	DisplayName   string `json:"display_name"`
	Avatar        string `json:"avatar,omitempty"`
	Banner        string `json:"banner,omitempty"`
	Bio           string `json:"bio,omitempty"`
	AboutMe       string `json:"about_me,omitempty"`
	Bot           bool   `json:"bot,omitempty"`
}

type RoomRef struct {
	OriginDomain string `json:"origin_domain"`
	OriginRoomID string `json:"origin_room_id"`
}

type MessageRef struct {
	OriginDomain    string `json:"origin_domain"`
	OriginMessageID string `json:"origin_message_id"`
}

// RoomAnnounce: POST /rooms - the origin instance tells a peer about a new room some of
// its users are in. Idempotent on (origin_domain, origin_room_id).
type RoomAnnounce struct {
	Room         RoomRef   `json:"room"`
	Type         int       `json:"type"`
	Name         string    `json:"name,omitempty"`
	E2EEEnabled  bool      `json:"e2ee_enabled"`
	Creator      string    `json:"creator,omitempty"`
	Participants []Profile `json:"participants"`
}

// RoomAnnounceReply returns the receiver's local room id (for diagnostics only).
type RoomAnnounceReply struct {
	RoomID string `json:"room_id"`
}

// ParticipantsUpdate: PUT /rooms/participants - the full current member set plus who left.
type ParticipantsUpdate struct {
	Room         RoomRef   `json:"room"`
	Participants []Profile `json:"participants"`
	Removed      []string  `json:"removed,omitempty"`
}

// RoomPatch: PATCH /rooms - name / E2EE setting changes.
type RoomPatch struct {
	Room        RoomRef `json:"room"`
	Name        *string `json:"name,omitempty"`
	E2EEEnabled *bool   `json:"e2ee_enabled,omitempty"`
}

// TypingEvent: POST /rooms/typing.
type TypingEvent struct {
	Room RoomRef `json:"room"`
	User string  `json:"user"`
}

// MessageEvent: POST /rooms/messages - a new message (including system messages, which
// have SystemType set and no sender).
type MessageEvent struct {
	Room            RoomRef               `json:"room"`
	Message         MessageRef            `json:"message"`
	Sender          *Profile              `json:"sender,omitempty"`
	SenderDeviceID  string                `json:"sender_device_id,omitempty"`
	Ciphertext      string                `json:"ciphertext,omitempty"`
	Plaintext       string                `json:"plaintext,omitempty"`
	ReplyTo         *MessageRef           `json:"reply_to,omitempty"`
	Mentions        []string              `json:"mentions,omitempty"`
	MentionEveryone bool                  `json:"mention_everyone,omitempty"`
	Attachments     []messages.Attachment `json:"attachments,omitempty"`
	SystemType      string                `json:"system_type,omitempty"`
	SystemPayload   string                `json:"system_payload,omitempty"`
	CreatedAt       time.Time             `json:"created_at"`
}

// MessageEdit: PATCH /rooms/messages.
type MessageEdit struct {
	Room       RoomRef    `json:"room"`
	Message    MessageRef `json:"message"`
	Ciphertext string     `json:"ciphertext,omitempty"`
	Plaintext  string     `json:"plaintext,omitempty"`
}

// MessageDelete: POST /rooms/messages/delete.
type MessageDelete struct {
	Room    RoomRef    `json:"room"`
	Message MessageRef `json:"message"`
}

// ProfileUpdate: POST /users/update - a user on the sending instance changed their profile.
type ProfileUpdate struct {
	User Profile `json:"user"`
}

// KeysQuery / KeysClaim mirror the Matrix-shaped device endpoints, scoped to the
// receiver's own users.
type KeysQuery struct {
	DeviceKeys map[string][]string `json:"device_keys"`
}

type KeysClaim struct {
	OneTimeKeys map[string]map[string]string `json:"one_time_keys"`
}

// ToDevice: POST /to_device - Olm/Megolm to-device traffic for the receiver's users.
type ToDevice struct {
	Sender         Profile                               `json:"sender"`
	SenderDeviceID string                                `json:"sender_device_id"`
	EventType      string                                `json:"event_type"`
	Messages       map[string]map[string]json.RawMessage `json:"messages"`
}
