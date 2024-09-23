package types

type UserPresence struct {
	Online       bool   `json:"online"`
	Status       string `json:"status"`
	CustomStatus string `json:"custom_status"`
}
