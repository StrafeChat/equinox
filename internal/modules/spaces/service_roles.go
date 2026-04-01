package spaces

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

var (
	ErrRoleNotFound       = errors.New("role not found")
	ErrCannotEditEveryone = errors.New("cannot rename or delete the @everyone role")
	ErrMissingPerm        = errors.New("missing permission")
	ErrInvalidRoom        = errors.New("room not in this space")
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
