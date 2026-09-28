package auth

import (
	"time"

	"github.com/gocql/gocql"
)

type UserPresence struct {
	Online       bool   `db:"online" json:"-"`
	Status       string `db:"status" json:"status"`
	CustomStatus string `db:"custom_status" json:"custom_status"`
}

func (u UserPresence) MarshalUDT(name string, info gocql.TypeInfo) ([]byte, error) {
	switch name {
	case "online":
		return gocql.Marshal(info, u.Online)
	case "status":
		return gocql.Marshal(info, u.Status)
	case "custom_status":
		return gocql.Marshal(info, u.CustomStatus)
	default:
		return nil, nil
	}
}

func (u *UserPresence) UnmarshalUDT(name string, info gocql.TypeInfo, data []byte) error {
	switch name {
	case "online":
		return gocql.Unmarshal(info, data, &u.Online)
	case "status":
		return gocql.Unmarshal(info, data, &u.Status)
	case "custom_status":
		return gocql.Unmarshal(info, data, &u.CustomStatus)
	default:
		return nil
	}
}

type User struct {
	ID            int64        `db:"id" json:"id"`
	Email         string       `db:"email" json:"email"`
	PasswordHash  string       `db:"password" json:"-"`
	Username      string       `db:"username" json:"username"`
	Discriminator int          `db:"discriminator" json:"discriminator"`
	DisplayName   string       `db:"display_name" json:"display_name"`
	Avatar        string       `db:"avatar" json:"avatar"`
	Banner        string       `db:"banner" json:"banner"`
	Bot           bool         `db:"bot" json:"bot"`
	Bots          []string     `db:"bots" json:"bots"`
	System        bool         `db:"system" json:"system"`
	Bio           string       `db:"bio" json:"bio"`
	Flags         int          `db:"flags" json:"flags"`
	Relationships []int64      `db:"relationships" json:"relationships"`
	Spaces        []int64      `db:"spaces" json:"spaces"`
	Blocks        []int64      `db:"blocks" json:"blocks,omitempty"`
	DateOfBirth   time.Time    `db:"date_of_birth" json:"date_of_birth"`
	VerifiedEmail bool         `db:"verified_email" json:"verified_email"`
	AboutMe       string       `db:"about_me" json:"about_me"`
	AccentColor   string       `db:"accent_color" json:"accent_color"`
	Locale        string       `db:"locale" json:"locale"`
	Presence      UserPresence `db:"presence" json:"presence"`
	CreatedAt     time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time    `db:"updated_at" json:"updated_at"`
	// Federation: set only on "shadow" rows - local copies of users whose home is another
	// instance. HomeDomain is that instance and RemoteID the id it knows them by; every
	// other column is a cached copy of their profile. Shadows have no email/password and
	// are absent from the email/username lookup tables (they can't log in here).
	HomeDomain string `db:"home_domain" json:"home_domain,omitempty"`
	RemoteID   *int64 `db:"remote_id" json:"-"`
}

// IsRemote reports whether this row is a shadow of a user on another instance.
func (u *User) IsRemote() bool {
	return u != nil && u.HomeDomain != "" && u.RemoteID != nil
}

// OriginID is the id the user's home instance knows them by (their own id for local users).
func (u *User) OriginID() int64 {
	if u.IsRemote() {
		return *u.RemoteID
	}
	return u.ID
}

// UserByRemote maps a federated identity (@remote_id:home_domain) to its local shadow row.
type UserByRemote struct {
	HomeDomain string `db:"home_domain"`
	RemoteID   int64  `db:"remote_id"`
	UserID     int64  `db:"user_id"`
}

type UserByEmail struct {
	Email  string `db:"email"`
	UserID int64  `db:"user_id"`
}

type UserByUsernameDiscriminator struct {
	Username      string `db:"username"`
	Discriminator int    `db:"discriminator"`
	UserID        int64  `db:"user_id"`
}

type RegisterInput struct {
	Email         string    `json:"email"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	DateOfBirth   time.Time `json:"date_of_birth"`
	Discriminator *int      `json:"discriminator"`
	// CaptchaToken is the challenge response from the client widget. Only looked at when
	// the instance has a captcha configured; ignored entirely otherwise.
	CaptchaToken string `json:"captcha_token"`
	// Invite is an instance invite code. Required only when the instance is invite-only
	// and this is not the very first account on it.
	Invite string `json:"invite"`
}

// Session is stored in sessions_by_user and sessions_by_token (token_hash is the raw hash).
type Session struct {
	UserID     int64     `db:"user_id"`
	SessionID  int64     `db:"session_id"`
	TokenHash  string    `db:"token_hash"`
	CreatedAt  time.Time `db:"created_at"`
	ExpiresAt  time.Time `db:"expires_at"`
	IPAddress  string    `db:"ip_address"`
	UserAgent  string    `db:"user_agent"`
	DeviceName string    `db:"device_name"`
	RevokedAt  time.Time `db:"revoked_at"`
}

// LoginInput is the request body for POST /auth/login.
type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}
