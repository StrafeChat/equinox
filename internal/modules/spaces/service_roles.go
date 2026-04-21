package spaces

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

var (
	ErrRoleNotFound       = errors.New("role not found")
	ErrCannotEditEveryone = errors.New("cannot rename or delete the @everyone role")
	ErrMissingPerm        = errors.New("missing permission")
	ErrInvalidRoom        = errors.New("room not in this space")
	ErrInvalidSlowmode    = errors.New("invalid slowmode")
	ErrInvalidRoomType    = errors.New("invalid room type")
	ErrInvalidReorder     = errors.New("invalid room order")
)

func (s *Service) canManageRoles(ctx context.Context, actorID, spaceID int64) error {
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if permissions.Has(base, permissions.PermManageRoles) || permissions.Has(base, permissions.PermAdministrator) {
		return nil
	}
	return ErrMissingPerm
}

func (s *Service) canManageRooms(ctx context.Context, actorID, spaceID int64) error {
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if permissions.Has(base, permissions.PermManageRooms) || permissions.Has(base, permissions.PermAdministrator) {
		return nil
	}
	return ErrMissingPerm
}

// ListSpaceRoles returns all roles in a space. Any member may list.
func (s *Service) ListSpaceRoles(ctx context.Context, actorID, spaceID int64) ([]SpaceRole, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	if _, err := s.ensureEveryoneRoleID(ctx, sp); err != nil {
		return nil, err
	}
	rows, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Position != rows[j].Position {
			return rows[i].Position < rows[j].Position
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

// CreateSpaceRole creates a custom role. Requires ManageRoles (or owner via SpacePermissionBase).
func (s *Service) CreateSpaceRole(ctx context.Context, actorID, spaceID int64, in *CreateSpaceRoleInput) (*SpaceRole, error) {
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || strings.EqualFold(name, EveryoneRoleName) {
		return nil, errors.New("invalid role name")
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	_, err = s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return nil, err
	}
	existing, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	maxPos := 0
	for _, r := range existing {
		if r.Position > maxPos {
			maxPos = r.Position
		}
	}
	rid := id.Next()
	now := time.Now().UTC()
	role := &SpaceRole{
		SpaceID:     spaceID,
		ID:          rid,
		Name:        name,
		Permissions: in.Permissions,
		Position:    maxPos + 1,
		Color:       in.Color,
		Hoist:       in.Hoist,
		Mentionable: in.Mentionable,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.repo.InsertSpaceRole(ctx, role); err != nil {
		return nil, err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_CREATE", spaceRoleEventData(role))
	return role, nil
}

// UpdateSpaceRole updates a role. @everyone may only have permissions changed (not name).
func (s *Service) UpdateSpaceRole(ctx context.Context, actorID, spaceID, roleID int64, in *UpdateSpaceRoleInput) (*SpaceRole, error) {
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return nil, err
	}
	role, err := s.repo.GetSpaceRole(ctx, spaceID, roleID)
	if err != nil || role == nil {
		return nil, ErrRoleNotFound
	}
	if roleID == everyoneID {
		if in.Name != nil && *in.Name != role.Name {
			return nil, ErrCannotEditEveryone
		}
		if in.Position != nil {
			return nil, ErrCannotEditEveryone
		}
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || strings.EqualFold(n, EveryoneRoleName) && roleID != everyoneID {
			return nil, errors.New("invalid role name")
		}
		if roleID != everyoneID {
			role.Name = n
		}
	}
	if in.Permissions != nil {
		role.Permissions = *in.Permissions
	}
	if in.Position != nil && roleID != everyoneID {
		role.Position = *in.Position
	}
	if in.Color != nil {
		role.Color = *in.Color
	}
	if in.Hoist != nil {
		role.Hoist = *in.Hoist
	}
	if in.Mentionable != nil {
		role.Mentionable = *in.Mentionable
	}
	role.UpdatedAt = time.Now().UTC()
	if err := s.repo.UpdateSpaceRole(ctx, role); err != nil {
		return nil, err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_UPDATE", spaceRoleEventData(role))
	return role, nil
}

// DeleteSpaceRole removes a custom role.
func (s *Service) DeleteSpaceRole(ctx context.Context, actorID, spaceID, roleID int64) error {
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return ErrSpaceNotFound
	}
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return err
	}
	if roleID == everyoneID {
		return ErrCannotEditEveryone
	}
	if err := s.repo.DeleteSpaceRole(ctx, spaceID, roleID); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_DELETE", map[string]interface{}{
		"role_id": id.Format(roleID),
	})
	return nil
}

