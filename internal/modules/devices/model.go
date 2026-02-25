package devices

import "time"

// DeviceKeys holds public keys for a device (Signal Protocol X3DH).
// Note: signed_prekey_signature is a placeholder until Ed25519 signing is implemented.
type DeviceKeys struct {
	UserID               int64     `db:"user_id" json:"-"`
	DeviceID             int64     `db:"device_id" json:"device_id"`
	IdentityKeyPublic    string    `db:"identity_key_public" json:"identity_key"`
	SignedPrekeyPublic   string    `db:"signed_prekey_public" json:"signed_prekey"`
	SignedPrekeySig      string    `db:"signed_prekey_signature" json:"signed_prekey_signature"`
	SignedPrekeyID       int       `db:"signed_prekey_id" json:"signed_prekey_id"`
	RegistrationID       int       `db:"registration_id" json:"registration_id"`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

// OneTimePrekey is a single-use key for X3DH.
type OneTimePrekey struct {
	UserID    int64     `db:"user_id" json:"-"`
	DeviceID  int64     `db:"device_id" json:"-"`
	KeyID     int       `db:"key_id" json:"key_id"`
	PublicKey string    `db:"public_key" json:"public_key"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// PrekeyBundle is fetched by initiator for X3DH session establishment.
type PrekeyBundle struct {
	IdentityKey    string `json:"identity_key"`
	SignedPrekey   string `json:"signed_prekey"`
	SignedPrekeyID int    `json:"signed_prekey_id"`
	SignedPrekeySig string `json:"signed_prekey_signature"`
	OneTimePrekey  *struct {
		KeyID     int    `json:"key_id"`
		PublicKey string `json:"public_key"`
	} `json:"one_time_prekey,omitempty"`
	RegistrationID int `json:"registration_id"`
}

// RegisterDeviceInput is the payload for device registration.
type RegisterDeviceInput struct {
	DeviceID           int64   `json:"device_id"`
	IdentityKey        string  `json:"identity_key"`
	SignedPrekey       string  `json:"signed_prekey"`
	SignedPrekeySig    string  `json:"signed_prekey_signature"`
	SignedPrekeyID     int     `json:"signed_prekey_id"`
	RegistrationID     int     `json:"registration_id"`
	OneTimePrekeys     []OneTimePrekeyUpload `json:"one_time_prekeys"`
}

// OneTimePrekeyUpload is a single one-time prekey to upload.
type OneTimePrekeyUpload struct {
	KeyID     int    `json:"key_id"`
	PublicKey string `json:"public_key"`
}
