package spaces

import "time"

// EveryoneRoleName is the fixed display name for the built-in @everyone role (cannot rename/delete).
const EveryoneRoleName = "@everyone"

// Space is a Discord-style server.
type Space struct {
	ID                      int64     `db:"id" json:"id"`
	Name                    string    `db:"name" json:"name"`
	NameAcronym             string    `db:"name_acronym" json:"name_acronym"`
	Description             string    `db:"description" json:"description"`
	Icon                    string    `db:"icon" json:"icon"`
	Banner                  string    `db:"banner" json:"banner"`
	OwnerID                 int64     `db:"owner_id" json:"owner_id"`
	VerificationLevel       int       `db:"verification_level" json:"verification_level"`
	DefaultMessageNotif     int       `db:"default_message_notifications" json:"default_message_notifications"`
	ExplicitContentFilter   int       `db:"explicit_content_filter" json:"explicit_content_filter"`
	Features                []string  `db:"features" json:"features"`
	AFKRoomID               *int64    `db:"afk_room_id" json:"afk_room_id,omitempty"`
	AFKTimeout              int       `db:"afk_timeout" json:"afk_timeout"`
	SystemRoomID            *int64    `db:"system_room_id" json:"system_room_id,omitempty"`
	SystemRoomFlags         int       `db:"system_room_flags" json:"system_room_flags"`
	RulesRoomID             *int64    `db:"rules_room_id" json:"rules_room_id,omitempty"`
	MaxPresences            int       `db:"max_presences" json:"max_presences"`
	MaxMembers              int       `db:"max_members" json:"max_members"`
	VanityURLCode           string    `db:"vanity_url_code" json:"vanity_url_code"`
	PreferredLocale         string    `db:"preferred_locale" json:"preferred_locale"`
	PublicUpdatesRoomID     *int64    `db:"public_updates_room_id" json:"public_updates_room_id,omitempty"`
	MaxVideoRoomUsers       int       `db:"max_video_room_users" json:"max_video_room_users"`
	EveryoneRoleID          int64     `db:"everyone_role_id" json:"everyone_role_id,omitempty"`
	CreatedAt               time.Time `db:"created_at" json:"created_at"`
	UpdatedAt               time.Time `db:"updated_at" json:"updated_at"`
}

// SpaceRole is a permission-bearing role in a space (@everyone or custom).
type SpaceRole struct {
	SpaceID     int64     `db:"space_id" json:"-"`
	ID          int64     `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	Permissions int64     `db:"permissions" json:"permissions"`
	Position    int       `db:"position" json:"position"`
	Color       int       `db:"color" json:"color"`
	Hoist       bool      `db:"hoist" json:"hoist"`
	Mentionable bool      `db:"mentionable" json:"mentionable"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

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

// SpaceMember is a user's membership in a space.
type SpaceMember struct {
	SpaceID   int64     `db:"space_id"`
	UserID    int64     `db:"user_id"`
	RoleIDs   []int64   `db:"role_ids"`
	Nickname  string    `db:"nickname"`
	JoinedAt  time.Time `db:"joined_at"`
}

// SpaceMemberRow is spaces_by_user row.
type SpaceMemberRow struct {
	UserID   int64     `db:"user_id"`
	SpaceID  int64     `db:"space_id"`
	JoinedAt time.Time `db:"joined_at"`
}

// CreateSpaceInput is the request body for creating a space.
type CreateSpaceInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
}

// SpaceInvite is an invite link/code for joining a space.
// For now, invites have unlimited uses and no expiry; this can be extended later.
type SpaceInvite struct {
	Code      string    `db:"code" json:"code"`
	SpaceID   int64     `db:"space_id" json:"space_id"`
	InviterID int64     `db:"inviter_id" json:"inviter_id"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

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