// SetMemberRoles replaces a member's roles. Caller must ManageRoles. @everyone is added if missing.
func (s *Service) SetMemberRoles(ctx context.Context, actorID, spaceID, targetUserID int64, roleIDs []int64) error {
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return ErrSpaceNotFound
	}
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return err
	}
	if sp.OwnerID == targetUserID {
		return errors.New("cannot change roles of the space owner")
	}
	mem, err := s.repo.GetMember(ctx, spaceID, targetUserID)
	if err != nil || mem == nil {
		return ErrNotMember
	}
	_ = mem
	seen := make(map[int64]struct{})
	var out []int64
	for _, rid := range roleIDs {
		if _, ok := seen[rid]; ok {
			continue
		}
		if rid == everyoneID {
			continue
		}
		r, err := s.repo.GetSpaceRole(ctx, spaceID, rid)
		if err != nil || r == nil {
			return ErrRoleNotFound
		}
		seen[rid] = struct{}{}
		out = append(out, rid)
	}
	out = append([]int64{everyoneID}, out...)
	if err := s.repo.SetMemberRoleIDs(ctx, spaceID, targetUserID, out); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_MEMBER_UPDATE", map[string]interface{}{
		"user_id":   id.Format(targetUserID),
		"role_ids":  formatRoleIDStrings(out),
	})
	return nil
}

// ListRoomRoleOverrides returns overrides for a channel. Any member.
func (s *Service) ListRoomRoleOverrides(ctx context.Context, actorID, spaceID, roomID int64) ([]SpaceRoomRoleOverride, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return nil, err
	}
	return s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
}

// ListRoomUserOverrides returns user-specific overrides for a channel. Any member.
func (s *Service) ListRoomUserOverrides(ctx context.Context, actorID, spaceID, roomID int64) ([]SpaceRoomUserOverride, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return nil, err
	}
	return s.repo.ListRoomUserOverrides(ctx, spaceID, roomID)
}

// PutRoomRoleOverride sets allow/deny for a role in a room. Requires ManageRooms.
func (s *Service) PutRoomRoleOverride(ctx context.Context, actorID, spaceID, roomID, roleID int64, in *PutRoomRoleOverrideInput) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return ErrSpaceNotFound
	}
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return err
	}
	r, err := s.repo.GetSpaceRole(ctx, spaceID, roleID)
	if err != nil || r == nil {
		return ErrRoleNotFound
	}
	_ = everyoneID
	now := time.Now().UTC()
	o := &SpaceRoomRoleOverride{
		SpaceID:   spaceID,
		RoomID:    roomID,
		RoleID:    roleID,
		Allow:     in.Allow,
		Deny:      in.Deny,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.UpsertRoomRoleOverride(ctx, o); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_OVERRIDE_UPDATE", roomOverrideEventData(o))
	return nil
}

// PutRoomUserOverride sets allow/deny for a user in a room. Requires ManageRooms.
func (s *Service) PutRoomUserOverride(ctx context.Context, actorID, spaceID, roomID, targetUserID int64, in *PutRoomUserOverrideInput) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	member, err := s.repo.GetMember(ctx, spaceID, targetUserID)
	if err != nil {
		return err
	}
	if member == nil {
		return ErrNotMember
	}
	now := time.Now().UTC()
	o := &SpaceRoomUserOverride{
		SpaceID:   spaceID,
		RoomID:    roomID,
		UserID:    targetUserID,
		Allow:     in.Allow,
		Deny:      in.Deny,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.UpsertRoomUserOverride(ctx, o); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_UPDATE", roomUserOverrideEventData(o))
	return nil
}

// DeleteRoomRoleOverride removes an override row.
func (s *Service) DeleteRoomRoleOverride(ctx context.Context, actorID, spaceID, roomID, roleID int64) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	if err := s.repo.DeleteRoomRoleOverride(ctx, spaceID, roomID, roleID); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_OVERRIDE_DELETE", map[string]interface{}{
		"room_id": id.Format(roomID),
		"role_id": id.Format(roleID),
	})
	return nil
}

