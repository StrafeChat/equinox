package types

import "time"

type Space struct {
	ID                          int64     `json:"id"`
	Name                        string    `json:"name"`
	NameAcronym                 string    `json:"name_acronym"`
	Description                 *string   `json:"description,omitempty"`
	Icon                        *string   `json:"icon,omitempty"`
	Banner                      *string   `json:"banner,omitempty"`
	OwnerID                     string    `json:"owner_id"`
	VerificationLevel           int       `json:"verification_level"`
	DefaultMessageNotifications int       `json:"default_message_notifications"`
	ExplicitContentFilter       int       `json:"explicit_content_filter"`
	Features                    []string  `json:"features"`
	AfkRoomID                   *string   `json:"afk_room_id,omitempty"`
	AfkTimeout                  int       `json:"afk_timeout"`
	SystemRoomID                *string   `json:"system_room_id,omitempty"`
	SystemRoomFlags             int       `json:"system_room_flags"`
	RulesRoomID                 *string   `json:"rules_room_id,omitempty"`
	MaxPresences                *int      `json:"max_presences,omitempty"`
	MaxMembers                  *int      `json:"max_members,omitempty"`
	VanityUrlCode               *string   `json:"vanity_url_code,omitempty"`
	PreferredLocale             string    `json:"preferred_locale"`
	PublicUpdatesRoomID         *string   `json:"public_updates_room_id,omitempty"`
	MaxVideoRoomUsers           *int      `json:"max_video_room_users,omitempty"`
	NsfwLevel                   int       `json:"nsfw_level"`
	CreatedAt                   time.Time `json:"created_at"`
	UpdatedAt                   time.Time `json:"updated_at"`
}

type SpaceMember struct {
	SpaceID                    int64      `json:"space_id"`
	UserID                     string     `json:"user_id"`
	Nick                       *string    `json:"nick,omitempty"`
	Avatar                     *string    `json:"avatar,omitempty"`
	Roles                      []string   `json:"roles"`
	JoinedAt                   time.Time  `json:"joined_at"`
	PremiumSince               *time.Time `json:"premium_since,omitempty"`
	Deaf                       bool       `json:"deaf"`
	Mute                       bool       `json:"mute"`
	Flags                      int        `json:"flags"`
	Pending                    bool       `json:"pending"`
	Permissions                *string    `json:"permissions,omitempty"`
	CommunicationDisabledUntil *time.Time `json:"communication_disabled_until,omitempty"`
}
