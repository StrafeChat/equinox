package discover

import (
	"errors"
	"time"
)

// Discover is the instance's directory: the spaces and bots whose managers asked to be
// found here, shown to every signed-in user once an instance administrator approved
// them. A listing is per instance - a space is listed on the instance that hosts it, a
// bot on the instance its application lives on - and adds nothing the space or bot does
// not already show except the blurb (tagline) and tags written for the directory.

const (
	KindSpace = "space"
	KindBot   = "bot"

	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDenied   = "denied"

	MaxTagline = 140
	MaxTags    = 5
	MaxTagLen  = 24
	MaxNote    = 500
	// MaxListings bounds what one directory page or review queue returns.
	MaxListings = 500

	// Instance audit actions (instance.AuditEntry.Action).
	AuditApprove = "discover_approve"
	AuditDeny    = "discover_deny"
	AuditRemove  = "discover_remove"

	// Instance audit target types; the space one matches instance.TargetSpace.
	targetSpace       = "space"
	targetApplication = "application"
)

var (
	ErrNotFound        = errors.New("not listed")
	ErrNotApproved     = errors.New("that is not listed on Discover")
	ErrInvalidKind     = errors.New("unknown listing kind")
	ErrInvalidStatus   = errors.New("status must be pending, approved or denied")
	ErrInvalidTagline  = errors.New("tagline must be one line of at most 140 characters")
	ErrInvalidTags     = errors.New("up to 5 tags of letters, digits, spaces and dashes, at most 24 characters each")
	ErrInvalidNote     = errors.New("note must be at most 500 characters")
	ErrInvalidDecision = errors.New("decision must be approve or deny")
	ErrNotListable     = errors.New("that cannot be listed on this instance")
	ErrNotAdmin        = errors.New("instance administrator required")
)

// Listing is one application to Discover and what became of it.
type Listing struct {
	Kind        string     `db:"kind" json:"kind"`
	ID          int64      `db:"id" json:"id,string"`
	Status      string     `db:"status" json:"status"`
	Tagline     string     `db:"tagline" json:"tagline"`
	Tags        []string   `db:"tags" json:"tags"`
	RequestedBy int64      `db:"requested_by" json:"requested_by,omitempty,string"`
	RequestedAt time.Time  `db:"requested_at" json:"requested_at"`
	ReviewedBy  int64      `db:"reviewed_by" json:"reviewed_by,omitempty,string"`
	ReviewedAt  *time.Time `db:"reviewed_at" json:"reviewed_at,omitempty"`
	// Note is the administrator's word to the applicant (why it was declined or removed).
	Note string `db:"note" json:"note,omitempty"`
	// FederateOptOut: the managers asked to be found on this instance only. Stored as the
	// exception because sharing is the default, so a listing from before the column existed
	// (null, i.e. false here) is shared without a backfill. The wire field is the positive
	// `federate`, which is what people reason about.
	FederateOptOut bool `db:"federate_opt_out" json:"-"`
	// Federate is the wire form of the above, the positive that people reason about. Filled
	// on the way out; a change comes in through ApplyInput, never from here.
	Federate bool `db:"-" json:"federate"`
}

// Federated reports whether this listing may be shown on other instances.
func (l *Listing) Federated() bool { return !l.FederateOptOut }

// ApplyInput is what an applicant writes for the directory. Federate is optional: left out
// it keeps whatever the listing already said, and a new listing is shared by default.
type ApplyInput struct {
	Tagline  string   `json:"tagline"`
	Tags     []string `json:"tags"`
	Federate *bool    `json:"federate,omitempty"`
}

// RemoteListing is a space another instance lists, as last fetched from it. The id is the
// origin's; this instance may hold no mirror of the space at all.
type RemoteListing struct {
	Domain      string    `db:"domain" json:"domain"`
	SpaceID     int64     `db:"space_id" json:"space_id,string"`
	Name        string    `db:"name" json:"name"`
	NameAcronym string    `db:"name_acronym" json:"name_acronym"`
	Icon        string    `db:"icon" json:"icon,omitempty"`
	Banner      string    `db:"banner" json:"banner,omitempty"`
	Description string    `db:"description" json:"description,omitempty"`
	Tagline     string    `db:"tagline" json:"tagline"`
	Tags        []string  `db:"tags" json:"tags"`
	MemberCount int       `db:"member_count" json:"member_count"`
	OnlineCount int       `db:"online_count" json:"online_count"`
	FetchedAt   time.Time `db:"fetched_at" json:"fetched_at"`
}

// MaxRemotePerPeer caps what one instance can put on this one's Discover page, so a peer
// cannot crowd out everything else by listing thousands of spaces.
const MaxRemotePerPeer = 100

// UserRef names a person on a card or in the review queue.
type UserRef struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Avatar      string `json:"avatar,omitempty"`
	Bot         bool   `json:"bot,omitempty"`
}

// SpaceCard is a listed space as the directory shows it. OriginDomain is set only for a
// space another instance hosts and lists; for those, ID is that instance's id for it.
type SpaceCard struct {
	OriginDomain string `json:"origin_domain,omitempty"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	NameAcronym  string `json:"name_acronym"`
	Icon         string `json:"icon,omitempty"`
	Banner       string `json:"banner,omitempty"`
	Description  string `json:"description,omitempty"`
	MemberCount  int    `json:"member_count"`
	OnlineCount  int    `json:"online_count"`
}

// BotCard is a listed bot: its application's public face and the bot account.
type BotCard struct {
	ApplicationID string   `json:"application_id"`
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Icon          string   `json:"icon,omitempty"`
	Bot           *UserRef `json:"bot,omitempty"`
}

// Entry is a listing with the thing it lists; the review queue also names the applicant.
type Entry struct {
	Listing
	Space           *SpaceCard `json:"space,omitempty"`
	Bot             *BotCard   `json:"bot,omitempty"`
	RequestedByUser *UserRef   `json:"requested_by_user,omitempty"`
}
