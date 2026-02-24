package relationships

import "time"

const (
	TypeNone            = 0
	TypeFriend          = 1
	TypeBlocked         = 2
	TypeIncomingRequest = 3
	TypeOutgoingRequest = 4
	TypeImplicit        = 5
	TypeSuggestion      = 6
)

type Relationship struct {
	ID             string                 `json:"id"`
	Type           int                    `json:"type"`
	User           map[string]interface{} `json:"user"`
	Nickname       *string                `json:"nickname"`
	IsSpamRequest  bool                   `json:"is_spam_request,omitempty"`
	StrangerRequest bool                  `json:"stranger_request,omitempty"`
	UserIgnored    bool                   `json:"user_ignored"`
	Since          *string                `json:"since,omitempty"`
}

// Request is a pending friend request.
type Request struct {
	FromUserID int64     `json:"from_user_id" db:"from_user_id"`
	ToUserID   int64     `json:"to_user_id" db:"to_user_id"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
}

// IncomingRow is stored in relationship_requests_by_recipient.
type IncomingRow struct {
	ToUserID   int64     `db:"to_user_id"`
	FromUserID int64     `db:"from_user_id"`
	CreatedAt  time.Time `db:"created_at"`
}

// OutgoingRow is stored in relationship_requests_by_sender.
type OutgoingRow struct {
	FromUserID int64     `db:"from_user_id"`
	ToUserID   int64     `db:"to_user_id"`
	CreatedAt  time.Time `db:"created_at"`
}
