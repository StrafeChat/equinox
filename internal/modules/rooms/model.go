package rooms

import (
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// RoomType discriminator for polymorphic rooms.
const (
	TypePM          = 1
	TypeGroupPM     = 2
	TypeSpaceText   = 3
	TypeSpaceVoice  = 4
	TypeRoomSection = 5
	TypeThread      = 6
)

// NotifyMode is a per-user, per-room override of the client's global notification
// default for that room type - mirrors Discord's per-channel notification setting.
const (
	NotifyModeDefault  = 0
	NotifyModeAll      = 1
	NotifyModeMentions = 2
	NotifyModeNone     = 3
)

// ValidNotifyMode reports whether v is one of the NotifyMode constants.
func ValidNotifyMode(v int) bool {
	return v >= NotifyModeDefault && v <= NotifyModeNone
}

// Room is a polymorphic container: PM, Group PM, Space text/voice, room section, or thread.
// For TypeGroupPM, CreatorID is the user who created the group (only they can remove members or rename).
// E2EEEnabled: when false, messages are stored as plaintext. For PM/GroupPM nil/true = E2EE on (default).
// For space rooms (TypeSpaceText, TypeSpaceVoice), E2EE is off by default (scale; a future space-scale
// E2EE protocol, e.g. MLS or sender-keys-style for thousands of members, could be added later).
type Room struct {
	ID              int64  `db:"id" json:"id"`
	Type            int    `db:"type" json:"type"`
	SpaceID         *int64 `db:"space_id" json:"space_id,omitempty"`
	ParentID        *int64 `db:"parent_id" json:"parent_id,omitempty"`
	Name            string `db:"name" json:"name,omitempty"`
	Topic           string `db:"topic" json:"topic,omitempty"`
	SlowmodeSeconds int    `db:"slowmode_seconds" json:"slowmode_seconds,omitempty"`
	Position        int    `db:"position" json:"position"`
	CreatorID       int64  `db:"creator_id" json:"creator_id,omitempty"`
	E2EEEnabled     *bool  `db:"e2ee_enabled" json:"e2ee_enabled,omitempty"`
	// PermissionsSynced: a space text/voice channel follows its parent section's permission
	// overrides (Discord category sync) instead of its own. Resolved at read time, so editing
	// the section updates every synced channel without copying. Nil/false = not synced.
	PermissionsSynced *bool  `db:"permissions_synced" json:"permissions_synced,omitempty"`
	LastMessageID   *int64 `db:"last_message_id" json:"last_message_id,omitempty"`
	// Voice rooms only. UserLimit caps how many people can be connected at once (0 =
	// unlimited; members with Move Members bypass it, as on Discord). Bitrate is the
	// audio bitrate clients publish at, in bits per second (0 = DefaultVoiceBitrate).
	UserLimit int       `db:"user_limit" json:"user_limit,omitempty"`
	Bitrate   int       `db:"bitrate" json:"bitrate,omitempty"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// Voice room setting bounds. Bitrates are Opus-sensible: 8 kbps is barely intelligible,
// 384 kbps is beyond what Opus voice benefits from (Discord's ceiling is the same).
const (
	DefaultVoiceBitrate = 64_000
	MinVoiceBitrate     = 8_000
	MaxVoiceBitrate     = 384_000
	MaxVoiceUserLimit   = 99
)

// IsVoice reports whether the room carries voice/video (space voice rooms, and PMs and
// group PMs, which can run a call).
func (r *Room) IsVoice() bool {
	return r != nil && (r.Type == TypeSpaceVoice || r.Type == TypePM || r.Type == TypeGroupPM)
}

// Participant is a minimal user for room display.
type Participant struct {
	ID            string              `json:"id"`
	Username      string              `json:"username"`
	Discriminator int                 `json:"discriminator"`
	DisplayName   string              `json:"display_name"`
	Avatar        string              `json:"avatar,omitempty"`
	Banner        string              `json:"banner,omitempty"`
	Bio           string              `json:"bio,omitempty"`
	AboutMe       string              `json:"about_me,omitempty"`
	PublicFlags   int                 `json:"public_flags"`
	Bot           bool                `json:"bot,omitempty"`
	Presence      auth.PublicPresence `json:"presence"`
	// Federation (only when this instance has a domain): the user's home instance and the
	// id it knows them by. Clients build the E2EE identity @origin_id:home_domain from these.
	HomeDomain string `json:"home_domain,omitempty"`
	OriginID   string `json:"origin_id,omitempty"`
}

// Federation is a room's global identity: the (domain, id) of the instance that created
// it. Present on rooms that span instances so clients key Megolm sessions consistently.
type Federation struct {
	OriginDomain string `json:"origin_domain"`
	OriginID     string `json:"origin_id"`
}

// RoomWithParticipants extends Room with participant IDs and optional details.
type RoomWithParticipants struct {
	Room
	ParticipantIDs    []int64       `json:"recipients,omitempty"`
	Participants      []Participant `json:"participants,omitempty"`
	LastReadMessageID *int64        `json:"last_read_message_id,omitempty"`
	MentionCount      int           `json:"mention_count,omitempty"`
	Federation        *Federation   `json:"federation,omitempty"`
	// Muted/MutedUntil/NotifyMode are this user's own per-room notification settings -
	// see RoomRow.
	Muted      bool       `json:"muted,omitempty"`
	MutedUntil *time.Time `json:"muted_until,omitempty"`
	NotifyMode int        `json:"notify_mode,omitempty"`
}

// RoomRow is rooms_by_user row. MentionCount is the dead pre-counter-table column (see
// migrations/006_read_state.cql / 017_mentions.cql) - unused going forward, kept only so
// existing rows don't need a destructive migration. MentionCountBaseline is the real
// counter's value as of the last ack; the displayed count is GetMentionCount(...) minus this.
type RoomRow struct {
	UserID               int64      `db:"user_id"`
	RoomID               int64      `db:"room_id"`
	LastMessageID        *int64     `db:"last_message_id"`
	LastReadMessageID    *int64     `db:"last_read_message_id"`
	MentionCount         int        `db:"mention_count"`
	MentionCountBaseline *int64     `db:"mention_count_baseline"`
	JoinedAt             time.Time  `db:"joined_at"`
	Muted                bool       `db:"muted"`
	MutedUntil           *time.Time `db:"muted_until"`
	NotifyMode           int        `db:"notify_mode"`
}

// IsMuted reports whether the room is currently muted for this user - either indefinitely
// (Muted) or under a timed mute that hasn't expired yet (MutedUntil in the future). A timed
// mute past its expiry is treated as unmuted without any cleanup: this is checked lazily on
// every read, never written back.
func (r *RoomRow) IsMuted(now time.Time) bool {
	if r == nil {
		return false
	}
	if r.Muted {
		return true
	}
	return r.MutedUntil != nil && now.Before(*r.MutedUntil)
}

// DisplayMentionCount computes the user-facing mention count: the counter's raw total
// (from room_mention_counts) minus whatever it was at the last ack. Never negative.
func (r *RoomRow) DisplayMentionCount(rawTotal int) int {
	baseline := int64(0)
	if r != nil && r.MentionCountBaseline != nil {
		baseline = *r.MentionCountBaseline
	}
	count := int64(rawTotal) - baseline
	if count < 0 {
		return 0
	}
	return int(count)
}

// PMRoomsRow is pm_rooms lookup row.
type PMRoomsRow struct {
	UserAID   int64     `db:"user_a_id"`
	UserBID   int64     `db:"user_b_id"`
	RoomID    int64     `db:"room_id"`
	CreatedAt time.Time `db:"created_at"`
}

// RoomBySpaceRow is rooms_by_space row.
type RoomBySpaceRow struct {
	SpaceID   int64     `db:"space_id"`
	RoomID    int64     `db:"room_id"`
	Position  int       `db:"position"`
	CreatedAt time.Time `db:"created_at"`
}
