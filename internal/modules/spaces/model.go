package spaces

import "time"

// EveryoneRoleName is the fixed display name for the built-in @everyone role (cannot rename/delete).
const EveryoneRoleName = "@everyone"

// Space is a Discord-style server.
type Space struct {
	ID                int64  `db:"id" json:"id"`
	Name              string `db:"name" json:"name"`
	NameAcronym       string `db:"name_acronym" json:"name_acronym"`
	Description       string `db:"description" json:"description"`
	Icon              string `db:"icon" json:"icon"`
	Banner            string `db:"banner" json:"banner"`
	OwnerID           int64  `db:"owner_id" json:"owner_id"`
	VerificationLevel int    `db:"verification_level" json:"verification_level"`
	// Official marks a space an instance admin has blessed as part of this instance. It is
	// instance-local (never set on a mirror of a remote space) and only the instance module
	// writes it, from the admin dashboard.
	Official              bool     `db:"official" json:"official"`
	DefaultMessageNotif   int      `db:"default_message_notifications" json:"default_message_notifications"`
	ExplicitContentFilter int      `db:"explicit_content_filter" json:"explicit_content_filter"`
	Features              []string `db:"features" json:"features"`
	AFKRoomID             *int64   `db:"afk_room_id" json:"afk_room_id,omitempty"`
	AFKTimeout            int      `db:"afk_timeout" json:"afk_timeout"`
	SystemRoomID          *int64   `db:"system_room_id" json:"system_room_id,omitempty"`
	SystemRoomFlags       int      `db:"system_room_flags" json:"system_room_flags"`
	// Birthdays: the text channel opted-in members are wished a happy birthday in, and an
	// optional custom message template (empty = the client's default greeting).
	BirthdayChannelID   *int64 `db:"birthday_channel_id" json:"birthday_channel_id,omitempty"`
	BirthdayMessage     string `db:"birthday_message" json:"birthday_message,omitempty"`
	RulesRoomID         *int64 `db:"rules_room_id" json:"rules_room_id,omitempty"`
	MaxPresences        int    `db:"max_presences" json:"max_presences"`
	MaxMembers          int    `db:"max_members" json:"max_members"`
	VanityURLCode       string `db:"vanity_url_code" json:"vanity_url_code"`
	PreferredLocale     string `db:"preferred_locale" json:"preferred_locale"`
	PublicUpdatesRoomID *int64 `db:"public_updates_room_id" json:"public_updates_room_id,omitempty"`
	MaxVideoRoomUsers   int    `db:"max_video_room_users" json:"max_video_room_users"`
	EveryoneRoleID      int64  `db:"everyone_role_id" json:"everyone_role_id,omitempty"`
	// Server widget: a public JSON document (GET /spaces/:id/widget.json) with the member
	// and online counts plus, when WidgetRoomID is set, an invite that never expires, for
	// embedding a "join us" card outside Strafe. WidgetInviteCode is that invite; it is
	// created lazily the first time the widget is served and never exposed on the space
	// payload itself (only through the widget document).
	WidgetEnabled    bool   `db:"widget_enabled" json:"widget_enabled"`
	WidgetRoomID     *int64 `db:"widget_room_id" json:"widget_room_id,omitempty"`
	WidgetInviteCode string `db:"widget_invite_code" json:"-"`
	// Light automod, enforced on send with VerificationLevel (see messages/automod.go):
	// which Automod* rules are on, and the mention cap the mass-mention rule uses (0 =
	// DefaultAutomodMentionLimit).
	AutomodFlags        int       `db:"automod_flags" json:"automod_flags"`
	AutomodMentionLimit int       `db:"automod_mention_limit" json:"automod_mention_limit"`
	CreatedAt           time.Time `db:"created_at" json:"created_at"`
	UpdatedAt           time.Time `db:"updated_at" json:"updated_at"`
	// Federation is set on a mirror of a space another instance hosts (see federation.go);
	// not stored, filled from the federation mapping on read.
	Federation *SpaceFederation `db:"-" json:"federation,omitempty"`
}

// System-room flags (spaces.system_room_flags): which server-generated notices are
// *suppressed* in the system room, so a zero value means "post everything".
const (
	SystemRoomFlagSuppressJoin  = 1 << 0
	SystemRoomFlagSuppressLeave = 1 << 1
	systemRoomFlagsAll          = SystemRoomFlagSuppressJoin | SystemRoomFlagSuppressLeave
)

// Default notification levels (spaces.default_message_notifications), Discord's values.
const (
	DefaultNotifAllMessages  = 0
	DefaultNotifOnlyMentions = 1
)

