package models

import (
	"github.com/scylladb/gocqlx/v2/table"
)

type File struct {
	ID             string `db:"id" json:"id"`
	UserID         string `db:"user_id" json:"user_id"`
	Type           string `db:"type" json:"type"`
	Filename       string `db:"filename" json:"filename"`
	StoredFilename string `db:"stored_filename" json:"stored_filename"`
	MimeType       string `db:"mime_type" json:"mime_type"`
	Size           int64  `db:"size" json:"size"`
	Width          *int   `db:"width" json:"width,omitempty"`
	Height         *int   `db:"height" json:"height,omitempty"`
	CreatedAt      int64  `db:"created_at" json:"created_at"`
	UpdatedAt      int64  `db:"updated_at" json:"updated_at"`
}

var FileMeta = table.Metadata{
	Name:    "files",
	Columns: []string{"id", "user_id", "type", "filename", "stored_filename", "mime_type", "size", "width", "height", "created_at", "updated_at"},
	PartKey: []string{"id"},
}

var FileTable = table.New(FileMeta)
