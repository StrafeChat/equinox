package types

import "time"

type LoginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type RegisterBody struct {
	Email         string    `json:"email"`
	Username      string    `json:"username"`
	Discriminator string    `json:"discriminator"`
	DisplayName   string    `json:"display_name"`
	DateOfBirth   time.Time `json:"date_of_birth"`
	Password      string    `json:"password"`
	Locale        string    `json:"locale"`
}