// Verification levels (spaces.verification_level): what a member who holds no role must
// satisfy before they can send messages in the space. Cumulative, like Discord's (there
// is no level 4: Strafe has no phone verification). Members with any role, and anyone with
// Manage Messages or Administrator, are exempt - they are the people the moderators vouch
// for, and raiders are not.
const (
	VerificationNone     = 0 // anyone may talk
	VerificationLow      = 1 // verified email address
	VerificationMedium   = 2 // + account older than VerificationAccountAge
	VerificationHigh     = 3 // + member of the space for VerificationMemberAge
	MaxVerificationLevel = VerificationHigh
)

// Automod rules (spaces.automod_flags). Content rules apply to plaintext channels only -
// the server cannot read an E2EE room; the mention cap applies to declared mentions there.
const (
	AutomodRepeatedMessages = 1 << 0 // the same text three times within a minute
	AutomodInviteLinks      = 1 << 1 // invite links to other spaces (and Discord servers)
	AutomodMassMentions     = 1 << 2 // more than AutomodMentionLimit distinct user/role mentions
	automodAll              = AutomodRepeatedMessages | AutomodInviteLinks | AutomodMassMentions

	DefaultAutomodMentionLimit = 5
	MaxAutomodMentionLimit     = 50
)

// SendPolicy is what sending into a space channel is gated on, read in one call by the
// messages module (SpaceChannelAuth.SendPolicy): the space's settings plus the sender's
// standing. JoinedAt is zero and HasRole false for a non-member; both are left unread when
// the space has neither a verification level nor automod on.
type SendPolicy struct {
	VerificationLevel   int
	AutomodFlags        int
	AutomodMentionLimit int
	JoinedAt            time.Time
	// HasRole: the member holds a role beyond @everyone (exempt from the verification level).
	HasRole bool
}

// Enforced reports whether anything in the policy can refuse a message.
func (p *SendPolicy) Enforced() bool { return p.VerificationLevel > 0 || p.AutomodFlags != 0 }

// AFKTimeouts are the accepted values for spaces.afk_timeout (seconds) - Discord's set.
var AFKTimeouts = map[int]bool{60: true, 300: true, 900: true, 1800: true, 3600: true}

// SpaceAuditEntry is one row of space_audit_log: who did what to which target, with the
// before/after values (Changes is a JSON object of field -> {old, new}) and an optional
// reason. Newest first by ID (snowflake).
type SpaceAuditEntry struct {
	SpaceID    int64     `db:"space_id"`
	ID         int64     `db:"id"`
	ActionType string    `db:"action_type"`
	UserID     int64     `db:"user_id"`
	TargetID   string    `db:"target_id"`
	Changes    string    `db:"changes"`
	Reason     string    `db:"reason"`
	CreatedAt  time.Time `db:"created_at"`
}

