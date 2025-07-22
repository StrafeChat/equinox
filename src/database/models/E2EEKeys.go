package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

// E2EEIdentityKey stores the long-term identity key for each user
// Note: Private keys should NEVER be stored on the server in production
type E2EEIdentityKey struct {
	UserID    string    `db:"user_id" json:"user_id"`
	PublicKey []byte    `db:"public_key" json:"public_key"`
	KeyType   string    `db:"key_type" json:"key_type"` // "ed25519" for identity keys
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

var identityKeyMeta = table.Metadata{
	Name:    "e2ee_identity_keys",
	Columns: []string{"user_id", "public_key", "key_type", "created_at", "updated_at"},
	PartKey: []string{"user_id"},
}

var E2EEIdentityKeyTable = table.New(identityKeyMeta)

// E2EEPreKey stores one-time prekeys for the Signal Protocol
type E2EEPreKey struct {
	UserID    string    `db:"user_id" json:"user_id"`
	KeyID     int32     `db:"key_id" json:"key_id"`
	PublicKey []byte    `db:"public_key" json:"public_key"`
	Used      bool      `db:"used" json:"used"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

var preKeyMeta = table.Metadata{
	Name:    "e2ee_prekeys",
	Columns: []string{"user_id", "key_id", "public_key", "used", "created_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"key_id"},
}

var E2EEPreKeyTable = table.New(preKeyMeta)

// E2EESignedPreKey stores the signed prekey for the Signal Protocol
type E2EESignedPreKey struct {
	UserID    string    `db:"user_id" json:"user_id"`
	KeyID     int32     `db:"key_id" json:"key_id"`
	PublicKey []byte    `db:"public_key" json:"public_key"`
	Signature []byte    `db:"signature" json:"signature"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

var signedPreKeyMeta = table.Metadata{
	Name:    "e2ee_signed_prekeys",
	Columns: []string{"user_id", "key_id", "public_key", "signature", "created_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"key_id"},
}

var E2EESignedPreKeyTable = table.New(signedPreKeyMeta)

// E2EESession stores the session state between two users
type E2EESession struct {
	UserID       string    `db:"user_id" json:"user_id"`
	RecipientID  string    `db:"recipient_id" json:"recipient_id"`
	SessionData  []byte    `db:"session_data" json:"session_data"`
	SessionVersion uint32  `db:"session_version" json:"session_version"`
	ProtocolVersion string `db:"protocol_version" json:"protocol_version"` // "signal_v1"
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

var e2eeSessionMeta = table.Metadata{
	Name:    "e2ee_sessions",
	Columns: []string{"user_id", "recipient_id", "session_data", "session_version", "protocol_version", "created_at", "updated_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"recipient_id"},
}

var E2EESessionTable = table.New(e2eeSessionMeta)

// E2EEGroupSession stores group session keys for group PMs
type E2EEGroupSession struct {
	RoomID      string    `db:"room_id" json:"room_id"`
	SessionID   string    `db:"session_id" json:"session_id"`
	SessionKey  []byte    `db:"session_key" json:"session_key"`
	CreatorID   string    `db:"creator_id" json:"creator_id"`
	Participants []string `db:"participants" json:"participants"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

var groupSessionMeta = table.Metadata{
	Name:    "e2ee_group_sessions",
	Columns: []string{"room_id", "session_id", "session_key", "creator_id", "participants", "created_at", "updated_at"},
	PartKey: []string{"room_id"},
	SortKey: []string{"session_id"},
}

var E2EEGroupSessionTable = table.New(groupSessionMeta)

// Schema definitions
func (e *E2EEIdentityKey) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS e2ee_identity_keys (
			user_id text PRIMARY KEY,
			public_key blob,
			key_type text,
			created_at timestamp,
			updated_at timestamp
		);`,
	}
}

func (e *E2EEPreKey) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS e2ee_prekeys (
			user_id text,
			key_id int,
			public_key blob,
			used boolean,
			created_at timestamp,
			PRIMARY KEY (user_id, key_id)
		);`,
	}
}

func (e *E2EESignedPreKey) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS e2ee_signed_prekeys (
			user_id text,
			key_id int,
			public_key blob,
			signature blob,
			created_at timestamp,
			PRIMARY KEY (user_id, key_id)
		);`,
	}
}

func (e *E2EESession) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS e2ee_sessions (
			user_id text,
			recipient_id text,
			session_data blob,
			session_version int,
			protocol_version text,
			created_at timestamp,
			updated_at timestamp,
			PRIMARY KEY (user_id, recipient_id)
		);`,
	}
}

func (e *E2EEGroupSession) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS e2ee_group_sessions (
			room_id text,
			session_id text,
			session_key blob,
			creator_id text,
			participants set<text>,
			created_at timestamp,
			updated_at timestamp,
			PRIMARY KEY (room_id, session_id)
		);`,
	}
}