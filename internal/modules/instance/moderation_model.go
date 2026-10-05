package instance

import (
	"errors"
	"time"
)

// Report targets and reasons. Reasons are a closed set so the queue can be filtered and
// counted; "other" plus free text covers the rest.
const (
	TargetUser        = "user"
	TargetSpace       = "space"
	TargetApplication = "application"

	ReportOpen      = "open"
	ReportResolved  = "resolved"
	ReportDismissed = "dismissed"

	// What resolving a report did. Recorded on the report so the queue's history reads
	// without cross-referencing the audit log.
	ResolutionNone         = "none"
	ResolutionBanned       = "banned"
	ResolutionSpaceRemoved = "space_removed"

	// Instance audit actions. Kept distinct from the space audit log's vocabulary.
	AuditUserBan       = "user_ban"
	AuditUserUnban     = "user_unban"
	AuditSpaceTakedown = "space_takedown"
	AuditSpaceOfficial = "space_official"
	AuditReportResolve = "report_resolve"
	AuditReportDismiss = "report_dismiss"
	AuditInviteCreate  = "invite_create"
	AuditInviteRevoke  = "invite_revoke"
	AuditUserBadges    = "user_badges"
	AuditUserRecovery  = "user_recovery_regen"
	// IP bans have no numeric target: the entry's target type is TargetIP, its target id
	// 0 and the CIDR leads the reason field ("10.0.0.0/8: spam farm").
	AuditIPBan   = "ip_ban"
	AuditIPUnban = "ip_unban"
	TargetIP     = "ip"

	MaxReportDetails = 2000
	MaxBanReason     = 500
	MaxBanAgeSeconds = 365 * 24 * 60 * 60
	// The narrowest prefix an IP ban may cover: /8 for IPv4 (a hosting provider's whole
	// allocation at most) and /32 for IPv6 (one provider's customer block). Anything
	// wider is a typo that would lock the instance's own users out.
	MinIPv4BanPrefix = 8
	MinIPv6BanPrefix = 32
	// A report queue page. The admin reads newest first; nobody pages through thousands.
	ReportPageSize = 100
	AuditPageSize  = 200
	SearchLimit    = 25
)

var ReportReasons = []string{"spam", "harassment", "hate", "sexual", "violence", "illegal", "impersonation", "other"}

var (
	ErrUserNotFound        = errors.New("user not found")
	ErrSpaceNotFound       = errors.New("space not found")
	ErrCannotBanSelf       = errors.New("you cannot ban yourself")
	ErrCannotBanRemote     = errors.New("that account lives on another instance")
	ErrRecoveryUnavailable = errors.New("recovery codes are not available on this instance")
	ErrAlreadyBanned       = errors.New("that account is already banned")
	ErrNotBanned           = errors.New("that account is not banned")
	ErrInvalidBan          = errors.New("ban reason must be at most 500 characters and expiry at most a year")
	ErrInvalidCIDR         = errors.New("enter an IP address or a CIDR range such as 203.0.113.0/24")
	ErrCIDRTooWide         = errors.New("that range is too wide to ban: use at least a /8 (IPv4) or /32 (IPv6)")
	ErrIPBanExists         = errors.New("that address or range is already banned")
	ErrIPBanNotFound       = errors.New("that address or range is not banned")
	ErrReportNotFound      = errors.New("report not found")
	ErrReportClosed        = errors.New("that report has already been handled")
	ErrInvalidReport       = errors.New("invalid report")
	ErrReportSelf          = errors.New("you cannot report yourself")
	ErrDuplicateReport     = errors.New("you already have an open report about this")
	ErrInvalidAction       = errors.New("unknown resolution action")
	ErrInvalidQuery        = errors.New("search for an id, an email, name#0001 or a username")
	ErrInvalidBadges       = errors.New("unknown badge flag")
)

// Ban keeps an account off the instance: its sessions are revoked when it is written, and
// login refuses while it stands. ExpiresAt nil means until lifted.
type Ban struct {
	UserID    int64      `db:"user_id" json:"user_id,string"`
	BannedBy  int64      `db:"banned_by" json:"banned_by,string"`
	Reason    string     `db:"reason" json:"reason"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	ExpiresAt *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	// BannedIPs lists the ranges BanInput.BanIPs added alongside; only on the response to
	// the ban itself, never stored.
	BannedIPs []string `db:"-" json:"banned_ips,omitempty"`
}

func (b *Ban) Expired(now time.Time) bool {
	return b.ExpiresAt != nil && !b.ExpiresAt.After(now)
}

// IPBan keeps a network off the instance: nothing under CIDR may register or sign in while
// it stands. CIDR is canonical (net.IPNet.String() of the parsed range, so "1.2.3.4" is
// stored as "1.2.3.4/32" and a host-bit-laden "10.1.2.3/8" as "10.0.0.0/8"). ExpiresAt nil
// means until lifted.
type IPBan struct {
	CIDR      string     `db:"cidr" json:"cidr"`
	BannedBy  int64      `db:"banned_by" json:"banned_by,string"`
	Reason    string     `db:"reason" json:"reason"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	ExpiresAt *time.Time `db:"expires_at" json:"expires_at,omitempty"`
}

