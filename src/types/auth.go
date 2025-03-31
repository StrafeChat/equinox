package types

import "time"

type LoginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type RegisterBody struct {
	Email         string    `json:"email"`
	Username      string    `json:"username"`
	Discriminator int       `json:"discriminator"`
	DisplayName   string    `json:"display_name"`
	DateOfBirth   time.Time `json:"date_of_birth"`
	Password      string    `json:"password"`
	Locale        string    `json:"locale"`
}

type PasswordResetRequestBody struct {
	Email string `json:"email"`
}

type PasswordResetVerifyBody struct {
	UserId string `json:"user_id"` // Now optional
	Code   string `json:"code"`
}

type PasswordResetCompleteBody struct {
	UserId      string `json:"user_id"` // Now optional
	Code        string `json:"code"`
	NewPassword string `json:"new_password"`
}
