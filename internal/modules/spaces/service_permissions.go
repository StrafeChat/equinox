package spaces

import (
	"context"
	"sort"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

// EffectiveChannelPermissions returns merged space role bits plus room allow/deny overwrites.
// Space owner gets full channel permissions (bypasses room denies). Administrator bit bypasses room denies.
func (s *Service) EffectiveChannelPermissions(ctx context.Context, userID, spaceID, roomID int64) (int64, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return 0, ErrSpaceNotFound
	}
	if sp.OwnerID == userID {
		return permissions.AllRoom, nil
	}
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return 0, err
	}
	mem, err := s.repo.GetMember(ctx, spaceID, userID)
	if err != nil || mem == nil {
		return 0, ErrNotMember
	}
	roleRows, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return 0, err
	}
	byID := make(map[int64]*SpaceRole, len(roleRows))
	for i := range roleRows {
		byID[roleRows[i].ID] = &roleRows[i]
	}
	var base int64
	if r := byID[everyoneID]; r != nil {
		base = r.Permissions
	}
	for _, rid := range mem.RoleIDs {
		if rid == everyoneID {
			continue
		}
		if r := byID[rid]; r != nil {
			base |= r.Permissions
		}
	}
	if permissions.Has(base, permissions.PermAdministrator) {
		return permissions.AllRoom, nil
	}
	ovs, err := s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
	if err != nil {
		return 0, err
	}
	ovByRole := make(map[int64]SpaceRoomRoleOverride, len(ovs))
	for _, o := range ovs {
		ovByRole[o.RoleID] = o
	}
	roleOrder := make([]int64, 0, 1+len(mem.RoleIDs))
	roleOrder = append(roleOrder, everyoneID)
	type ridPos struct {
		id  int64
		pos int
	}
	var rest []ridPos
	seen := map[int64]struct{}{everyoneID: {}}
	for _, rid := range mem.RoleIDs {
		if _, ok := seen[rid]; ok {
			continue
		}
		seen[rid] = struct{}{}
		p := 0x7fffffff
		if r := byID[rid]; r != nil {
			p = r.Position
		}
		rest = append(rest, ridPos{id: rid, pos: p})
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].pos != rest[j].pos {
			return rest[i].pos < rest[j].pos
		}
		return rest[i].id < rest[j].id
	})
	for _, rp := range rest {
		roleOrder = append(roleOrder, rp.id)
	}
	var stack []struct {
		Allow, Deny int64
	}
	for _, rid := range roleOrder {
		if o, ok := ovByRole[rid]; ok {
			stack = append(stack, struct {
				Allow, Deny int64
			}{Allow: o.Allow, Deny: o.Deny})
		}
	}
	return permissions.ApplyOverwrites(base, stack), nil
}

// SpacePermissionBase is OR of @everyone + member roles (no room overrides). For management checks.
func (s *Service) SpacePermissionBase(ctx context.Context, userID, spaceID int64) (int64, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return 0, ErrSpaceNotFound
	}
	if sp.OwnerID == userID {
		return permissions.AllSpace, nil
	}
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return 0, err
	}
	mem, err := s.repo.GetMember(ctx, spaceID, userID)
	if err != nil || mem == nil {
		return 0, ErrNotMember
	}
	roleRows, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return 0, err
	}
	byID := make(map[int64]*SpaceRole, len(roleRows))
	for i := range roleRows {
		byID[roleRows[i].ID] = &roleRows[i]
	}
	var base int64
	if r := byID[everyoneID]; r != nil {
		base = r.Permissions
	}
	for _, rid := range mem.RoleIDs {
		if rid == everyoneID {
			continue
		}
		if r := byID[rid]; r != nil {
			base |= r.Permissions
		}
	}
	return base, nil
}

func (s *Service) ensureEveryoneRoleID(ctx context.Context, sp *Space) (int64, error) {
	if sp.EveryoneRoleID != 0 {
		return sp.EveryoneRoleID, nil
	}
	return s.bootstrapEveryoneRole(ctx, sp.ID)
}

func (s *Service) bootstrapEveryoneRole(ctx context.Context, spaceID int64) (int64, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return 0, ErrSpaceNotFound
	}
	if sp.EveryoneRoleID != 0 {
		return sp.EveryoneRoleID, nil
	}
	rid := id.Next()
	now := time.Now().UTC()
	role := &SpaceRole{
		SpaceID:     spaceID,
		ID:          rid,
		Name:        EveryoneRoleName,
		Permissions: permissions.DefaultEveryone,
		Position:    0,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.repo.InsertSpaceRole(ctx, role); err != nil {
		return 0, err
	}
	if err := s.repo.UpdateEveryoneRoleID(ctx, spaceID, rid); err != nil {
		return 0, err
	}
	return rid, nil
}

// IsMember implements rooms.SpaceMemberChecker.
func (s *Service) IsMember(ctx context.Context, spaceID, userID int64) (bool, error) {
	return s.repo.IsMember(ctx, spaceID, userID)
}
