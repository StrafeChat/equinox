package models

import (
	"github.com/scylladb/gocqlx/v2/table"
)

var botbyTokenTableMeta = table.Metadata{
	Name:    "bots_by_token",
	Columns: []string{"bot_token", "user_id"},
	PartKey: []string{"bot_token"},
}

var BotByTokenTable = table.New(botbyTokenTableMeta)

type BotByToken struct {
	Token  string `json:"token" db:"bot_token"`
	UserID string `json:"user_id" db:"user_id"`
}

func (b *BotByToken) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS bots_by_token (
            bot_token text PRIMARY KEY,
			user_id bigint
        );`,
	}
}
