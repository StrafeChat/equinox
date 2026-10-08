package spaces

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/safego"
)

// RoomOverrides is every permission override on one room.
type RoomOverrides struct {
	Roles []SpaceRoomRoleOverride `json:"roles"`
	Users []SpaceRoomUserOverride `json:"users"`
}

// RoomMeta is the little bit of a room the permission resolver needs beyond its overrides:
// its parent, whether it is a channel synced to that parent section (follows the section's
// overrides instead of its own - Discord's category sync), or a thread (always resolves
// through its parent channel), and whether a thread is private (members only).
type RoomMeta struct {
	ParentID int64 `json:"parent_id"`
	Synced   bool  `json:"synced"`
	Thread   bool  `json:"thread,omitempty"`
	Private  bool  `json:"private,omitempty"`
}

// Snapshot is everything needed to resolve any member's permissions anywhere in a space
// without touching the database again: the owner, the @everyone role, every role and every
// room's overrides. It is what a Discord client holds per guild after GUILD_CREATE, and
// here it is also what the server consults on every message send/list instead of the
// five Scylla reads that used to run per request.
//
// Cached in Redis (shared by the API and the gateway) and dropped whenever a role, an
// override or the room list changes, so it can never be stale for longer than one
// mutation. The TTL is only a safety net.
type Snapshot struct {
	OwnerID        int64                   `json:"owner_id"`
	EveryoneRoleID int64                   `json:"everyone_role_id"`
	Roles          []SpaceRole             `json:"roles"`
	Overrides      map[int64]RoomOverrides `json:"overrides"`
	Meta           map[int64]RoomMeta      `json:"meta"`
	BuiltAt        time.Time               `json:"built_at"`
}

const (
	snapshotTTL              = 10 * time.Minute
	snapshotBuildConcurrency = 8
)

// RoomOverridesFor returns a room's own overrides (zero value when it has none).
func (snap *Snapshot) RoomOverridesFor(roomID int64) RoomOverrides {
	if snap == nil {
		return RoomOverrides{}
	}
	return snap.Overrides[roomID]
}

// EffectiveRoomOverrides returns the overrides that actually apply to a room: its parent
// section's when the room is synced to it (Discord category sync), otherwise its own. This is
// the single place inheritance is resolved, so every permission check and the wire form agree.
func (snap *Snapshot) EffectiveRoomOverrides(roomID int64) RoomOverrides {
	if snap == nil {
		return RoomOverrides{}
	}
	if m, ok := snap.Meta[roomID]; ok && m.ParentID != 0 {
		// A thread has no overrides of its own: it is its channel's, and through the channel a
		// synced section's (Discord: threads inherit the parent channel's overwrites).
		if m.Thread {
			return snap.EffectiveRoomOverrides(m.ParentID)
		}
		if m.Synced {
			return snap.Overrides[m.ParentID]
		}
	}
	return snap.Overrides[roomID]
}

// basePermissions ORs @everyone with every role the member holds.
func (snap *Snapshot) basePermissions(memberRoleIDs []int64) int64 {
	var base int64
	byID := make(map[int64]int64, len(snap.Roles))
	for i := range snap.Roles {
		byID[snap.Roles[i].ID] = snap.Roles[i].Permissions
	}
	base = byID[snap.EveryoneRoleID]
	for _, rid := range memberRoleIDs {
		if rid == snap.EveryoneRoleID {
			continue
		}
		base |= byID[rid]
	}
	return base
}

func (s *Service) snapshotCacheEnabled() bool {
	return s.redis != nil && s.cfg != nil && s.cfg.Database.Redis.CacheEnabled
}

func (s *Service) snapshotKey(spaceID int64) string {
	prefix := ""
	if s.cfg != nil {
		prefix = s.cfg.Database.Redis.CachePrefix
		if prefix != "" && !strings.HasSuffix(prefix, ":") {
			prefix += ":"
		}
	}
	// The version segment guards against a cache poisoned by a differently-shaped snapshot -
	// e.g. during a deploy when the API and the gateway (which share this cache) briefly run
	// different versions. Bump it whenever the Snapshot struct's stored shape changes.
	// v2 added RoomMeta (parent + sync) for category permission inheritance; v3 marks threads
	// (which resolve through their channel) and private threads in it.
	return prefix + "space:snapshot:v3:" + id.Format(spaceID)
}

