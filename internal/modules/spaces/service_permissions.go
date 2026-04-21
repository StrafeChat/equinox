package spaces

import (
	"context"
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
	roleOverrides, err := s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
	if err != nil {
		return 0, err
	}
	userOverrides, err := s.repo.ListRoomUserOverrides(ctx, spaceID, roomID)
	if err != nil {
		return 0, err
	}
	return resolveEffectiveRoomPermissions(base, everyoneID, mem.RoleIDs, userID, roleOverrides, userOverrides), nil
}

func resolveEffectiveRoomPermissions(
	base int64,
	everyoneID int64,
	memberRoleIDs []int64,
	userID int64,
	roleOverrides []SpaceRoomRoleOverride,
	userOverrides []SpaceRoomUserOverride,
) int64 {
	perms := base

	// 1) Apply @everyone room override (deny, then allow).
	for _, o := range roleOverrides {
		if o.RoleID != everyoneID {
			continue
		}
		perms &= ^o.Deny
		perms |= o.Allow
		break
	}

	// 2) Aggregate all role overrides member has (excluding @everyone), deny then allow.
	memberRoleSet := make(map[int64]struct{}, len(memberRoleIDs))
	for _, rid := range memberRoleIDs {
		if rid == everyoneID {
			continue
		}
		memberRoleSet[rid] = struct{}{}
	}
	var rolesDeny int64
	var rolesAllow int64
	for _, o := range roleOverrides {
		if _, ok := memberRoleSet[o.RoleID]; !ok {
			continue
		}
		rolesDeny |= o.Deny
		rolesAllow |= o.Allow
	}
	perms &= ^rolesDeny
	perms |= rolesAllow

	// 3) Apply user-specific room override, if any (deny then allow).
	for _, o := range userOverrides {
		if o.UserID != userID {
			continue
		}
		perms &= ^o.Deny
		perms |= o.Allow
		break
	}
	return perms
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
