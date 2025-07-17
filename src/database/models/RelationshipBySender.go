package models

import "github.com/scylladb/gocqlx/v2/table"

var relationshipBySenderMeta = table.Metadata{
	Name:    "relationships_by_sender",
	Columns: []string{"sender_id", "recipient_id", "id"},
	PartKey: []string{"sender_id"},
	SortKey: []string{"id"},
}

var RelationshipBySenderTable = table.New(relationshipBySenderMeta)

type RelationshipBySender struct {
	SenderId    string `db:"sender_id" json:"sender_id"`
	RecipientId string `db:"recipient_id" json:"recipient_id"`
	Id          string `db:"id" json:"id"`
}

func (u *RelationshipBySender) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS relationships_by_sender (
			sender_id bigint,
            recipient_id bigint,
			id bigint,
			PRIMARY KEY (sender_id, id)
		);`,
	}
}
