package models

import "github.com/scylladb/gocqlx/v3/table"

var relationshipByRecipientMeta = table.Metadata{
	Name:    "relationships_by_recipient",
	Columns: []string{"recipient_id", "sender_id", "id"},
	PartKey: []string{"recipient_id"},
	SortKey: []string{"id"},
}

var RelationshipByRecipientTable = table.New(relationshipByRecipientMeta)

type RelationshipByRecipient struct {
	RecipientId string `db:"recipient_id" json:"recipient_id"`
	SenderId    string `db:"sender_id" json:"sender_id"`
	Id          string `db:"id" json:"id"`
}

func (u *RelationshipByRecipient) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS relationships_by_recipient (
			recipient_id bigint,
            sender_id bigint,
			id bigint,
			PRIMARY KEY (recipient_id, id)
		);`,
	}
}
