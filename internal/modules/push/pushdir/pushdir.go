// Package pushdir implements push.Directory over the real repositories. It is its own
// package because push must stay a leaf: rooms, messages and voice import push to queue
// notifications, so push cannot import them back - this package can.
package pushdir

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/push"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// sessionCacheTTL is how long "this session is signed in" is believed without asking the
// database again. Short, and safe to be: a revoked session's subscription is deleted by
// the revocation hook itself; this check only catches sessions that quietly expired.
const sessionCacheTTL = time.Minute

// ChannelPermissions resolves a member's effective permissions in a space room
// (spaces.Service).
type ChannelPermissions interface {
	EffectiveChannelPermissions(ctx context.Context, userID, spaceID, roomID int64) (int64, error)
}

// Directory answers the push workers' lookups.
type Directory struct {
	users    auth.UserRepository
	sessions auth.SessionRepository
	rooms    rooms.Repository
	spaces   spaces.Repository
	perms    ChannelPermissions
	now      func() time.Time

	mu     sync.Mutex
	active map[[2]int64]time.Time // (user, session) -> believed active until
}

// New returns a Directory. users should be the cached user repository: the workers look
// the same authors and recipients up over and over.
func New(users auth.UserRepository, sessions auth.SessionRepository, roomRepo rooms.Repository, spaceRepo spaces.Repository, perms ChannelPermissions) *Directory {
	return &Directory{
		users: users, sessions: sessions, rooms: roomRepo, spaces: spaceRepo, perms: perms,
		now: time.Now, active: map[[2]int64]time.Time{},
	}
}

var _ push.Directory = (*Directory)(nil)

func (d *Directory) User(ctx context.Context, userID int64) (*push.UserInfo, error) {
	u, err := d.users.GetByID(ctx, userID)
	if err != nil || u == nil {
		return nil, err
	}
	return &push.UserInfo{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Avatar: u.Avatar,
		Status: u.Presence.Status, Blocks: u.Blocks,
	}, nil
}

func (d *Directory) SessionActive(ctx context.Context, userID, sessionID int64) (bool, error) {
	key := [2]int64{userID, sessionID}
	now := d.now()
	d.mu.Lock()
	until, ok := d.active[key]
	d.mu.Unlock()
	if ok && now.Before(until) {
		return true, nil
	}
	s, err := d.sessions.GetByUserSession(ctx, userID, sessionID)
	if err != nil {
		return false, err
	}
	live := s != nil && s.RevokedAt.IsZero() && now.Before(s.ExpiresAt)
	d.mu.Lock()
	if live {
		if len(d.active) > 100_000 {
			clear(d.active) // bounded; a miss only costs one read
		}
		d.active[key] = now.Add(min(sessionCacheTTL, s.ExpiresAt.Sub(now)))
	} else {
		delete(d.active, key)
	}
	d.mu.Unlock()
	return live, nil
}

func (d *Directory) Room(ctx context.Context, roomID int64) (*push.RoomInfo, error) {
	r, err := d.rooms.GetByID(ctx, roomID)
	if err != nil || r == nil {
		return nil, err
	}
	info := &push.RoomInfo{ID: r.ID, Type: r.Type, Name: r.Name}
	if r.SpaceID != nil {
		info.SpaceID = *r.SpaceID
	}
	if r.ParentID != nil {
		info.ParentID = *r.ParentID
	}
	return info, nil
}

func (d *Directory) RoomParticipants(ctx context.Context, roomID int64) ([]int64, error) {
	return d.rooms.GetParticipants(ctx, roomID)
}

func (d *Directory) RoomSettings(ctx context.Context, userID, roomID int64) (*push.RoomSettings, error) {
	row, err := d.rooms.GetRoomRow(ctx, userID, roomID)
	if err != nil || row == nil {
		return nil, err
	}
	return &push.RoomSettings{Muted: row.Muted, MutedUntil: row.MutedUntil, NotifyMode: row.NotifyMode}, nil
}

func (d *Directory) Space(ctx context.Context, spaceID int64) (*push.SpaceInfo, error) {
	sp, err := d.spaces.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, err
	}
	// default_message_notifications: 0 = all messages, 1 = only mentions (Discord's values).
	return &push.SpaceInfo{ID: sp.ID, Name: sp.Name, DefaultMentionsOnly: sp.DefaultMessageNotif == 1}, nil
}

func (d *Directory) RoleNames(ctx context.Context, spaceID int64) (map[int64]string, error) {
	roles, err := d.spaces.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(roles))
	for _, r := range roles {
		out[r.ID] = r.Name
	}
	return out, nil
}

func (d *Directory) CanView(ctx context.Context, userID int64, room *push.RoomInfo) (bool, error) {
	if room.SpaceID == 0 {
		return true, nil
	}
	perms, err := d.perms.EffectiveChannelPermissions(ctx, userID, room.SpaceID, room.ID)
	if errors.Is(err, spaces.ErrNotMember) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return permissions.Has(perms, permissions.PermViewRoom), nil
}
