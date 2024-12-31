package models

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/scylladb/gocqlx/v3/table"
)

var relationshipMeta = table.Metadata{
	Name:    "relationships",
	Columns: []string{"id", "sender_id", "recipient_id", "created_at"},
	PartKey: []string{"id"},
}

var RelationshipTable = table.New(relationshipMeta)

type Relationship struct {
	Id          int64     `db:"id"`
	SenderId    int64     `db:"sender_id"`
	RecipientId int64     `db:"recipient_id"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

func (u *Relationship) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS relationships (
			id bigint PRIMARY KEY,
            sender_id bigint,
			recipient_id bigint, 
			created_at timestamp
		);`,
	}
}

// MarshalJSON implements custom JSON marshaling to convert IDs to strings
func (r *Relationship) MarshalJSON() ([]byte, error) {
	return json.Marshal(&struct {
		Id          string    `json:"id"`
		SenderId    string    `json:"sender_id"`
		RecipientId string    `json:"recipient_id"`
		CreatedAt   time.Time `json:"created_at"`
	}{
		Id:          strconv.FormatInt(r.Id, 10),
		SenderId:    strconv.FormatInt(r.SenderId, 10),
		RecipientId: strconv.FormatInt(r.RecipientId, 10),
		CreatedAt:   r.CreatedAt,
	})
}

// UnmarshalJSON implements custom JSON unmarshaling to convert string IDs to int64
func (r *Relationship) UnmarshalJSON(data []byte) error {
	aux := struct {
		Id          string    `json:"id"`
		SenderId    string    `json:"sender_id"`
		RecipientId string    `json:"recipient_id"`
		CreatedAt   time.Time `json:"created_at"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	
	var err error
	if r.Id, err = strconv.ParseInt(aux.Id, 10, 64); err != nil {
		return err
	}
	if r.SenderId, err = strconv.ParseInt(aux.SenderId, 10, 64); err != nil {
		return err
	}
	if r.RecipientId, err = strconv.ParseInt(aux.RecipientId, 10, 64); err != nil {
		return err
	}
	r.CreatedAt = aux.CreatedAt
	return nil
}
