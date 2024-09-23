package database

import (
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/types"
)

type UserFlags uint32

const (
	FOUNDER         UserFlags = 1 << iota // 1
	PLATFORM_ADMIN                        // 2
	PLATFORM_MOD                          // 4
	CONTRIBUTOR                           // 8
	TRANSLATOR                            // 16
	BUG_REPORTER                          // 32
	EARLY_SUPPORTER                       // 64
	SUPPORTER                             // 128
	EARLY_ADOPTER                         // 256
	BOT_DEVELOPER                         // 512
)

type User struct {
	ID            string             `json:"id"`
	Email         string             `json:"email"`
	Password      string             `json:"password"`
	Username      string             `json:"username"`
	Discriminator string             `json:"discriminator"`
	DisplayName   string             `json:"display_name"`
	Avatar        string             `json:"avatar"`
	Banner        string             `json:"banner"`
	Bot           bool               `json:"bot"`
	System        bool               `json:"system"`
	Bio           string             `json:"bio"`
	Flags         UserFlags          `json:"flags"`
	Dob           time.Time          `json:"dob"`
	AboutMe       string             `json:"about_me"`
	AccentColor   string             `json:"accent_color"`
	Locale        string             `json:"locale"`
	Presence      types.UserPresence `json:"presence"`
}

func (u *User) Save() error {

	return database.Session.Query(`INSERT INTO users (id, name, email, password) 
		VALUES (?, ?, ?, ?) IF NOT EXISTS`,
		u.ID, u.Username, u.Email, u.Password).Exec()
}
