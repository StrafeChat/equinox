package push

import (
	"context"
	"time"
)

// Directory is what the delivery workers look up about users, sessions, rooms and spaces.
// It is an interface so this package stays a leaf (the modules it would otherwise import
// call the producers); push/pushdir implements it over the real, cached repositories.
// Every method returns (nil, nil) for something that does not exist.
type Directory interface {
	User(ctx context.Context, userID int64) (*UserInfo, error)
	// SessionActive reports whether the session exists, is not revoked and has not expired.
	SessionActive(ctx context.Context, userID, sessionID int64) (bool, error)
	Room(ctx context.Context, roomID int64) (*RoomInfo, error)
	RoomParticipants(ctx context.Context, roomID int64) ([]int64, error)
	// RoomSettings is the user's own mute/notify-mode override for a room; nil = none.
	RoomSettings(ctx context.Context, userID, roomID int64) (*RoomSettings, error)
	Space(ctx context.Context, spaceID int64) (*SpaceInfo, error)
	// RoleNames maps a space's role ids to their names.
	RoleNames(ctx context.Context, spaceID int64) (map[int64]string, error)
	// CanView reports whether the user may see a space room (its channel permissions grant
	// View Room). A message in a private channel fans out to the whole space's member
	// list; only those who can read it may be told about it.
	CanView(ctx context.Context, userID int64, room *RoomInfo) (bool, error)
}

// UserInfo is the slice of an account the decision and the payload need.
type UserInfo struct {
	ID          int64
	Username    string
	DisplayName string
	// Avatar is an absolute URL, or empty.
	Avatar string
	// Status is the presence the user chose ("dnd" silences notifications).
	Status string
	Blocks []int64
}

// Name is how the user is shown: display name, else username.
func (u *UserInfo) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func (u *UserInfo) blocked(other int64) bool {
	for _, b := range u.Blocks {
		if b == other {
			return true
		}
	}
	return false
}

// RoomInfo is a room's identity and name.
type RoomInfo struct {
	ID       int64
	Type     int
	SpaceID  int64 // 0 outside a space
	ParentID int64 // a thread's channel
	Name     string
}

// RoomSettings is a user's per-room notification override (rooms.RoomRow's fields).
type RoomSettings struct {
	Muted      bool
	MutedUntil *time.Time
	NotifyMode int
}

// mutedAt mirrors rooms.RoomRow.IsMuted: muted indefinitely, or under a timed mute that
// has not run out.
func (s *RoomSettings) mutedAt(now time.Time) bool {
	if s == nil {
		return false
	}
	return s.Muted || (s.MutedUntil != nil && now.Before(*s.MutedUntil))
}

// SpaceInfo is a space's name and its default notification level.
type SpaceInfo struct {
	ID   int64
	Name string
	// DefaultMentionsOnly: the space's default_message_notifications is "only mentions".
	DefaultMentionsOnly bool
}
