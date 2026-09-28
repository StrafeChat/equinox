package devices

import (
	"encoding/json"
	"time"
)

// DeviceKeys is one device's self-signed key claim, stored opaquely - the server never
// parses or verifies its cryptographic content (the client is what verifies signatures
// before trusting a bundle; the server storing/relaying it faithfully is what matters
// here). Shape mirrors the Matrix /keys/upload `device_keys` object: identity Curve25519
// key + Ed25519 fingerprint key + algorithms, signed as one unit.
type DeviceKeys struct {
	UserID    int64     `db:"user_id" json:"-"`
	DeviceID  int64     `db:"device_id" json:"-"`
	KeysJSON  string    `db:"keys_json" json:"-"`
	CreatedAt time.Time `db:"created_at" json:"-"`
	UpdatedAt time.Time `db:"updated_at" json:"-"`
}

// Keys unmarshals KeysJSON for handlers that need to re-embed it in a response.
func (d *DeviceKeys) Keys() json.RawMessage {
	if d == nil || d.KeysJSON == "" {
		return nil
	}
	return json.RawMessage(d.KeysJSON)
}

// OneTimeKey is a single one-time (or fallback) key, keyed by the Matrix-style
// "<algorithm>:<key_id>" composite, e.g. "signed_curve25519:AAAAAQ". KeyJSON is the
// individually-signed key object, stored opaquely for the same reason as DeviceKeys.
type OneTimeKey struct {
	UserID     int64     `db:"user_id" json:"-"`
	DeviceID   int64     `db:"device_id" json:"-"`
	KeyID      string    `db:"key_id" json:"-"`
	KeyJSON    string    `db:"key_json" json:"-"`
	IsFallback bool      `db:"is_fallback" json:"-"`
	CreatedAt  time.Time `db:"created_at" json:"-"`
}

// ToDeviceMessage is a small encrypted control message addressed to one specific device
// (Olm pre-key/session messages, Megolm room-key distribution, device-revocation
// notices) - never chat content, which stays in the `messages` table.
type ToDeviceMessage struct {
	RecipientUserID   int64     `db:"recipient_user_id" json:"-"`
	RecipientDeviceID int64     `db:"recipient_device_id" json:"-"`
	MessageID         int64     `db:"message_id" json:"id"`
	SenderUserID      int64     `db:"sender_user_id" json:"sender_user_id"`
	SenderDeviceID    int64     `db:"sender_device_id" json:"sender_device_id"`
	SenderFID         string    `db:"sender_fid" json:"sender_fid,omitempty"`
	EventType         string    `db:"event_type" json:"type"`
	ContentJSON       string    `db:"content_json" json:"-"`
	CreatedAt         time.Time `db:"created_at" json:"-"`
}

// Content unmarshals ContentJSON for handlers that need to re-embed it in a response.
func (m *ToDeviceMessage) Content() json.RawMessage {
	if m == nil || m.ContentJSON == "" {
		return nil
	}
	return json.RawMessage(m.ContentJSON)
}

// --- Request/response shapes, mirroring the Matrix Client-Server API's key-management
// endpoints (https://spec.matrix.org/latest/client-server-api/#end-to-end-encryption).
// Reusing this well-documented, stable wire shape - rather than inventing a bespoke one -
// is deliberate: it's exactly what an OlmMachine-based client (vodozemac/matrix-sdk-crypto)
// already produces and expects, so the Go handlers are thin, low-risk adapters instead of
// a from-scratch protocol.

// UploadKeysInput mirrors POST /keys/upload.
type UploadKeysInput struct {
	DeviceKeys   json.RawMessage            `json:"device_keys,omitempty"`
	OneTimeKeys  map[string]json.RawMessage `json:"one_time_keys,omitempty"`
	FallbackKeys map[string]json.RawMessage `json:"fallback_keys,omitempty"`
}

// UploadKeysOutput mirrors the response to POST /keys/upload.
type UploadKeysOutput struct {
	OneTimeKeyCounts map[string]int `json:"one_time_key_counts"`
}

// QueryKeysInput mirrors POST /keys/query. DeviceKeys maps user_id -> list of device_ids
// (an empty list means "all of that user's devices").
type QueryKeysInput struct {
	DeviceKeys map[string][]string `json:"device_keys"`
}

// QueryKeysOutput mirrors the response to POST /keys/query.
type QueryKeysOutput struct {
	DeviceKeys map[string]map[string]json.RawMessage `json:"device_keys"`
	Failures   map[string]json.RawMessage            `json:"failures,omitempty"`
}

