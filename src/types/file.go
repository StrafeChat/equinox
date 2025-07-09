package types

// FileType represents the type of file
type FileType string

const (
	UserAvatar FileType = "user_avatar"
	UserBanner FileType = "user_banner"
	RoomIcon   FileType = "room_icon"
)

// FileMetadata represents file metadata stored in the database
type FileMetadata struct {
	ID        string   `json:"id"`
	UserID    string   `json:"user_id"`
	Type      FileType `json:"type"`
	Filename  string   `json:"filename"`
	MimeType  string   `json:"mime_type"`
	Size      int64    `json:"size"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
}

// Attachment represents a message attachment
type Attachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
	URL      string `json:"url"`
}
