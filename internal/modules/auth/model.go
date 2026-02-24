package auth

import "time"

type UserPresence struct {
	Online       bool   `db:"online" json:"online"`
	Status       string `db:"status" json:"status"`
	CustomStatus string `db:"custom_status" json:"custom_status"`
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
	DateOfBirth   time.Time    `db:"date_of_birth" json:"date_of_birth"`
	VerifiedEmail bool         `db:"verified_email" json:"verified_email"`
	AboutMe       string       `db:"about_me" json:"about_me"`
	AccentColor   string       `db:"accent_color" json:"accent_color"`
	Locale        string       `db:"locale" json:"locale"`
	Presence      UserPresence `db:"presence" json:"presence"`
	CreatedAt     time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time    `db:"updated_at" json:"updated_at"`
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
	Discriminator *int      `json:"discriminator,omitempty"`
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