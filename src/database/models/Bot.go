package models

import (
	"github.com/scylladb/gocqlx/v3/table"
)

var botTableMeta = table.Metadata{
	Name:    "bots",
	Columns: []string{"user_id", "owner_id", "bot_token", "public", "discoverable", "description", "terms_of_service_url", "privacy_policy_url"},
	PartKey: []string{"user_id"},
	SortKey: []string{"owner_id"},
}

var BotTable = table.New(botTableMeta)

type Bot struct {
	UserID            string `json:"user_id" db:"user_id"`
	OwnerID           string `json:"owner_id" db:"owner_id"`
	Token             string `json:"token" db:"bot_token"`
	Public            bool   `json:"public" db:"public"`
	Discoverable      bool   `json:"discoverable" db:"discoverable"`
	Description       string `json:"description" db:"description"`
	TermsOfServiceURL string `json:"terms_of_service_url" db:"terms_of_service_url"`
	PrivacyPolicyURL  string `json:"privacy_policy_url" db:"privacy_policy_url"`
}

func (b *Bot) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS bots (
            user_id bigint PRIMARY KEY,
			bot_token text,
            owner_id bigint,
            public boolean,
			discoverable boolean,
            description text,
            terms_of_service_url text,
            privacy_policy_url text
        );`,
	}
}
