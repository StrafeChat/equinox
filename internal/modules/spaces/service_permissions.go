package spaces

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

// EffectiveChannelPermissions returns merged space role bits plus room allow/deny overwrites.
// Space owner gets full channel permissions (bypasses room denies). Administrator bit bypasses room denies.
//
// Roles and overrides come from the space snapshot (one Redis read when cached); the only
// per-call database read is the member row.
func (s *Service) EffectiveChannelPermissions(ctx context.Context, userID, spaceID, roomID int64) (int64, error) {
	snap, err := s.permissionSnapshot(ctx, spaceID, roomID)
	if err != nil {
		return 0, err
	}
	if snap.OwnerID == userID {
		return permissions.AllRoom, nil
	}
	mem, err := s.repo.GetMember(ctx, spaceID, userID)
	if err != nil || mem == nil {
		return 0, ErrNotMember
	}
	base := snap.basePermissions(mem.RoleIDs)
	if permissions.Has(base, permissions.PermAdministrator) {
		return permissions.AllRoom, nil
	}
	ov := snap.EffectiveRoomOverrides(roomID)
	return resolveEffectiveRoomPermissions(base, snap.EveryoneRoleID, mem.RoleIDs, userID, ov.Roles, ov.Users), nil
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
	return applyImplicitRoomRules(perms)
}

// applyImplicitRoomRules is Discord's two dependent-permission rules, applied after the
// overwrite stack so every consumer (messages, voice, search, the gateway, the client's
// mirror of this function) agrees: a member who cannot View Room has no permission at all in
// that room, and one who cannot Send Messages cannot do the things that only make sense
// while sending - attach files or mention @everyone.
func applyImplicitRoomRules(perms int64) int64 {
	if !permissions.Has(perms, permissions.PermViewRoom) {
		return 0
	}
	if !permissions.Has(perms, permissions.PermSendMessages) {
		perms &^= permissions.PermAttachFiles | permissions.PermMentionEveryone
	}
	return perms
}

// SpacePermissionBase is OR of @everyone + member roles (no room overrides). For management checks.
// The owner and anyone holding Administrator get every space bit, Discord's rule, so a
// management check never has to remember to test the Administrator bit separately (role
// hierarchy still applies to them - only the owner is above it).
func (s *Service) SpacePermissionBase(ctx context.Context, userID, spaceID int64) (int64, error) {
	snap, err := s.permissionSnapshot(ctx, spaceID, 0)
	if err != nil {
		return 0, err
	}
	if snap.OwnerID == userID {
		return permissions.AllSpace, nil
	}
	mem, err := s.repo.GetMember(ctx, spaceID, userID)
	if err != nil || mem == nil {
		return 0, ErrNotMember
	}
	base := snap.basePermissions(mem.RoleIDs)
	if permissions.Has(base, permissions.PermAdministrator) {
		return permissions.AllSpace, nil
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
	s.invalidateSnapshot(ctx, spaceID)
	return rid, nil
}

// IsMember implements rooms.SpaceMemberChecker.
func (s *Service) IsMember(ctx context.Context, spaceID, userID int64) (bool, error) {
	return s.repo.IsMember(ctx, spaceID, userID)
}
