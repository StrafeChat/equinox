package models

import (
	"github.com/scylladb/gocqlx/v2/table"
)

// CustomEmoji represents a custom emoji for a space
type CustomEmoji struct {
	ID        string `db:"id" json:"id"`
	SpaceID   string `db:"space_id" json:"space_id"`
	Shortcode string `db:"shortcode" json:"shortcode"`
	FileID    string `db:"file_id" json:"file_id"`
	Name      string `db:"name" json:"name"`
	CreatedBy string `db:"created_by" json:"created_by"`
	CreatedAt string `db:"created_at" json:"created_at"`
	UpdatedAt string `db:"updated_at" json:"updated_at"`
}

// CustomEmojiBySpace represents custom emojis ordered by creation date for efficient listing
type CustomEmojiBySpace struct {
	SpaceID   string `db:"space_id" json:"space_id"`
	CreatedAt string `db:"created_at" json:"created_at"`
	ID        string `db:"id" json:"id"`
	Shortcode string `db:"shortcode" json:"shortcode"`
	FileID    string `db:"file_id" json:"file_id"`
	Name      string `db:"name" json:"name"`
	CreatedBy string `db:"created_by" json:"created_by"`
}

// ToMap converts CustomEmoji to map for database operations
func (ce *CustomEmoji) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"id":         ce.ID,
		"space_id":   ce.SpaceID,
		"shortcode":  ce.Shortcode,
		"file_id":    ce.FileID,
		"name":       ce.Name,
		"created_by": ce.CreatedBy,
		"created_at": ce.CreatedAt,
		"updated_at": ce.UpdatedAt,
	}
}

// ToMap converts CustomEmojiBySpace to map for database operations
func (cebs *CustomEmojiBySpace) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"space_id":   cebs.SpaceID,
		"created_at": cebs.CreatedAt,
		"id":         cebs.ID,
		"shortcode":  cebs.Shortcode,
		"file_id":    cebs.FileID,
		"name":       cebs.Name,
		"created_by": cebs.CreatedBy,
	}
}

// Table metadata for custom_emojis table
var CustomEmojiMeta = table.Metadata{
	Name:    "custom_emojis",
	Columns: []string{"id", "space_id", "shortcode", "file_id", "name", "created_by", "created_at", "updated_at"},
	PartKey: []string{"id"},
	SortKey: []string{},
}

// Table metadata for custom_emojis_by_space table
var CustomEmojiBySpaceMeta = table.Metadata{
	Name:    "custom_emojis_by_space",
	Columns: []string{"space_id", "created_at", "id", "shortcode", "file_id", "name", "created_by"},
	PartKey: []string{"space_id"},
	SortKey: []string{"created_at", "id"},
}

// Table instances
var CustomEmojiTable = table.New(CustomEmojiMeta)
var CustomEmojiBySpaceTable = table.New(CustomEmojiBySpaceMeta)

// SchemaDefinition returns the CREATE TABLE statement for custom_emojis
func (ce *CustomEmoji) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS custom_emojis (
			id text PRIMARY KEY,
			space_id text,
			shortcode text,
			file_id text,
			name text,
			created_by text,
			created_at text,
			updated_at text
		)`,
	}
}

// SchemaDefinition returns the CREATE TABLE statement for custom_emojis_by_space
func (cebs *CustomEmojiBySpace) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS custom_emojis_by_space (
			space_id text,
			created_at text,
			id text,
			shortcode text,
			file_id text,
			name text,
			created_by text,
			PRIMARY KEY (space_id, created_at, id)
		) WITH CLUSTERING ORDER BY (created_at DESC, id ASC)`,
	}
}