// ClaimKeysInput mirrors POST /keys/claim. OneTimeKeys maps user_id -> device_id -> algorithm.
type ClaimKeysInput struct {
	OneTimeKeys map[string]map[string]string `json:"one_time_keys"`
}

// ClaimKeysOutput mirrors the response to POST /keys/claim: user_id -> device_id ->
// {"<algorithm>:<key_id>": key_json}.
type ClaimKeysOutput struct {
	OneTimeKeys map[string]map[string]json.RawMessage `json:"one_time_keys"`
	Failures    map[string]json.RawMessage            `json:"failures,omitempty"`
}

// SendToDeviceInput mirrors PUT /sendToDevice/{eventType}/{txnId}. Messages maps
// user_id -> device_id (or "*" for all of that user's devices) -> event content.
type SendToDeviceInput struct {
	Messages map[string]map[string]json.RawMessage `json:"messages"`
}

// --- Asymmetric key backup (m.megolm_backup.v1.curve25519-aes-sha2), mirroring Matrix's
// /room_keys endpoints. The server is a dumb store here: it never holds anything that can
// read the backup, because room keys are encrypted to a public key and the matching private
// key only exists wrapped under the user's recovery code.

// KeyBackupVersion is one backup version's metadata.
type KeyBackupVersion struct {
	UserID    int64  `db:"user_id" json:"-"`
	Version   int64  `db:"version" json:"-"`
	Algorithm string `db:"algorithm" json:"-"`
	// AuthData is the scheme's public parameters, verbatim JSON - for curve25519-aes-sha2
	// that's {"public_key": "<base64>"}. Public by design: every device needs it to add keys.
	AuthData string `db:"auth_data" json:"-"`
	// WrappedPrivateKey is the backup decryption key, AES-256-GCM encrypted under a key
	// derived from the user's recovery code. Ciphertext as far as the server is concerned.
	WrappedPrivateKey string    `db:"wrapped_private_key" json:"-"`
	Salt              string    `db:"salt" json:"-"`
	Etag              string    `db:"etag" json:"-"`
	CreatedAt         time.Time `db:"created_at" json:"-"`
}

// KeyBackupSession is one backed-up Megolm session. SessionData is opaque ciphertext
// (ephemeral key + MAC + ciphertext, encrypted to the backup public key); the other fields
// are the plaintext metadata Matrix uses to decide whether an incoming copy of a session is
// better than the stored one - see BetterThan.
type KeyBackupSession struct {
	UserID            int64     `db:"user_id" json:"-"`
	Version           int64     `db:"version" json:"-"`
	RoomID            string    `db:"room_id" json:"-"`
	SessionID         string    `db:"session_id" json:"-"`
	SessionData       string    `db:"session_data" json:"-"`
	FirstMessageIndex int       `db:"first_message_index" json:"-"`
	ForwardedCount    int       `db:"forwarded_count" json:"-"`
	IsVerified        bool      `db:"is_verified" json:"-"`
	CreatedAt         time.Time `db:"created_at" json:"-"`
}

// BetterThan reports whether s should replace `other` in the backup, following the ordering
// the Matrix spec defines for /room_keys: a session from a verified device wins, then the one
// that decrypts further back in the room's history, then the one that travelled through
// fewer forwards. Without this, a second device uploading the same session from a later
// point would overwrite the copy that can read the earlier messages.
func (s *KeyBackupSession) BetterThan(other *KeyBackupSession) bool {
	if other == nil {
		return true
	}
	if s.IsVerified != other.IsVerified {
		return s.IsVerified
	}
	if s.FirstMessageIndex != other.FirstMessageIndex {
		return s.FirstMessageIndex < other.FirstMessageIndex
	}
	return s.ForwardedCount < other.ForwardedCount
}

// BackupSessionData is the per-session shape inside a PUT /room_keys/keys body.
type BackupSessionData struct {
	FirstMessageIndex int             `json:"first_message_index"`
	ForwardedCount    int             `json:"forwarded_count"`
	IsVerified        bool            `json:"is_verified"`
	SessionData       json.RawMessage `json:"session_data"`
}

// PutBackupKeysInput mirrors PUT /room_keys/keys: room id -> {"sessions": {session id -> data}}.
type PutBackupKeysInput struct {
	Rooms map[string]struct {
		Sessions map[string]BackupSessionData `json:"sessions"`
	} `json:"rooms"`
}