// Snapshot returns the space's full permission snapshot, from Redis when cached.
func (s *Service) Snapshot(ctx context.Context, spaceID int64) (*Snapshot, error) {
	if s.snapshotCacheEnabled() {
		if raw, err := s.redis.Get(ctx, s.snapshotKey(spaceID)).Bytes(); err == nil {
			var snap Snapshot
			if json.Unmarshal(raw, &snap) == nil && snap.EveryoneRoleID != 0 {
				return &snap, nil
			}
		}
	}
	snap, err := s.buildSnapshot(ctx, spaceID, nil, true)
	if err != nil {
		return nil, err
	}
	if s.snapshotCacheEnabled() {
		if raw, err := json.Marshal(snap); err == nil {
			if err := s.redis.Set(ctx, s.snapshotKey(spaceID), raw, snapshotTTL).Err(); err != nil {
				logger.Warn("spaces", "could not cache snapshot for space %d: %v", spaceID, err)
			}
		}
	}
	return snap, nil
}

// permissionSnapshot is what a single permission check needs. With the cache on that is
// the shared full snapshot; without it, only this room's overrides are read so a check
// costs no more than it did before snapshots existed.
func (s *Service) permissionSnapshot(ctx context.Context, spaceID, roomID int64) (*Snapshot, error) {
	if s.snapshotCacheEnabled() {
		return s.Snapshot(ctx, spaceID)
	}
	var only []int64
	if roomID != 0 {
		only = []int64{roomID}
	}
	return s.buildSnapshot(ctx, spaceID, only, false)
}

func (s *Service) invalidateSnapshot(ctx context.Context, spaceID int64) {
	if !s.snapshotCacheEnabled() {
		return
	}
	if err := s.redis.Del(ctx, s.snapshotKey(spaceID)).Err(); err != nil {
		logger.Warn("spaces", "could not invalidate snapshot for space %d: %v", spaceID, err)
	}
}

