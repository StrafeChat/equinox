package messages

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
)

// SnowflakeID is a request-body id that decodes from either a JSON string ("2102…") or a
// bare number. Clients written in JavaScript must send the string form: snowflakes exceed
// 2^53, so a JSON number has already been rounded (to the nearest 8) by the time it is
// serialized - which is exactly how every reply used to point at a message that didn't exist.
type SnowflakeID int64

func (s *SnowflakeID) UnmarshalJSON(b []byte) error {
	t := strings.TrimSpace(string(b))
	if t == "" || t == "null" {
		return nil
	}
	t = strings.Trim(t, `"`)
	v, err := id.Parse(t)
	if err != nil {
		return err
	}
	*s = SnowflakeID(v)
	return nil
}

func (s SnowflakeID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.Format(int64(s)))
}

// Int64Ptr returns the id as the *int64 the storage layer uses (nil for a nil receiver).
func (s *SnowflakeID) Int64Ptr() *int64 {
	if s == nil {
		return nil
	}
	v := int64(*s)
	return &v
}

// Message is an E2EE message: server stores ciphertext; when room has E2EE disabled, Plaintext is set instead.
// For system messages (e.g. member added/removed, room renamed), SenderID is 0 and SystemType/SystemPayload are set.
//
// Mentions/MentionEveryone/MentionRoles: for E2EE rooms these are trusted client-declared
// metadata (the server never sees the plaintext to verify them - same tradeoff Matrix/Signal
// make). For non-E2EE rooms they're parsed server-side from Plaintext and are authoritative;
// see messages.Service.Create.
type Message struct {
	RoomID          int64   `db:"room_id" json:"room_id"`
	ID              int64   `db:"id" json:"id"`
	SenderID        int64   `db:"sender_id" json:"sender_id"`
	SenderDeviceID  int64   `db:"sender_device_id" json:"sender_device_id"`
	Ciphertext      string  `db:"ciphertext" json:"ciphertext"`
	Plaintext       string  `db:"plaintext" json:"plaintext,omitempty"`
	ReplyToID       *int64  `db:"reply_to_id" json:"reply_to_id,omitempty"`
	Mentions        []int64 `db:"mentions" json:"mentions,omitempty"`
	MentionEveryone bool    `db:"mention_everyone" json:"mention_everyone,omitempty"`
	MentionRoles    []int64 `db:"mention_roles" json:"mention_roles,omitempty"`
	SystemType      string  `db:"system_type" json:"system_type,omitempty"`
	SystemPayload   string  `db:"system_payload" json:"system_payload,omitempty"`
	// AttachmentsJSON is the denormalised []Attachment for this message (see
	// Attachments()/SetAttachments) - stored as opaque JSON so listing history stays a
	// single partition read with no per-message registry lookup.
	AttachmentsJSON string     `db:"attachments" json:"-"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time  `db:"updated_at" json:"updated_at"`
	DeletedAt       *time.Time `db:"deleted_at" json:"deleted_at,omitempty"`
}

// Attachments decodes the message's attachment list (nil when there are none).
func (m *Message) Attachments() []Attachment {
	if m == nil || strings.TrimSpace(m.AttachmentsJSON) == "" {
		return nil
	}
	var out []Attachment
	if err := json.Unmarshal([]byte(m.AttachmentsJSON), &out); err != nil {
		return nil
	}
	return out
}

// SetAttachments stores the attachment list ("" for none).
func (m *Message) SetAttachments(list []Attachment) {
	if len(list) == 0 {
		m.AttachmentsJSON = ""
		return
	}
	b, err := json.Marshal(list)
	if err != nil {
		m.AttachmentsJSON = ""
		return
	}
	m.AttachmentsJSON = string(b)
}

// Attachment is the wire/stored form of a file attached to a message. In E2EE rooms only
// ID/URL/Size/Encrypted are populated - the rest (and the decryption key) is inside the
// message ciphertext, which is why the server never needs to know a file's real name.
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Encrypted   bool   `json:"encrypted,omitempty"`
}

// AttachmentRow is the message_attachments registry row: one per upload, so that a
// message can only reference uploads its own sender made into the same room, and so a
// deleted message's blobs can be cleaned up.
type AttachmentRow struct {
	RoomID      int64     `db:"room_id"`
	ID          int64     `db:"id"`
	UploaderID  int64     `db:"uploader_id"`
	MessageID   *int64    `db:"message_id"`
	Filename    string    `db:"filename"`
	ContentType string    `db:"content_type"`
	Size        int64     `db:"size"`
	URL         string    `db:"url"`
	Width       int       `db:"width"`
	Height      int       `db:"height"`
	Encrypted   bool      `db:"encrypted"`
	CreatedAt   time.Time `db:"created_at"`
}

// ToAttachment is the wire form of a registry row. Encrypted uploads deliberately drop
// everything but id/url/size - the uploader never told us the rest, and the copy inside
// the ciphertext is the authoritative one anyway.
func (r *AttachmentRow) ToAttachment() Attachment {
	a := Attachment{ID: id.Format(r.ID), Size: r.Size, URL: r.URL, Encrypted: r.Encrypted}
	if !r.Encrypted {
		a.Filename = r.Filename
		a.ContentType = r.ContentType
		a.Width = r.Width
		a.Height = r.Height
	}
	return a
}

// UploadAttachmentInput describes one multipart upload into a room.
type UploadAttachmentInput struct {
	Filename    string
	ContentType string
	Size        int64
	Width       int
	Height      int
	// Encrypted uploads are opaque blobs: no name/type is recorded and the stored
	// content type is application/octet-stream regardless of what the client sent.
	Encrypted bool
	Body      io.Reader
}

// CreateMessageInput is the payload for POST. When room has E2EE disabled, Plaintext is sent and Ciphertext may be empty.
// Mentions/MentionEveryone/MentionRoles are only used as-is for E2EE rooms (see Message doc); non-E2EE rooms ignore
// whatever the client sends here and derive mentions from Plaintext instead. Mentions/MentionRoles are snowflake
// strings (like every other id in this API, e.g. rooms.CreatePM's RecipientID) - unparseable entries are dropped.
type CreateMessageInput struct {
	// Accepts "123" (preferred) or 123 - device ids are snowflakes, so a JSON number from a
	// JS client arrives rounded to the nearest 2^53 multiple.
	SenderDeviceID SnowflakeID `json:"sender_device_id"`
	Ciphertext     string      `json:"ciphertext"`
	Plaintext      string      `json:"plaintext,omitempty"`
	// Accepts "123" (preferred) or 123 - see SnowflakeID.
	ReplyToID       *SnowflakeID `json:"reply_to_id,omitempty"`
	Mentions        []string     `json:"mentions,omitempty"`
	MentionEveryone bool         `json:"mention_everyone,omitempty"`
	MentionRoles    []string     `json:"mention_roles,omitempty"`
	// Attachments are ids returned by POST /rooms/:id/attachments, uploaded by this same
	// user into this same room and not yet used by another message.
	Attachments []string `json:"attachments,omitempty"`
}

// EditMessageInput is the payload for PATCH. Exactly one of Ciphertext/Plaintext is used,
// matching whichever the room's E2EE setting dictates - same rule Create already applies.
type EditMessageInput struct {
	Ciphertext string `json:"ciphertext,omitempty"`
	Plaintext  string `json:"plaintext,omitempty"`
}
