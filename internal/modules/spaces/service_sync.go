package spaces

import (
	"context"
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
	// Syncing changes the channel's effective View, so some members may gain or lose it.
	s.republishRoomVisibility(ctx, spaceID, []int64{room.ID})
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
// permission change on roomID: the room itself, plus - when it is a section - every channel
// synced to it (those inherit the section's overrides).
func (s *Service) visibilityAffectedRooms(ctx context.Context, spaceID, roomID int64) []int64 {
	ids := []int64{roomID}
	room, err := s.roomRepo.GetByID(ctx, roomID)
	if err != nil || room == nil || room.Type != rooms.TypeRoomSection {
		return ids
	}
	all, err := s.listRooms(ctx, spaceID)
	if err != nil {
		return ids
	}
	for _, r := range all {
		if r.ParentID != nil && *r.ParentID == roomID && r.PermissionsSynced != nil && *r.PermissionsSynced {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// republishRoomVisibility tells each member whether they can now see the given rooms, so a
// permission change adds or removes them from sidebars in real time (Discord-style) without a
// reload - including for a member who never had the room because it was created private.
// Members who can view get SPACE_ROOM_CREATE (the client upserts it); members who cannot get
// SPACE_ROOM_DELETE (the client drops it, which also clears any metadata it had cached). Runs
// only on override/sync changes, which are infrequent admin actions.
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
	region := s.stargateRegion()
	for _, rid := range roomIDs {
		room, err := s.roomRepo.GetByID(ctx, rid)
		if err != nil || room == nil || room.SpaceID == nil || *room.SpaceID != spaceID {
			continue
		}
		payload := AttachOverrides(RoomMap(room), snap.RoomOverridesFor(rid))
		payload["room_id"] = id.Format(rid)
		ov := snap.EffectiveRoomOverrides(rid)
		var canView, cannotView []int64
		for i := range members {
			m := &members[i]
			visible := snap.OwnerID == m.UserID
			if !visible {
				base := snap.basePermissions(m.RoleIDs)
				if permissions.Has(base, permissions.PermAdministrator) {
					visible = true
				} else {
					perms := resolveEffectiveRoomPermissions(base, snap.EveryoneRoleID, m.RoleIDs, m.UserID, ov.Roles, ov.Users)
					visible = permissions.Has(perms, permissions.PermViewRoom)
				}
			}
			if visible {
				canView = append(canView, m.UserID)
			} else {
				cannotView = append(cannotView, m.UserID)
			}
		}
		if len(canView) > 0 {
			stargate.PublishToUsers(ctx, s.redis, canView, "SPACE_ROOM_CREATE", payload, region)
		}
		if len(cannotView) > 0 {
			stargate.PublishToUsers(ctx, s.redis, cannotView, "SPACE_ROOM_DELETE", map[string]interface{}{"room_id": id.Format(rid), "space_id": id.Format(spaceID)}, region)
		}
	}
}
