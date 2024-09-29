package models

import (
	"github.com/scylladb/gocqlx/v3/table"
)

var BotTable = table.Metadata{
	Name:    "bots",
	Columns: []string{"user_id", "owner_id", "public", "description", "terms_of_service_url", "privacy_policy_url"},
	PartKey: []string{"user_id"},
	SortKey: []string{"owner_id"},
}

type Bot struct {
	UserID            string `json:"user_id" db:"user_id"`
	OwnerID           string `json:"owner_id" db:"owner_id"`
	Public            bool   `json:"public" db:"public"`
	Description       string `json:"description" db:"description"`
	TermsOfServiceURL string `json:"terms_of_service_url" db:"terms_of_service_url"`
	PrivacyPolicyURL  string `json:"privacy_policy_url" db:"privacy_policy_url"`
}

func (b *Bot) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS bots (
            user_id text PRIMARY KEY,
            owner_id text,
            public boolean,
            description text,
            terms_of_service_url text,
            privacy_policy_url text
        );`,
	}
}