// buildSnapshot reads the space, its roles and - for every room when all is set, else
// just `only` - the room overrides. Override partitions are read concurrently.
func (s *Service) buildSnapshot(ctx context.Context, spaceID int64, only []int64, all bool) (*Snapshot, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, ErrSpaceNotFound
	}
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return nil, err
	}
	roles, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sortRoles(roles)
	roomIDs := only
	if all {
		rows, err := s.roomRepo.ListBySpace(ctx, spaceID)
		if err != nil {
			return nil, err
		}
		roomIDs = make([]int64, 0, len(rows))
		for _, row := range rows {
			roomIDs = append(roomIDs, row.RoomID)
		}
	}
	// Room meta (parent + synced) for the rooms in scope. A synced room resolves against its
	// section, so in the single-room (cache-off) path its parent's overrides must be loaded too.
	meta := make(map[int64]RoomMeta, len(roomIDs))
	if len(roomIDs) > 0 {
		rs, err := s.roomRepo.GetByIDs(ctx, roomIDs)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if r == nil {
				continue
			}
			var parent int64
			if r.ParentID != nil {
				parent = *r.ParentID
			}
			meta[r.ID] = RoomMeta{ParentID: parent, Synced: r.PermissionsSynced != nil && *r.PermissionsSynced, Thread: r.Type == rooms.TypeThread, Private: r.ThreadIsPrivate()}
		}
	}
	overrideIDs := roomIDs
	if !all {
		seen := make(map[int64]struct{}, len(roomIDs))
		for _, rid := range roomIDs {
			seen[rid] = struct{}{}
		}
		// A thread resolves through its channel: pull the channel in (its meta too, since the
		// channel may itself be synced to a section) so the single-room snapshot can resolve it.
		var parents []int64
		for _, m := range meta {
			if m.Thread && m.ParentID != 0 {
				if _, ok := seen[m.ParentID]; !ok {
					seen[m.ParentID] = struct{}{}
					parents = append(parents, m.ParentID)
					overrideIDs = append(overrideIDs, m.ParentID)
				}
			}
		}
		if len(parents) > 0 {
			if rs, err := s.roomRepo.GetByIDs(ctx, parents); err == nil {
				for _, r := range rs {
					if r == nil {
						continue
					}
					var parent int64
					if r.ParentID != nil {
						parent = *r.ParentID
					}
					meta[r.ID] = RoomMeta{ParentID: parent, Synced: r.PermissionsSynced != nil && *r.PermissionsSynced}
				}
			}
		}
		for _, m := range meta {
			if m.Synced && m.ParentID != 0 {
				if _, ok := seen[m.ParentID]; !ok {
					seen[m.ParentID] = struct{}{}
					overrideIDs = append(overrideIDs, m.ParentID)
				}
			}
		}
	}
	snap := &Snapshot{
		OwnerID:        sp.OwnerID,
		EveryoneRoleID: everyoneID,
		Roles:          roles,
		Overrides:      make(map[int64]RoomOverrides, len(overrideIDs)),
		Meta:           meta,
		BuiltAt:        time.Now().UTC(),
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, snapshotBuildConcurrency)
	)
	for _, roomID := range overrideIDs {
		roomID := roomID
		wg.Add(1)
		sem <- struct{}{}
		go safego.Run("spaces", func() {
			defer wg.Done()
			defer func() { <-sem }()
			ro, err1 := s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
			uo, err2 := s.repo.ListRoomUserOverrides(ctx, spaceID, roomID)
			mu.Lock()
			defer mu.Unlock()
			if err1 != nil || err2 != nil {
				if firstErr == nil {
					if err1 != nil {
						firstErr = err1
					} else {
						firstErr = err2
					}
				}
				return
			}
			if len(ro) > 0 || len(uo) > 0 {
				snap.Overrides[roomID] = RoomOverrides{Roles: ro, Users: uo}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return snap, nil
}

func sortRoles(roles []SpaceRole) {
	sort.Slice(roles, func(i, j int) bool {
		if roles[i].Position != roles[j].Position {
			return roles[i].Position < roles[j].Position
		}
		return roles[i].ID < roles[j].ID
	})
}

// SpaceRoles returns a space's roles sorted by position, from the snapshot.
func (s *Service) SpaceRoles(ctx context.Context, spaceID int64) ([]SpaceRole, error) {
	snap, err := s.permissionSnapshot(ctx, spaceID, 0)
	if err != nil {
		return nil, err
	}
	return snap.Roles, nil
}

// RoleMap is the wire form of a role, shared by REST responses, gateway events and the
// READY payload so every path agrees on the shape.
func RoleMap(r *SpaceRole) map[string]interface{} {
	m := map[string]interface{}{
		"id":          id.Format(r.ID),
		"name":        r.Name,
		"permissions": r.Permissions,
		"position":    r.Position,
		"color":       r.Color,
		"hoist":       r.Hoist,
		"mentionable": r.Mentionable,
		"created_at":  r.CreatedAt,
		"updated_at":  r.UpdatedAt,
	}
	if r.BotID != 0 {
		m["bot_id"] = id.Format(r.BotID)
	}
	return m
}

// RoleMaps converts a role list for a payload.
func RoleMaps(roles []SpaceRole) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(roles))
	for i := range roles {
		out = append(out, RoleMap(&roles[i]))
	}
	return out
}

// OverrideMaps is the wire form of a room's overrides: role overrides then user
// overrides, always non-nil so clients can tell "no overrides" from "not included".
func OverrideMaps(o RoomOverrides) (roles []map[string]interface{}, users []map[string]interface{}) {
	roles = make([]map[string]interface{}, 0, len(o.Roles))
	for i := range o.Roles {
		r := &o.Roles[i]
		roles = append(roles, map[string]interface{}{
			"role_id":    id.Format(r.RoleID),
			"allow":      r.Allow & permissions.AllRoom,
			"deny":       r.Deny & permissions.AllRoom,
			"created_at": r.CreatedAt,
			"updated_at": r.UpdatedAt,
		})
	}
	users = make([]map[string]interface{}, 0, len(o.Users))
	for i := range o.Users {
		u := &o.Users[i]
		users = append(users, map[string]interface{}{
			"user_id":    id.Format(u.UserID),
			"allow":      u.Allow & permissions.AllRoom,
			"deny":       u.Deny & permissions.AllRoom,
			"created_at": u.CreatedAt,
			"updated_at": u.UpdatedAt,
		})
	}
	return roles, users
}

// AttachOverrides adds a room's overrides to its wire map under the keys the client reads.
func AttachOverrides(m map[string]interface{}, o RoomOverrides) map[string]interface{} {
	roles, users := OverrideMaps(o)
	m["permission_overrides"] = roles
	m["user_overrides"] = users
	return m
}