// DeleteRoomUserOverride removes a user override row.
func (s *Service) DeleteRoomUserOverride(ctx context.Context, actorID, spaceID, roomID, targetUserID int64) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	if err := s.repo.DeleteRoomUserOverride(ctx, spaceID, roomID, targetUserID); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_DELETE", map[string]interface{}{
		"room_id": id.Format(roomID),
		"user_id": id.Format(targetUserID),
	})
	return nil
}

func (s *Service) assertRoomInSpace(ctx context.Context, spaceID, roomID int64) error {
	room, err := s.roomRepo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrInvalidRoom
	}
	if room.SpaceID == nil || *room.SpaceID != spaceID {
		return ErrInvalidRoom
	}
	return nil
}

// UpdateRoom updates a room's basic metadata. Requires ManageRooms.
func (s *Service) UpdateRoom(ctx context.Context, actorID, spaceID, roomID int64, in *UpdateRoomInput) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	if in.SlowmodeSeconds < 0 || in.SlowmodeSeconds > 21600 {
		return ErrInvalidSlowmode
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "unnamed"
	}
	topic := strings.TrimSpace(in.Topic)
	if err := s.roomRepo.UpdateSpaceRoom(ctx, roomID, name, topic, in.SlowmodeSeconds); err != nil {
		return err
	}
	room, _ := s.roomRepo.GetByID(ctx, roomID)
	if room != nil {
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_UPDATE", map[string]interface{}{
			"room_id":          id.Format(room.ID),
			"name":             room.Name,
			"topic":            room.Topic,
			"slowmode_seconds": room.SlowmodeSeconds,
			"updated_at":       room.UpdatedAt,
		})
	}
	return nil
}

// DeleteRoom removes a room and its override rows. Requires ManageRooms.
func (s *Service) DeleteRoom(ctx context.Context, actorID, spaceID, roomID int64) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	if err := s.roomRepo.DeleteSpaceRoom(ctx, spaceID, roomID); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_DELETE", map[string]interface{}{
		"room_id": id.Format(roomID),
	})
	return nil
}

// CreateRoom creates a new room or section in the space. Requires ManageRooms.
func (s *Service) CreateRoom(ctx context.Context, actorID, spaceID int64, in *CreateRoomInput) (*rooms.Room, error) {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	if in.Type != rooms.TypeSpaceText && in.Type != rooms.TypeSpaceVoice && in.Type != rooms.TypeRoomSection {
		return nil, ErrInvalidRoomType
	}
	if in.ParentID != nil {
		if err := s.assertRoomInSpace(ctx, spaceID, *in.ParentID); err != nil {
			return nil, err
		}
		parent, err := s.roomRepo.GetByID(ctx, *in.ParentID)
		if err != nil || parent == nil || parent.Type != rooms.TypeRoomSection {
			return nil, ErrInvalidRoom
		}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "new-room"
	}
	rows, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	maxPos := 0
	for _, row := range rows {
		if row.Position > maxPos {
			maxPos = row.Position
		}
	}
	roomID := id.Next()
	sp, _ := s.repo.GetByID(ctx, spaceID)
	creatorID := actorID
	if sp != nil {
		creatorID = sp.OwnerID
	}
	e2eeOff := false
	room := &rooms.Room{
		ID:              roomID,
		Type:            in.Type,
		SpaceID:         &spaceID,
		ParentID:        in.ParentID,
		Name:            name,
		Position:        maxPos + 1,
		CreatorID:       creatorID,
		SlowmodeSeconds: 0,
	}
	if in.Type == rooms.TypeSpaceText || in.Type == rooms.TypeSpaceVoice {
		room.E2EEEnabled = &e2eeOff
	}
	if err := s.roomRepo.CreateSpaceRoom(ctx, room); err != nil {
		return nil, err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_CREATE", map[string]interface{}{
		"room_id":          id.Format(room.ID),
		"type":             room.Type,
		"name":             room.Name,
		"space_id":         id.Format(spaceID),
		"position":         room.Position,
		"slowmode_seconds": room.SlowmodeSeconds,
		"created_at":       room.CreatedAt,
		"updated_at":       room.UpdatedAt,
		"parent_id": func() interface{} {
			if room.ParentID == nil {
				return nil
			}
			return id.Format(*room.ParentID)
		}(),
	})
	return room, nil
}