func (b *IPBan) Expired(now time.Time) bool {
	return b.ExpiresAt != nil && !b.ExpiresAt.After(now)
}

type IPBanInput struct {
	// CIDR is an address ("203.0.113.9", "2001:db8::1") or a range ("203.0.113.0/24").
	CIDR          string `json:"cidr"`
	Reason        string `json:"reason"`
	MaxAgeSeconds int    `json:"max_age_seconds"`
}

// Report is one user's complaint about a user or a space. RoomID/MessageID are evidence
// the reporter pointed at; the server never copies message content into the report -
// an encrypted room's text is not the server's to read, and a plaintext room's can be
// fetched by id while it exists.
type Report struct {
	ID             int64      `db:"id" json:"id,string"`
	ReporterID     int64      `db:"reporter_id" json:"reporter_id,string"`
	TargetType     string     `db:"target_type" json:"target_type"`
	TargetID       int64      `db:"target_id" json:"target_id,string"`
	SpaceID        *int64     `db:"space_id" json:"space_id,omitempty,string"`
	RoomID         *int64     `db:"room_id" json:"room_id,omitempty,string"`
	MessageID      *int64     `db:"message_id" json:"message_id,omitempty,string"`
	Reason         string     `db:"reason" json:"reason"`
	Details        string     `db:"details" json:"details"`
	Status         string     `db:"status" json:"status"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	ResolvedBy     *int64     `db:"resolved_by" json:"resolved_by,omitempty,string"`
	ResolvedAt     *time.Time `db:"resolved_at" json:"resolved_at,omitempty"`
	Resolution     string     `db:"resolution" json:"resolution,omitempty"`
	ResolutionNote string     `db:"resolution_note" json:"resolution_note,omitempty"`
}

// ReportSummary is the reports_by_target row: enough to list and count, not the full text.
type ReportSummary struct {
	TargetType string    `db:"target_type" json:"target_type"`
	TargetID   int64     `db:"target_id" json:"target_id,string"`
	ID         int64     `db:"id" json:"id,string"`
	ReporterID int64     `db:"reporter_id" json:"reporter_id,string"`
	Status     string    `db:"status" json:"status"`
	Reason     string    `db:"reason" json:"reason"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type AuditEntry struct {
	ID         int64     `db:"id" json:"id,string"`
	ActorID    int64     `db:"actor_id" json:"actor_id,string"`
	Action     string    `db:"action" json:"action"`
	TargetType string    `db:"target_type" json:"target_type"`
	TargetID   int64     `db:"target_id" json:"target_id,string"`
	Reason     string    `db:"reason" json:"reason,omitempty"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type CreateReportInput struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	SpaceID    string `json:"space_id"`
	RoomID     string `json:"room_id"`
	MessageID  string `json:"message_id"`
	Reason     string `json:"reason"`
	Details    string `json:"details"`
}

type BanInput struct {
	Reason        string `json:"reason"`
	MaxAgeSeconds int    `json:"max_age_seconds"`
	// BanIPs also bans the public addresses of the account's live sessions, for the same
	// reason and the same length, so the person cannot simply register again.
	BanIPs bool `json:"ban_ips"`
}

// ResolveInput closes a report. Action is one of dismiss, resolve, ban_user (user
// reports) or remove_space (space reports); the last two do the thing and then close.
type ResolveInput struct {
	Action string `json:"action"`
	Note   string `json:"note"`
	// For ban_user: how long. Zero means until lifted.
	MaxAgeSeconds int `json:"max_age_seconds"`
}

func validReason(r string) bool {
	for _, ok := range ReportReasons {
		if r == ok {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------- federation policy ----

const (
	PolicyKindAllow = "allow"
	PolicyKindBlock = "block"

	// Audit action + target for an allow/block-list change.
	AuditFederationPolicy = "federation_policy"
	TargetFederation      = "federation"
)

var (
	ErrFederationDisabled = errors.New("federation is not enabled on this instance")
	ErrInvalidPolicyKind  = errors.New("policy must be allow or block")
	ErrInvalidPeerDomain  = errors.New("that is not a valid instance domain")
)

// PeerPolicyEntry is one admin-managed allow/block rule for a peer instance domain, stored in
// federation_peer_policy and merged into IsAllowedPeer via the config runtime lists.
type PeerPolicyEntry struct {
	Domain    string    `db:"domain" json:"domain"`
	Kind      string    `db:"kind" json:"kind"`
	AddedBy   int64     `db:"added_by" json:"-"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// FederationPolicyView is the dashboard payload: the editable runtime entries plus the
// read-only static env lists (FEDERATION_ALLOWLIST/BLOCKLIST) and this instance's own domain.
type FederationPolicyView struct {
	Enabled  bool
	Domain   string
	Entries  []PeerPolicyEntry
	EnvAllow []string
	EnvBlock []string
}

// PeerPolicyInput is the body of POST /instance/federation/policy.
type PeerPolicyInput struct {
	Domain string `json:"domain"`
	Kind   string `json:"kind"`
}
