package types

type RelationshipsPost struct {
	Username      string `json:"username"`
	Discriminator int    `json:"discriminator"`
}

type RelationshipsPutParams struct {
	ID string `params:"id"`
}

type RelationshipsDeleteParams struct {
	ID string `params:"id"`
}

type RelationshipEvent struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	SenderId  string `json:"sender_id"`
	RecipientId string `json:"recipient_id"`
	CreatedAt int64 `json:"created_at"`
}

const (
	RelationshipEventCreate = "RELATIONSHIP_CREATE"
	RelationshipEventAccept = "RELATIONSHIP_ACCEPT"
	RelationshipEventDelete = "RELATIONSHIP_DELETE"
)