func int64PtrEqual(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return *a == *b
}

func removeInt64FromSlice(ids []int64, id int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func multisetEqualInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	ca := make(map[int64]int, len(a))
	cb := make(map[int64]int, len(b))
	for _, x := range a {
		ca[x]++
	}
	for _, x := range b {
		cb[x]++
	}
	if len(ca) != len(cb) {
		return false
	}
	for k, v := range ca {
		if cb[k] != v {
			return false
		}
	}
	return true
}

func (s *Service) listRoomIDsForReorderGroup(ctx context.Context, spaceID int64, scope string, parentSectionID *int64) ([]int64, error) {
	rows, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })
	var out []int64
	for _, row := range rows {
		room, err := s.roomRepo.GetByID(ctx, row.RoomID)
		if err != nil || room == nil || room.SpaceID == nil || *room.SpaceID != spaceID {
			continue
		}
		switch scope {
		case "sections":
			if room.Type == rooms.TypeRoomSection && room.ParentID == nil {
				out = append(out, room.ID)
			}
		case "channels":
			if room.Type != rooms.TypeSpaceText && room.Type != rooms.TypeSpaceVoice {
				continue
			}
			if parentSectionID == nil {
				if room.ParentID == nil {
					out = append(out, room.ID)
				}
			} else {
				if room.ParentID != nil && *room.ParentID == *parentSectionID {
					out = append(out, room.ID)
				}
			}
		default:
			return nil, ErrInvalidReorder
		}
	}
	return out, nil
}

// ReorderRooms updates positions for a full sibling group (sections, top-level channels, or channels under one section).
func (s *Service) ReorderRooms(ctx context.Context, actorID, spaceID int64, in *ReorderRoomsInput) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if in == nil || len(in.RoomIDs) == 0 {
		return ErrInvalidReorder
	}
	var parentSectionID *int64
	if in.ParentSectionID != nil && strings.TrimSpace(*in.ParentSectionID) != "" {
		pid, err := id.Parse(strings.TrimSpace(*in.ParentSectionID))
		if err != nil {
			return ErrInvalidReorder
		}
		parentSectionID = &pid
	}
	ordered := make([]int64, 0, len(in.RoomIDs))
	for _, sid := range in.RoomIDs {
		rid, err := id.Parse(strings.TrimSpace(sid))
		if err != nil {
			return ErrInvalidReorder
		}
		ordered = append(ordered, rid)
	}
	expected, err := s.listRoomIDsForReorderGroup(ctx, spaceID, in.Scope, parentSectionID)
	if err != nil {
		return err
	}
	if !multisetEqualInt64(expected, ordered) {
		return ErrInvalidReorder
	}
	for i, rid := range ordered {
		if err := s.roomRepo.UpdateSpaceRoomPosition(ctx, spaceID, rid, i); err != nil {
			return err
		}
		room, _ := s.roomRepo.GetByID(ctx, rid)
		if room == nil {
			continue
		}
		payload := map[string]interface{}{
			"room_id":    id.Format(rid),
			"position":   room.Position,
			"space_id":   id.Format(spaceID),
			"updated_at": room.UpdatedAt,
		}
		if room.ParentID != nil {
			payload["parent_id"] = id.Format(*room.ParentID)
		} else {
			payload["parent_id"] = nil
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_UPDATE", payload)
	}
	return nil
}

func (s *Service) orderedSpaceChannelIDs(ctx context.Context, spaceID int64, parentSectionID *int64) ([]int64, error) {
	rows, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })
	out := make([]int64, 0)
	for _, row := range rows {
		room, err := s.roomRepo.GetByID(ctx, row.RoomID)
		if err != nil || room == nil || room.SpaceID == nil || *room.SpaceID != spaceID {
			continue
		}
		if room.Type != rooms.TypeSpaceText && room.Type != rooms.TypeSpaceVoice {
			continue
		}
		if parentSectionID == nil {
			if room.ParentID == nil {
				out = append(out, room.ID)
			}
		} else {
			if room.ParentID != nil && *room.ParentID == *parentSectionID {
				out = append(out, room.ID)
			}
		}
	}
	return out, nil
}

