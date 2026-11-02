package spaces

import (
	"context"
	"slices"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// clearOwnOverrides removes every permission override stored directly on a room and tells
// clients about each removal. Used when a channel is synced to its section: a synced channel
// follows the section's overrides, so its own are redundant and are cleared for a clean state.
func (s *Service) clearOwnOverrides(ctx context.Context, spaceID, roomID int64) error {
	roleOv, err := s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
	if err != nil {
		return err
	}
	for i := range roleOv {
		if err := s.repo.DeleteRoomRoleOverride(ctx, spaceID, roomID, roleOv[i].RoleID); err != nil {
			return err
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_OVERRIDE_DELETE", map[string]interface{}{
			"room_id": id.Format(roomID),
			"role_id": id.Format(roleOv[i].RoleID),
		})
	}
	userOv, err := s.repo.ListRoomUserOverrides(ctx, spaceID, roomID)
	if err != nil {
		return err
	}
	for i := range userOv {
		if err := s.repo.DeleteRoomUserOverride(ctx, spaceID, roomID, userOv[i].UserID); err != nil {
			return err
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_DELETE", map[string]interface{}{
			"room_id": id.Format(roomID),
			"user_id": id.Format(userOv[i].UserID),
		})
	}
	return nil
}

// copyOverridesFromParent writes the parent section's overrides onto the room as its own and
// tells clients about each. Used when a channel is unsynced (or moved out of its section): the
// perms it had while synced are snapshotted onto it so nothing changes at the moment it diverges.
func (s *Service) copyOverridesFromParent(ctx context.Context, spaceID, roomID, parentID int64) error {
	roleOv, err := s.repo.ListRoomRoleOverrides(ctx, spaceID, parentID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range roleOv {
		o := &SpaceRoomRoleOverride{
			SpaceID:   spaceID,
			RoomID:    roomID,
			RoleID:    roleOv[i].RoleID,
			Allow:     roleOv[i].Allow,
			Deny:      roleOv[i].Deny,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.repo.UpsertRoomRoleOverride(ctx, o); err != nil {
			return err
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_OVERRIDE_UPDATE", roomOverrideEventData(o))
	}
	userOv, err := s.repo.ListRoomUserOverrides(ctx, spaceID, parentID)
	if err != nil {
		return err
	}
	for i := range userOv {
		o := &SpaceRoomUserOverride{
			SpaceID:   spaceID,
			RoomID:    roomID,
			UserID:    userOv[i].UserID,
			Allow:     userOv[i].Allow,
			Deny:      userOv[i].Deny,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.repo.UpsertRoomUserOverride(ctx, o); err != nil {
			return err
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_UPDATE", roomUserOverrideEventData(o))
	}
	return nil
}

// applyPermissionsSync syncs or unsyncs a channel to its parent section as part of a room
// update (runs on the origin; mirrors forward the whole update first). Syncing requires a
// text/voice channel with a parent section and clears its own overrides; unsyncing copies the
// section's overrides down so the channel keeps the perms it had. Returns whether it changed.
func (s *Service) applyPermissionsSync(ctx context.Context, spaceID int64, room *roomForSync, want bool) (bool, error) {
	current := room.Synced
	if want == current {
		return false, nil
	}
	if want {
		if room.Type != rooms.TypeSpaceText && room.Type != rooms.TypeSpaceVoice {
			return false, ErrInvalidRoomType
		}
		if room.ParentID == 0 {
			return false, ErrInvalidRoom
		}
		if err := s.clearOwnOverrides(ctx, spaceID, room.ID); err != nil {
			return false, err
		}
	} else {
		if room.ParentID != 0 {
			if err := s.copyOverridesFromParent(ctx, spaceID, room.ID, room.ParentID); err != nil {
				return false, err
			}
		}
	}
	if err := s.roomRepo.UpdateRoomPermissionsSynced(ctx, room.ID, want); err != nil {
		return false, err
	}
	// The snapshot carries each room's synced flag and its overrides, both of which just
	// changed - drop it so the next permission check and room listing rebuild from the writes.
	s.invalidateSnapshot(ctx, spaceID)
	// Syncing changes the channel's effective View, so some members may gain or lose it, in
	// the channel and in its threads.
	s.republishRoomVisibility(ctx, spaceID, s.visibilityAffectedRooms(ctx, spaceID, room.ID))
	return true, nil
}

// roomForSync is the slice of a room applyPermissionsSync needs.
type roomForSync struct {
	ID       int64
	Type     int
	ParentID int64
	Synced   bool
}

// visibilityAffectedRooms returns the rooms whose per-member View could change from a
// permission change on the given rooms: those rooms, plus every channel synced to one of them
// that is a section (those inherit the section's overrides), plus the threads of all of those
// (a thread resolves through its channel).
func (s *Service) visibilityAffectedRooms(ctx context.Context, spaceID int64, roomIDs ...int64) []int64 {
	all, err := s.listRooms(ctx, spaceID)
	if err != nil {
		return roomIDs
	}
	affected := make(map[int64]struct{}, len(roomIDs))
	for _, rid := range roomIDs {
		affected[rid] = struct{}{}
	}
	parentAffected := func(r *rooms.Room) bool {
		if r.ParentID == nil {
			return false
		}
		_, ok := affected[*r.ParentID]
		return ok
	}
	for _, r := range all {
		if r.Type != rooms.TypeThread && r.PermissionsSynced != nil && *r.PermissionsSynced && parentAffected(r) {
			affected[r.ID] = struct{}{}
		}
	}
	for _, r := range all {
		if r.Type == rooms.TypeThread && parentAffected(r) {
			affected[r.ID] = struct{}{}
		}
	}
	ids := make([]int64, 0, len(affected))
	for rid := range affected {
		ids = append(ids, rid)
	}
	slices.Sort(ids)
	return ids
}

// republishRoomVisibility tells each member whether they can now see the given rooms, so a
// permission change adds or removes them from sidebars in real time (Discord-style) without a
// reload - including for a member who never had the room because it was created private.
// Members who can view get SPACE_ROOM_CREATE (the client upserts it); members who cannot get
// SPACE_ROOM_DELETE (the client drops it, which also clears any metadata it had cached, and
// the gateway ends any subscription to the room). Runs on override, sync and role-permission
// changes, which are infrequent admin actions, and reads the snapshot: call it after
// invalidateSnapshot, or a room just made private still reads as public.
func (s *Service) republishRoomVisibility(ctx context.Context, spaceID int64, roomIDs []int64) {
	if s.redis == nil || len(roomIDs) == 0 {
		return
	}
	snap, err := s.Snapshot(ctx, spaceID)
	if err != nil {
		return
	}
	members, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil || len(members) == 0 {
		return
	}
	list, err := s.roomRepo.GetByIDs(ctx, roomIDs)
	if err != nil {
		return
	}
	feds := s.RoomFederations(ctx, roomIDs)
	bases := snap.memberBases(members)
	region := s.stargateRegion()
	for _, room := range list {
		if room == nil || room.SpaceID == nil || *room.SpaceID != spaceID {
			continue
		}
		inThread := s.threadRoster(ctx, room.ID)
		canView, cannotView := snap.splitViewers(room.ID, members, bases, inThread)
		s.publishRoomCreate(ctx, snap, room, feds[room.ID], canView, inThread)
		if len(cannotView) > 0 {
			stargate.PublishToUsersIn(ctx, s.redis, cannotView, stargate.InSpace(spaceID).About(room.ID), "SPACE_ROOM_DELETE", map[string]interface{}{"room_id": id.Format(room.ID), "space_id": id.Format(spaceID)}, region)
		}
		s.lostAccess(ctx, spaceID, room.ID, cannotView)
	}
}

// republishViewChanges tells each member about the rooms a role change opened or closed for
// them, and nothing else: before is the space's snapshot from before the change. Telling every
// member about every room instead would send a SPACE_ROOM_DELETE for each room they never
// could see, and each one has the gateway re-check every subscription they hold. With no
// before, it falls back to doing exactly that.
func (s *Service) republishViewChanges(ctx context.Context, spaceID int64, before *Snapshot) {
	if before == nil {
		s.republishAllRooms(ctx, spaceID)
		return
	}
	after, err := s.Snapshot(ctx, spaceID)
	if err != nil {
		return
	}
	members, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil || len(members) == 0 {
		return
	}
	wasBases, nowBases := before.memberBases(members), after.memberBases(members)
	gained, lost := map[int64][]int64{}, map[int64][]int64{}
	rosters := map[int64]func(int64) bool{}
	for roomID := range after.Meta {
		roster := s.threadRoster(ctx, roomID)
		rosters[roomID] = roster
		for i := range members {
			m := &members[i]
			was := permissions.Has(before.roomPermissions(roomID, m.UserID, wasBases[i], m.RoleIDs, roster), permissions.PermViewRoom)
			now := permissions.Has(after.roomPermissions(roomID, m.UserID, nowBases[i], m.RoleIDs, roster), permissions.PermViewRoom)
			switch {
			case now && !was:
				gained[roomID] = append(gained[roomID], m.UserID)
			case was && !now:
				lost[roomID] = append(lost[roomID], m.UserID)
			}
		}
	}
	if s.redis != nil && len(gained) > 0 {
		ids := make([]int64, 0, len(gained))
		for rid := range gained {
			ids = append(ids, rid)
		}
		slices.Sort(ids)
		if list, err := s.roomRepo.GetByIDs(ctx, ids); err == nil {
			feds := s.RoomFederations(ctx, ids)
			for _, room := range list {
				if room != nil {
					s.publishRoomCreate(ctx, after, room, feds[room.ID], gained[room.ID], rosters[room.ID])
				}
			}
		}
	}
	for roomID, users := range lost {
		if s.redis != nil {
			stargate.PublishToUsersIn(ctx, s.redis, users, stargate.InSpace(spaceID).About(roomID), "SPACE_ROOM_DELETE", map[string]interface{}{"room_id": id.Format(roomID), "space_id": id.Format(spaceID)}, s.stargateRegion())
		}
		s.lostAccess(ctx, spaceID, roomID, users)
	}
}

// republishAllRooms is republishRoomVisibility over the whole space, for a change that can
// reach any room: a role's view bits, or the role itself going.
func (s *Service) republishAllRooms(ctx context.Context, spaceID int64) {
	if s.redis == nil {
		return
	}
	rows, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		return
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RoomID)
	}
	s.republishRoomVisibility(ctx, spaceID, ids)
}

// republishMemberVisibility tells one member about the rooms a change of their roles opened
// or closed, and nothing else. The SPACE_ROOM_DELETE also ends their subscription to the
// room, since the gateway authorizes a subscription only when it is made.
func (s *Service) republishMemberVisibility(ctx context.Context, spaceID, userID int64, before, after []int64) {
	if s.redis == nil {
		return
	}
	snap, err := s.Snapshot(ctx, spaceID)
	if err != nil {
		return
	}
	var joined map[int64]struct{}
	wasBase, nowBase := snap.basePermissions(before), snap.basePermissions(after)
	var gained, lost []int64
	for roomID := range snap.Meta {
		inThread := func(int64) bool {
			if joined == nil {
				joined = map[int64]struct{}{}
				if ids, err := s.roomRepo.ListUserThreadIDs(ctx, userID); err == nil {
					for _, tid := range ids {
						joined[tid] = struct{}{}
					}
				}
			}
			_, ok := joined[roomID]
			return ok
		}
		was := permissions.Has(snap.roomPermissions(roomID, userID, wasBase, before, inThread), permissions.PermViewRoom)
		now := permissions.Has(snap.roomPermissions(roomID, userID, nowBase, after, inThread), permissions.PermViewRoom)
		switch {
		case now && !was:
			gained = append(gained, roomID)
		case was && !now:
			lost = append(lost, roomID)
		}
	}
	slices.Sort(gained)
	slices.Sort(lost)
	if len(gained) > 0 {
		if list, err := s.roomRepo.GetByIDs(ctx, gained); err == nil {
			feds := s.RoomFederations(ctx, gained)
			for _, room := range list {
				if room != nil {
					s.publishRoomCreate(ctx, snap, room, feds[room.ID], []int64{userID}, s.threadRoster(ctx, room.ID))
				}
			}
		}
	}
	for _, roomID := range lost {
		stargate.PublishToUserIn(ctx, s.redis, userID, stargate.InSpace(spaceID).About(roomID), "SPACE_ROOM_DELETE", map[string]interface{}{"room_id": id.Format(roomID), "space_id": id.Format(spaceID)}, s.stargateRegion())
		s.lostAccess(ctx, spaceID, roomID, []int64{userID})
	}
}

// publishRoomCreate sends SPACE_ROOM_CREATE to members who can view a room. The payload names
// the room's global identity, which a client needs to key an E2EE channel's sessions on a
// mirror. The client takes the event as the room's whole state, so a thread carries its
// counts and each member's own `joined`, which a bare thread block would reset. An archived
// thread goes to nobody: like READY, it is left for clients to fetch on demand.
func (s *Service) publishRoomCreate(ctx context.Context, snap *Snapshot, room *rooms.Room, fed *rooms.Federation, users []int64, inThread func(userID int64) bool) {
	if len(users) == 0 || (room.Type == rooms.TypeThread && room.ThreadIsArchived()) {
		return
	}
	payload := AttachFederation(AttachOverrides(RoomMap(room), snap.RoomOverridesFor(room.ID)), fed)
	payload["room_id"] = id.Format(room.ID)
	region := s.stargateRegion()
	// The event gives each recipient the room: the gateway subscribes them as it delivers it.
	sc := stargate.InRoom(room.SpaceID, room.ID).Subscribing(room.ID)
	if room.Type != rooms.TypeThread {
		stargate.PublishToUsersIn(ctx, s.redis, users, sc, "SPACE_ROOM_CREATE", payload, region)
		return
	}
	states, _ := s.ThreadStates(ctx, 0, []int64{room.ID})
	state := states[room.ID]
	var joined, others []int64
	for _, uid := range users {
		if inThread(uid) {
			joined = append(joined, uid)
		} else {
			others = append(others, uid)
		}
	}
	AttachThreadState(payload, state)
	stargate.PublishToUsersIn(ctx, s.redis, others, sc, "SPACE_ROOM_CREATE", payload, region)
	state.Joined = true
	AttachThreadState(payload, state)
	stargate.PublishToUsersIn(ctx, s.redis, joined, sc, "SPACE_ROOM_CREATE", payload, region)
}

// threadRoster answers the view rule's "is this user in the thread" for any number of members
// from one read of the thread's member list, made only if someone asks. A failed read leaves
// nobody in it, so a private thread fails closed.
func (s *Service) threadRoster(ctx context.Context, roomID int64) func(userID int64) bool {
	var roster map[int64]struct{}
	return func(userID int64) bool {
		if roster == nil {
			roster = map[int64]struct{}{}
			if ids, err := s.roomRepo.ListThreadMemberIDs(ctx, roomID); err == nil {
				for _, uid := range ids {
					roster[uid] = struct{}{}
				}
			}
		}
		_, ok := roster[userID]
		return ok
	}
}

// everyoneCanView reports whether a room is safe to announce space-wide: @everyone can view it
// (its effective overrides, so a synced channel is judged by its section) and no override
// keeps a role or user out. Used to decide whether a new room must be delivered only to the
// members who can see it. When the snapshot cannot be read the answer is no: a room wrongly
// held back shows up on the next reload, while one wrongly announced has leaked its name.
// OpenToEveryone reports whether every member of the space can view the room, without reading
// the members: false on any doubt, so a caller that publishes space-wide only when it is true
// never tells someone who is kept out.
func (s *Service) OpenToEveryone(ctx context.Context, spaceID, roomID int64) bool {
	return s.everyoneCanView(ctx, spaceID, roomID)
}

func (s *Service) everyoneCanView(ctx context.Context, spaceID, roomID int64) bool {
	snap, err := s.Snapshot(ctx, spaceID)
	if err != nil {
		return false
	}
	return snap.openToEveryone(roomID)
}

// roleViewBits are the role permissions the view rule reads: changing one can change what the
// role's holders see in any room.
const roleViewBits = permissions.PermViewRoom | permissions.PermAdministrator | permissions.PermManageThreads

// roleViewChanged reports whether a role's permissions changed in a way that touches them.
func roleViewChanged(before, after int64) bool {
	return (before^after)&roleViewBits != 0
}
