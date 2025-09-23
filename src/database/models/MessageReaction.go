package models

import (
	"github.com/scylladb/gocqlx/v2/table"
)

// MessageReaction represents a reaction to a message
type MessageReaction struct {
	MessageID string `db:"message_id" json:"message_id"`
	UserID    string `db:"user_id" json:"user_id"`
	Emoji     string `db:"emoji" json:"emoji"`
	CreatedAt string `db:"created_at" json:"created_at"`
}

// MessageReactionByMessage represents reactions grouped by message
type MessageReactionByMessage struct {
	MessageID string `db:"message_id" json:"message_id"`
	Emoji     string `db:"emoji" json:"emoji"`
	UserID    string `db:"user_id" json:"user_id"`
	CreatedAt string `db:"created_at" json:"created_at"`
}

// MessageReactionCount represents reaction counts for a message
type MessageReactionCount struct {
	MessageID string   `db:"message_id" json:"message_id"`
	Emoji     string   `db:"emoji" json:"emoji"`
	Count     int      `db:"count" json:"count"`
	Users     []string `db:"users" json:"users"`
}

// ToMap converts MessageReaction to map for database operations
func (mr *MessageReaction) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"message_id": mr.MessageID,
		"user_id":    mr.UserID,
		"emoji":      mr.Emoji,
		"created_at": mr.CreatedAt,
	}
}

// ToMap converts MessageReactionByMessage to map for database operations
func (mrbm *MessageReactionByMessage) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"message_id": mrbm.MessageID,
		"emoji":      mrbm.Emoji,
		"user_id":    mrbm.UserID,
		"created_at": mrbm.CreatedAt,
	}
}

// ToMap converts MessageReactionCount to map for database operations
func (mrc *MessageReactionCount) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"message_id": mrc.MessageID,
		"emoji":      mrc.Emoji,
		"count":      mrc.Count,
		"users":      mrc.Users,
	}
}

var MessageReactionMeta = table.Metadata{
	Name:    "message_reactions",
	Columns: []string{"message_id", "user_id", "emoji", "created_at"},
	PartKey: []string{"message_id", "user_id", "emoji"},
	SortKey: []string{},
}

var MessageReactionByMessageMeta = table.Metadata{
	Name:    "message_reactions_by_message",
	Columns: []string{"message_id", "emoji", "user_id", "created_at"},
	PartKey: []string{"message_id"},
	SortKey: []string{"emoji", "user_id"},
}

var MessageReactionCountMeta = table.Metadata{
	Name:    "message_reaction_counts",
	Columns: []string{"message_id", "emoji", "count", "users"},
	PartKey: []string{"message_id", "emoji"},
	SortKey: []string{},
}

var MessageReactionTable = table.New(MessageReactionMeta)
var MessageReactionByMessageTable = table.New(MessageReactionByMessageMeta)
var MessageReactionCountTable = table.New(MessageReactionCountMeta)

// SchemaDefinition returns the CREATE TABLE statement for message_reactions
func (mr *MessageReaction) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS message_reactions (
			message_id text,
			user_id text,
			emoji text,
			created_at text,
			PRIMARY KEY ((message_id, user_id, emoji))
		)`,
	}
}

// SchemaDefinition returns the CREATE TABLE statement for message_reactions_by_message
func (mrbm *MessageReactionByMessage) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS message_reactions_by_message (
			message_id text,
			emoji text,
			user_id text,
			created_at text,
			PRIMARY KEY (message_id, emoji, user_id)
		)`,
	}
}

// SchemaDefinition returns the CREATE TABLE statement for message_reaction_counts
func (mrc *MessageReactionCount) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS message_reaction_counts (
			message_id text,
			emoji text,
			count int,
			users list<text>,
			PRIMARY KEY ((message_id, emoji))
		)`,
	}
}