// MoveSpaceChannel moves a text/voice channel to another parent (nil = top-level) and renumbers affected sibling positions.
// Use ReorderRooms when the channel stays in the same parent group.
func (s *Service) MoveSpaceChannel(ctx context.Context, actorID, spaceID, channelRoomID int64, newParentSectionID *int64, beforeRoomID *int64) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	ch, err := s.roomRepo.GetByID(ctx, channelRoomID)
	if err != nil || ch == nil || ch.SpaceID == nil || *ch.SpaceID != spaceID {
		return ErrInvalidRoom
	}
	if ch.Type != rooms.TypeSpaceText && ch.Type != rooms.TypeSpaceVoice {
		return ErrInvalidReorder
	}
	var newParent *int64
	if newParentSectionID != nil {
		parent, err := s.roomRepo.GetByID(ctx, *newParentSectionID)
		if err != nil || parent == nil || parent.Type != rooms.TypeRoomSection {
			return ErrInvalidReorder
		}
		if parent.SpaceID == nil || *parent.SpaceID != spaceID {
			return ErrInvalidReorder
		}
		newParent = newParentSectionID
	}
	if beforeRoomID != nil {
		br, err := s.roomRepo.GetByID(ctx, *beforeRoomID)
		if err != nil || br == nil || (br.Type != rooms.TypeSpaceText && br.Type != rooms.TypeSpaceVoice) {
			return ErrInvalidReorder
		}
		if br.SpaceID == nil || *br.SpaceID != spaceID {
			return ErrInvalidReorder
		}
		if !int64PtrEqual(br.ParentID, newParent) {
			return ErrInvalidReorder
		}
		if *beforeRoomID == channelRoomID {
			return ErrInvalidReorder
		}
	}

	oldParent := ch.ParentID
	if int64PtrEqual(oldParent, newParent) {
		return ErrInvalidReorder
	}

	oldPeers, err := s.orderedSpaceChannelIDs(ctx, spaceID, oldParent)
	if err != nil {
		return err
	}
	inOld := false
	for _, x := range oldPeers {
		if x == channelRoomID {
			inOld = true
			break
		}
	}
	if !inOld {
		return ErrInvalidReorder
	}
	destPeers, err := s.orderedSpaceChannelIDs(ctx, spaceID, newParent)
	if err != nil {
		return err
	}
	oldSans := removeInt64FromSlice(oldPeers, channelRoomID)
	destSans := removeInt64FromSlice(destPeers, channelRoomID)

	var newOrder []int64
	if beforeRoomID == nil {
		newOrder = append(append([]int64{}, destSans...), channelRoomID)
	} else {
		idx := -1
		for i, rid := range destSans {
			if rid == *beforeRoomID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return ErrInvalidReorder
		}
		newOrder = append(append(append([]int64{}, destSans[:idx]...), channelRoomID), destSans[idx:]...)
	}

	for i, rid := range oldSans {
		if err := s.roomRepo.UpdateSpaceRoomPosition(ctx, spaceID, rid, i); err != nil {
			return err
		}
	}
	for i, rid := range newOrder {
		if rid == channelRoomID {
			if err := s.roomRepo.UpdateSpaceRoomParentAndPosition(ctx, spaceID, rid, newParent, i); err != nil {
				return err
			}
		} else {
			if err := s.roomRepo.UpdateSpaceRoomPosition(ctx, spaceID, rid, i); err != nil {
				return err
			}
		}
	}

	published := make(map[int64]struct{})
	publishOne := func(rid int64) {
		if _, ok := published[rid]; ok {
			return
		}
		published[rid] = struct{}{}
		room, _ := s.roomRepo.GetByID(ctx, rid)
		if room == nil {
			return
		}
		payload := map[string]interface{}{
			"room_id":    id.Format(rid),
			"position":   room.Position,
			"space_id":   id.Format(spaceID),
			"updated_at": room.UpdatedAt,
		}
		if room.ParentID != nil {
			payload["parent_id"] = id.Format(*room.ParentID)
		} else {
			payload["parent_id"] = nil
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_UPDATE", payload)
	}
	for _, rid := range oldSans {
		publishOne(rid)
	}
	for _, rid := range newOrder {
		publishOne(rid)
	}
	return nil
}