// SpaceRole is a permission-bearing role in a space (@everyone or custom).
type SpaceRole struct {
	SpaceID     int64  `db:"space_id" json:"-"`
	ID          int64  `db:"id" json:"id"`
	Name        string `db:"name" json:"name"`
	Permissions int64  `db:"permissions" json:"permissions"`
	Position    int    `db:"position" json:"position"`
	Color       int    `db:"color" json:"color"`
	Hoist       bool   `db:"hoist" json:"hoist"`
	Mentionable bool   `db:"mentionable" json:"mentionable"`
	// BotID marks a role a bot install created (the bot's user id): editable, but never
	// deletable or assignable to anyone else, and removed when the bot leaves.
	BotID     int64     `db:"bot_id" json:"bot_id,omitempty"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// IsManaged reports whether the role belongs to a bot (see BotID).
func (r *SpaceRole) IsManaged() bool { return r.BotID != 0 }

// SpaceRoomRoleOverride is a Discord-style allow/deny mask for one role in one room.
type SpaceRoomRoleOverride struct {
	SpaceID   int64     `db:"space_id"`
	RoomID    int64     `db:"room_id"`
	RoleID    int64     `db:"role_id"`
	Allow     int64     `db:"allow_mask"`
	Deny      int64     `db:"deny_mask"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// SpaceRoomUserOverride is a Discord-style allow/deny mask for one user in one room.
type SpaceRoomUserOverride struct {
	SpaceID   int64     `db:"space_id"`
	RoomID    int64     `db:"room_id"`
	UserID    int64     `db:"user_id"`
	Allow     int64     `db:"allow_mask"`
	Deny      int64     `db:"deny_mask"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// SpaceEmoji is a custom emoji uploaded to a space. Used in message text as
// <:name:id> (or <a:name:id> when animated); the image lives on Nebula at URL.
type SpaceEmoji struct {
	SpaceID   int64     `db:"space_id"`
	ID        int64     `db:"id"`
	Name      string    `db:"name"`
	ImageHash string    `db:"image_hash"`
	URL       string    `db:"url"`
	Animated  bool      `db:"animated"`
	CreatorID int64     `db:"creator_id"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// SpaceEmojiRef is the space_emoji_by_id row: enough for any client to render an emoji
// it sees in a message, regardless of whether it's a member of the home space.
type SpaceEmojiRef struct {
	ID       int64  `db:"id"`
	SpaceID  int64  `db:"space_id"`
	Name     string `db:"name"`
	URL      string `db:"url"`
	Animated bool   `db:"animated"`
}

// MaxEmojisPerSpace caps a space's custom emoji.
const MaxEmojisPerSpace = 50

// SpaceMember is a user's membership in a space.
type SpaceMember struct {
	SpaceID  int64     `db:"space_id"`
	UserID   int64     `db:"user_id"`
	RoleIDs  []int64   `db:"role_ids"`
	Nickname string    `db:"nickname"`
	JoinedAt time.Time `db:"joined_at"`
}

// SpaceMemberRow is spaces_by_user row.
type SpaceMemberRow struct {
	UserID   int64     `db:"user_id"`
	SpaceID  int64     `db:"space_id"`
	JoinedAt time.Time `db:"joined_at"`
}

// SpaceBan blocks a user from rejoining a space via invite. Maps to the space_bans table
// (present in migrations/013_spaces.cql since the schema was first written, but never
// wired up in code until now).
type SpaceBan struct {
	SpaceID   int64     `db:"space_id" json:"-"`
	UserID    int64     `db:"user_id" json:"user_id"`
	Reason    string    `db:"reason" json:"reason,omitempty"`
	BannedBy  int64     `db:"banned_by" json:"banned_by"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// CreateSpaceInput is the request body for creating a space.
type CreateSpaceInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
}

// PatchSpaceInput patches space settings. Omitted fields are unchanged. Room ids are
// string snowflakes; an empty string clears the setting (JSON null can't be told apart
// from an omitted field once decoded into a pointer).
type PatchSpaceInput struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	// System messages: the text room that receives join/leave notices, and which of
	// those are suppressed (SystemRoomFlag*).
	SystemRoomID    *string `json:"system_room_id,omitempty"`
	SystemRoomFlags *int    `json:"system_room_flags,omitempty"`
	// DefaultNotif* - what members get notified for unless they override it.
	DefaultMessageNotif *int `json:"default_message_notifications,omitempty"`
	// Voice room idle members are moved to after AFKTimeout seconds (one of AFKTimeouts).
	AFKRoomID  *string `json:"afk_room_id,omitempty"`
	AFKTimeout *int    `json:"afk_timeout,omitempty"`
	// Server widget (see Space.WidgetEnabled).
	WidgetEnabled *bool   `json:"widget_enabled,omitempty"`
	WidgetRoomID  *string `json:"widget_room_id,omitempty"`
	// Birthdays: the announcement channel (empty string clears it) and an optional message
	// template.
	BirthdayChannelID *string `json:"birthday_channel_id,omitempty"`
	BirthdayMessage   *string `json:"birthday_message,omitempty"`
	// Moderation: the verification level (Verification*) and automod (Automod* flags plus
	// the mention cap, 0 for the default).
	VerificationLevel   *int `json:"verification_level,omitempty"`
	AutomodFlags        *int `json:"automod_flags,omitempty"`
	AutomodMentionLimit *int `json:"automod_mention_limit,omitempty"`
}

// SpaceInvite is an invite link/code for joining a space. MaxUses 0 = unlimited;
// ExpiresAt nil = never. Written to both space_invites (per-space listing) and
// space_invite_by_code (the join lookup).
type SpaceInvite struct {
	Code        string     `db:"code" json:"code"`
	SpaceID     int64      `db:"space_id" json:"space_id"`
	InviterID   int64      `db:"inviter_id" json:"inviter_id"`
	MaxUses     int        `db:"max_uses" json:"max_uses"`
	CurrentUses int        `db:"current_uses" json:"current_uses"`
	ExpiresAt   *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
}

// Expired reports whether the invite's expiry has passed.
func (i *SpaceInvite) Expired(now time.Time) bool {
	return i.ExpiresAt != nil && !i.ExpiresAt.After(now)
}

// Exhausted reports whether the invite has hit its use cap.
func (i *SpaceInvite) Exhausted() bool {
	return i.MaxUses > 0 && i.CurrentUses >= i.MaxUses
}

// CreateInviteInput is the optional body of POST /spaces/:id/invites. Zero values mean
// "never expires" / "unlimited uses".
type CreateInviteInput struct {
	MaxAgeSeconds int `json:"max_age_seconds,omitempty"`
	MaxUses       int `json:"max_uses,omitempty"`
}

const (
	MaxInviteAgeSeconds = 30 * 24 * 3600
	MaxInviteUses       = 1000
)

// CreateSpaceRoleInput creates a custom role (not @everyone).
type CreateSpaceRoleInput struct {
	Name        string `json:"name"`
	Permissions int64  `json:"permissions"`
	Color       int    `json:"color,omitempty"`
	Hoist       bool   `json:"hoist,omitempty"`
	Mentionable bool   `json:"mentionable,omitempty"`
}

// UpdateSpaceRoleInput patches a role (cannot rename @everyone).
type UpdateSpaceRoleInput struct {
	Name        *string `json:"name,omitempty"`
	Permissions *int64  `json:"permissions,omitempty"`
	Position    *int    `json:"position,omitempty"`
	Color       *int    `json:"color,omitempty"`
	Hoist       *bool   `json:"hoist,omitempty"`
	Mentionable *bool   `json:"mentionable,omitempty"`
}

// PutRoomRoleOverrideInput sets channel overrides for a role in a room.
type PutRoomRoleOverrideInput struct {
	Allow int64 `json:"allow"`
	Deny  int64 `json:"deny"`
}

// PutRoomUserOverrideInput sets channel overrides for a user in a room.
type PutRoomUserOverrideInput struct {
	Allow int64 `json:"allow"`
	Deny  int64 `json:"deny"`
}

// UpdateRoomInput is a partial update: every field is optional and only the ones present
// in the request body are written, so a client can flip one setting (e.g. e2ee_enabled)
// without having to echo back every other field it isn't changing.
type UpdateRoomInput struct {
	Name            *string `json:"name,omitempty"`
	Topic           *string `json:"topic,omitempty"`
	SlowmodeSeconds *int    `json:"slowmode_seconds,omitempty"`
	// E2EEEnabled turns end-to-end encryption on or off for a text room. Off by default
	// for space rooms (see createDefaultSpaceRooms); when on, the server only ever stores
	// Megolm ciphertext for the room and clients must fan the room key out to every
	// current space member - see messages.Service.roomE2EEOff for the storage rule.
	E2EEEnabled *bool `json:"e2ee_enabled,omitempty"`
	// Voice rooms only: how many people may be connected at once (0 = unlimited, max 99)
	// and the audio bitrate in bits per second (8000-384000; 0 = the default 64 kbps).
	UserLimit *int `json:"user_limit,omitempty"`
	Bitrate   *int `json:"bitrate,omitempty"`
	// PermissionsSynced syncs a text/voice channel to its parent section's permission
	// overrides (Discord category sync), or unsyncs it. Syncing clears the channel's own
	// overrides; unsyncing copies the section's current overrides down so nothing changes.
	PermissionsSynced *bool `json:"permissions_synced,omitempty"`
}

// CreateRoomInput is the service-layer input (parent as int64). HTTP JSON is decoded in the handler (string snowflake parent_id).
type CreateRoomInput struct {
	Name     string
	Type     int
	ParentID *int64
	// E2EEEnabled: nil or false = plaintext (the default for space rooms); true = E2EE from
	// the first message. Only honored for text rooms.
	E2EEEnabled *bool
	// Voice rooms only (see UpdateRoomInput).
	UserLimit *int
	Bitrate   *int
}

// ReorderRoomsInput reorders either all sections (scope "sections") or all text/voice channels in one parent group (scope "channels").
type ReorderRoomsInput struct {
	Scope           string   `json:"scope"`                       // "sections" | "channels"
	ParentSectionID *string  `json:"parent_section_id,omitempty"` // for "channels": omit or null = top-level (parent_id null); else snowflake of section
	RoomIDs         []string `json:"room_ids"`                    // must be a permutation of the server-side group
}

// MoveChannelInput moves a text/voice channel to another parent group (or top-level) in one step.
// Omitted or empty parent_section_id means top-level; omitted or empty before_room_id means append at end of the target group.
type MoveChannelInput struct {
	ChannelID       string  `json:"channel_id"`
	ParentSectionID *string `json:"parent_section_id,omitempty"`
	BeforeRoomID    *string `json:"before_room_id,omitempty"`
}
