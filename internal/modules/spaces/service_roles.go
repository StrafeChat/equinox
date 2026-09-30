package spaces

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

var (
	ErrRoleNotFound           = errors.New("role not found")
	ErrCannotEditEveryone     = errors.New("cannot rename or delete the @everyone role")
	ErrMissingPerm            = errors.New("missing permission")
	ErrInvalidRoom            = errors.New("room not in this space")
	ErrInvalidSlowmode        = errors.New("invalid slowmode")
	ErrInvalidRoomType        = errors.New("invalid room type")
	ErrInvalidReorder         = errors.New("invalid room order")
	ErrInvalidRoleName        = errors.New("role name must be 1-100 characters and not @everyone")
	ErrInvalidRoomName        = errors.New("room name must be at most 100 characters")
	ErrInvalidTopic           = errors.New("topic must be at most 1024 characters")
	ErrInvalidUserLimit       = errors.New("user limit must be 0 (unlimited) to 99")
	ErrInvalidBitrate         = errors.New("bitrate must be 8000-384000 bits per second, or 0 for the default")
	ErrCannotChangeOwnerRoles = errors.New("cannot change roles of the space owner")
	// ErrRoleHierarchy mirrors Discord: a non-owner can't manage a role at or above their own
	// highest role's position, and can't act on a member whose highest role outranks theirs.
	// Otherwise a Manage Roles holder could create an Administrator role and grant it to
	// themselves, or edit/demote staff above them.
	ErrRoleHierarchy = errors.New("cannot manage a role or member at or above your own highest role")
	// ErrPermissionEscalation mirrors Discord's other rule: you can only hand out
	// permissions you hold yourself. Without it a Manage Roles holder could set
	// Administrator on @everyone (which the hierarchy check deliberately exempts) and
	// become an administrator of the whole space.
	ErrPermissionEscalation = errors.New("cannot grant permissions you do not have")
	// ErrManagedRole: the role belongs to a bot (SpaceRole.BotID) - it can be edited, but
	// only its bot may hold it and it is removed when the bot leaves, never by hand.
	ErrManagedRole = errors.New("this role belongs to a bot and cannot be deleted or assigned")
)

const (
	MaxRoleNameRunes = 100
	MaxRoomNameRunes = 100
	MaxTopicRunes    = 1024
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

// grantable returns the space-wide permission bits the actor may hand out: everything
// for the owner and Administrators, otherwise exactly the bits they hold.
func (s *Service) grantable(ctx context.Context, actorID, spaceID int64) (int64, error) {
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return 0, err
	}
	if permissions.Has(base, permissions.PermAdministrator) {
		return permissions.AllSpace, nil
	}
	return base, nil
}

// checkGrant rejects a permission set that includes bits outside what the actor may grant.
func (s *Service) checkGrant(ctx context.Context, actorID, spaceID int64, perms int64) error {
	allowed, err := s.grantable(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if perms&^allowed != 0 {
		return ErrPermissionEscalation
	}
	return nil
}

func validRoleName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= MaxRoleNameRunes && !strings.EqualFold(name, EveryoneRoleName)
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

// highestRolePosition returns the highest position among roleIDs, excluding @everyone
// (always position 0, the floor everyone shares). Returns -1 if none apply.
func highestRolePosition(roleIDs []int64, everyoneID int64, byID map[int64]*SpaceRole) int {
	highest := -1
	for _, rid := range roleIDs {
		if rid == everyoneID {
			continue
		}
		if r := byID[rid]; r != nil && r.Position > highest {
			highest = r.Position
		}
	}
	return highest
}

// memberRoleContext loads what role-hierarchy checks need: the @everyone id, a role
// lookup map, and the actor's own highest role position (irrelevant if actor is owner).
func (s *Service) memberRoleContext(ctx context.Context, spaceID, actorID int64) (everyoneID int64, byID map[int64]*SpaceRole, actorHighest int, err error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return 0, nil, 0, ErrSpaceNotFound
	}
	everyoneID, err = s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return 0, nil, 0, err
	}
	roles, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return 0, nil, 0, err
	}
	byID = make(map[int64]*SpaceRole, len(roles))
	for i := range roles {
		byID[roles[i].ID] = &roles[i]
	}
	mem, err := s.repo.GetMember(ctx, spaceID, actorID)
	if err != nil {
		return 0, nil, 0, err
	}
	actorHighest = -1
	if mem != nil {
		actorHighest = highestRolePosition(mem.RoleIDs, everyoneID, byID)
	}
	return everyoneID, byID, actorHighest, nil
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
	return s.SpaceRoles(ctx, spaceID)
}

// CreateSpaceRole creates a custom role. Requires ManageRoles (or owner via SpacePermissionBase).
func (s *Service) CreateSpaceRole(ctx context.Context, actorID, spaceID int64, in *CreateSpaceRoleInput) (*SpaceRole, error) {
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if !validRoleName(name) {
		return nil, ErrInvalidRoleName
	}
	if err := s.checkGrant(ctx, actorID, spaceID, in.Permissions); err != nil {
		return nil, err
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
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_CREATE", spaceRoleEventData(role))
	s.audit(ctx, spaceID, actorID, AuditRoleCreate, id.Format(role.ID), map[string]change{
		"name":        {New: role.Name},
		"permissions": {New: role.Permissions},
	}, "")
	return role, nil
}

// UpdateSpaceRole updates a role. @everyone may only have permissions changed (not name).
// Non-owners cannot edit a role at or above their own highest role (Discord-style hierarchy).
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
	before := *role
	if roleID != everyoneID && sp.OwnerID != actorID {
		_, _, actorHighest, err := s.memberRoleContext(ctx, spaceID, actorID)
		if err != nil {
			return nil, err
		}
		if role.Position >= actorHighest {
			return nil, ErrRoleHierarchy
		}
		// Moving the role to or above the actor's own rank is the same escalation as
		// editing a role that is already there.
		if in.Position != nil && *in.Position >= actorHighest {
			return nil, ErrRoleHierarchy
		}
	}
	if roleID == everyoneID {
		if in.Name != nil && *in.Name != role.Name {
			return nil, ErrCannotEditEveryone
		}
		if in.Position != nil {
			return nil, ErrCannotEditEveryone
		}
	}
	if in.Name != nil && roleID != everyoneID {
		n := strings.TrimSpace(*in.Name)
		if !validRoleName(n) {
			return nil, ErrInvalidRoleName
		}
		role.Name = n
	}
	if in.Permissions != nil {
		// Applies to @everyone too - that role is exempt from the hierarchy check above,
		// so this is the only thing stopping a Manage Roles holder from granting
		// Administrator to every member, themselves included.
		if err := s.checkGrant(ctx, actorID, spaceID, *in.Permissions); err != nil {
			return nil, err
		}
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
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_UPDATE", spaceRoleEventData(role))
	changes := map[string]change{}
	diff(changes, "name", before.Name, role.Name)
	diff(changes, "permissions", before.Permissions, role.Permissions)
	diff(changes, "position", before.Position, role.Position)
	diff(changes, "color", before.Color, role.Color)
	diff(changes, "hoist", before.Hoist, role.Hoist)
	diff(changes, "mentionable", before.Mentionable, role.Mentionable)
	if len(changes) > 0 {
		s.audit(ctx, spaceID, actorID, AuditRoleUpdate, id.Format(roleID), changes, "")
	}
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
	role, err := s.repo.GetSpaceRole(ctx, spaceID, roleID)
	if err != nil {
		return err
	}
	if role == nil {
		return ErrRoleNotFound
	}
	if role.IsManaged() {
		return ErrManagedRole
	}
	if sp.OwnerID != actorID {
		_, _, actorHighest, err := s.memberRoleContext(ctx, spaceID, actorID)
		if err != nil {
			return err
		}
		if role.Position >= actorHighest {
			return ErrRoleHierarchy
		}
	}
	if err := s.repo.DeleteSpaceRole(ctx, spaceID, roleID); err != nil {
		return err
	}
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_DELETE", map[string]interface{}{
		"role_id": id.Format(roleID),
	})
	s.audit(ctx, spaceID, actorID, AuditRoleDelete, id.Format(roleID), map[string]change{"name": {Old: role.Name}}, "")
	return nil
}

// SetMemberRoles replaces a member's roles. Caller must ManageRoles. @everyone is added if missing.
//
// Discord-style hierarchy, with the same carve-out Discord makes for yourself:
//   - Only the owner may change the owner's roles - but they *may*, which is how the owner
//     gives themselves a colour or a hoisted title.
//   - A non-owner may edit their own roles too (you do not outrank yourself), and anyone
//     else whose highest role is below their own.
//   - Either way, a role at or above the actor's own highest can neither be handed out nor
//     taken away: roles the target already holds up there are carried over untouched,
//     matching the locked checkboxes the client draws for them.
func (s *Service) SetMemberRoles(ctx context.Context, actorID, spaceID, targetUserID int64, roleIDs []int64) error {
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return ErrSpaceNotFound
	}
	isSelf := actorID == targetUserID
	if sp.OwnerID == targetUserID && !isSelf {
		return ErrCannotChangeOwnerRoles
	}
	mem, err := s.repo.GetMember(ctx, spaceID, targetUserID)
	if err != nil || mem == nil {
		return ErrNotMember
	}
	everyoneID, byID, actorHighest, err := s.memberRoleContext(ctx, spaceID, actorID)
	if err != nil {
		return err
	}
	isOwner := sp.OwnerID == actorID
	if !isOwner && !isSelf && highestRolePosition(mem.RoleIDs, everyoneID, byID) >= actorHighest {
		return ErrRoleHierarchy
	}
	// A bot's own role is nobody else's to hold, and nobody's to take away from the bot.
	for _, rid := range roleIDs {
		if r := byID[rid]; r != nil && r.IsManaged() && r.BotID != targetUserID {
			return ErrManagedRole
		}
	}
	out, err := resolveMemberRoles(roleIDs, mem.RoleIDs, everyoneID, byID, isOwner, actorHighest)
	if err != nil {
		return err
	}
	out = keepManagedRoles(out, mem.RoleIDs, targetUserID, byID)
	if err := s.repo.SetMemberRoleIDs(ctx, spaceID, targetUserID, out); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_MEMBER_UPDATE", map[string]interface{}{
		"user_id":  id.Format(targetUserID),
		"role_ids": formatRoleIDStrings(out),
	})
	s.audit(ctx, spaceID, actorID, AuditMemberRolesUpdate, id.Format(targetUserID), map[string]change{
		"roles": {New: formatRoleIDStrings(out)},
	}, "")
	return nil
}

// resolveMemberRoles turns a requested role list into the set to store: @everyone first,
// then the requested roles, plus any role the member already holds at or above the
// actor's own rank (the owner has no rank and no such roles).
//
// Carrying those over rather than dropping them is what makes editing your own roles
// safe: the request says nothing about a role you may not touch, so the stored set keeps
// it. Requesting a role at or above the actor's rank that the member does not already
// hold is the escalation case, and fails.
func resolveMemberRoles(
	requested, current []int64,
	everyoneID int64,
	byID map[int64]*SpaceRole,
	isOwner bool,
	actorHighest int,
) ([]int64, error) {
	seen := make(map[int64]struct{}, len(requested)+len(current))
	out := []int64{everyoneID}
	if !isOwner {
		for _, rid := range current {
			if rid == everyoneID {
				continue
			}
			if _, ok := seen[rid]; ok {
				continue
			}
			if r := byID[rid]; r != nil && r.Position >= actorHighest {
				seen[rid] = struct{}{}
				out = append(out, rid)
			}
		}
	}
	for _, rid := range requested {
		if rid == everyoneID {
			continue
		}
		if _, ok := seen[rid]; ok {
			continue
		}
		r := byID[rid]
		if r == nil {
			return nil, ErrRoleNotFound
		}
		if !isOwner && r.Position >= actorHighest {
			return nil, ErrRoleHierarchy
		}
		seen[rid] = struct{}{}
		out = append(out, rid)
	}
	return out, nil
}

// keepManagedRoles puts back any of the member's own bot roles a request left out: a
// managed role leaves with the bot, not through the roles editor.
func keepManagedRoles(out, current []int64, userID int64, byID map[int64]*SpaceRole) []int64 {
	have := make(map[int64]struct{}, len(out))
	for _, rid := range out {
		have[rid] = struct{}{}
	}
	for _, rid := range current {
		if _, ok := have[rid]; ok {
			continue
		}
		if r := byID[rid]; r != nil && r.IsManaged() && r.BotID == userID {
			out = append(out, rid)
		}
	}
	return out
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

// checkOverrideGrant applies the "only permissions you hold" rule to a room override:
// both the allow and the deny mask are limited to room-scoped bits the actor has in that
// room (owner and Administrators may set any room bit). Space-wide bits never belong in
// an override and are dropped.
func (s *Service) checkOverrideGrant(ctx context.Context, actorID, spaceID, roomID int64, allow, deny int64) (int64, int64, error) {
	allow &= permissions.AllRoom
	deny &= permissions.AllRoom
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return 0, 0, err
	}
	if permissions.Has(base, permissions.PermAdministrator) {
		return allow, deny, nil
	}
	effective, err := s.EffectiveChannelPermissions(ctx, actorID, spaceID, roomID)
	if err != nil {
		return 0, 0, err
	}
	if (allow|deny)&^effective != 0 {
		return 0, 0, ErrPermissionEscalation
	}
	return allow, deny, nil
}

// PutRoomRoleOverride sets allow/deny for a role in a room. Requires ManageRooms.
func (s *Service) PutRoomRoleOverride(ctx context.Context, actorID, spaceID, roomID, roleID int64, in *PutRoomRoleOverrideInput) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	r, err := s.repo.GetSpaceRole(ctx, spaceID, roleID)
	if err != nil || r == nil {
		return ErrRoleNotFound
	}
	allow, deny, err := s.checkOverrideGrant(ctx, actorID, spaceID, roomID, in.Allow, in.Deny)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	o := &SpaceRoomRoleOverride{
		SpaceID:   spaceID,
		RoomID:    roomID,
		RoleID:    roleID,
		Allow:     allow,
		Deny:      deny,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.UpsertRoomRoleOverride(ctx, o); err != nil {
		return err
	}
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_OVERRIDE_UPDATE", roomOverrideEventData(o))
	s.audit(ctx, spaceID, actorID, AuditOverrideUpdate, id.Format(roomID)+":role:"+id.Format(roleID), map[string]change{
		"allow": {New: o.Allow},
		"deny":  {New: o.Deny},
	}, "")
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
	allow, deny, err := s.checkOverrideGrant(ctx, actorID, spaceID, roomID, in.Allow, in.Deny)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	o := &SpaceRoomUserOverride{
		SpaceID:   spaceID,
		RoomID:    roomID,
		UserID:    targetUserID,
		Allow:     allow,
		Deny:      deny,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.UpsertRoomUserOverride(ctx, o); err != nil {
		return err
	}
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_UPDATE", roomUserOverrideEventData(o))
	s.audit(ctx, spaceID, actorID, AuditOverrideUpdate, id.Format(roomID)+":user:"+id.Format(targetUserID), map[string]change{
		"allow": {New: o.Allow},
		"deny":  {New: o.Deny},
	}, "")
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
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_OVERRIDE_DELETE", map[string]interface{}{
		"room_id": id.Format(roomID),
		"role_id": id.Format(roleID),
	})
	s.audit(ctx, spaceID, actorID, AuditOverrideDelete, id.Format(roomID)+":role:"+id.Format(roleID), nil, "")
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
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_DELETE", map[string]interface{}{
		"room_id": id.Format(roomID),
		"user_id": id.Format(targetUserID),
	})
	s.audit(ctx, spaceID, actorID, AuditOverrideDelete, id.Format(roomID)+":user:"+id.Format(targetUserID), nil, "")
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

// UpdateRoom applies a partial update to a room's metadata and/or its E2EE setting.
// Requires ManageRooms.
func (s *Service) UpdateRoom(ctx context.Context, actorID, spaceID, roomID int64, in *UpdateRoomInput) error {
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if in == nil || (in.Name == nil && in.Topic == nil && in.SlowmodeSeconds == nil && in.E2EEEnabled == nil &&
		in.UserLimit == nil && in.Bitrate == nil) {
		return ErrNothingToPatch
	}
	room, err := s.roomRepo.GetByID(ctx, roomID)
	if err != nil || room == nil || room.SpaceID == nil || *room.SpaceID != spaceID {
		return ErrInvalidRoom
	}

	// Metadata: start from what's stored and overlay only the fields the request carried.
	name, topic, slowmode := room.Name, room.Topic, room.SlowmodeSeconds
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
		if name == "" {
			name = "unnamed"
		}
		if utf8.RuneCountInString(name) > MaxRoomNameRunes {
			return ErrInvalidRoomName
		}
	}
	if in.Topic != nil {
		topic = strings.TrimSpace(*in.Topic)
		if utf8.RuneCountInString(topic) > MaxTopicRunes {
			return ErrInvalidTopic
		}
	}
	if in.SlowmodeSeconds != nil {
		slowmode = *in.SlowmodeSeconds
		if slowmode < 0 || slowmode > 21600 {
			return ErrInvalidSlowmode
		}
	}
	if in.Name != nil || in.Topic != nil || in.SlowmodeSeconds != nil {
		if err := s.roomRepo.UpdateSpaceRoom(ctx, roomID, name, topic, slowmode); err != nil {
			return err
		}
	}

	// E2EE toggle. Only text rooms carry message content; sections have nothing to encrypt
	// and voice isn't implemented, so reject the flag anywhere else rather than storing a
	// setting nothing reads.
	e2eeTurnedOn := false
	if in.E2EEEnabled != nil {
		if room.Type != rooms.TypeSpaceText {
			return ErrInvalidRoomType
		}
		current := room.E2EEEnabled != nil && *room.E2EEEnabled
		if *in.E2EEEnabled != current {
			if err := s.roomRepo.UpdateRoomE2EEEnabled(ctx, roomID, *in.E2EEEnabled); err != nil {
				return err
			}
			e2eeTurnedOn = *in.E2EEEnabled
		}
	}

	// Voice settings. Only voice rooms have a connection cap or a bitrate.
	if in.UserLimit != nil || in.Bitrate != nil {
		if room.Type != rooms.TypeSpaceVoice {
			return ErrInvalidRoomType
		}
		userLimit, bitrate, err := voiceSettings(room, in.UserLimit, in.Bitrate)
		if err != nil {
			return err
		}
		if userLimit != room.UserLimit || bitrate != room.Bitrate {
			if err := s.roomRepo.UpdateVoiceSettings(ctx, roomID, userLimit, bitrate); err != nil {
				return err
			}
		}
	}

	updated, _ := s.roomRepo.GetByID(ctx, roomID)
	if updated != nil {
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_UPDATE", map[string]interface{}{
			"room_id":          id.Format(updated.ID),
			"name":             updated.Name,
			"topic":            updated.Topic,
			"slowmode_seconds": updated.SlowmodeSeconds,
			"e2ee_enabled":     updated.E2EEEnabled != nil && *updated.E2EEEnabled,
			"user_limit":       updated.UserLimit,
			"bitrate":          updated.Bitrate,
			"updated_at":       updated.UpdatedAt,
		})
		changes := map[string]change{}
		diff(changes, "name", room.Name, updated.Name)
		diff(changes, "topic", room.Topic, updated.Topic)
		diff(changes, "slowmode_seconds", room.SlowmodeSeconds, updated.SlowmodeSeconds)
		diff(changes, "e2ee_enabled", room.E2EEEnabled != nil && *room.E2EEEnabled, updated.E2EEEnabled != nil && *updated.E2EEEnabled)
		diff(changes, "user_limit", room.UserLimit, updated.UserLimit)
		diff(changes, "bitrate", room.Bitrate, updated.Bitrate)
		if len(changes) > 0 {
			s.audit(ctx, spaceID, actorID, AuditRoomUpdate, id.Format(roomID), changes, "")
		}
	}
	if e2eeTurnedOn {
		// Clients only rotate a room's Megolm session on membership changes while the room
		// is E2EE (see web's e2eeSync); a session established during an earlier E2EE stint
		// could therefore still be shared with people who have since left. Tell every
		// client to drop any cached outbound session so the first message under the new
		// setting is encrypted under a fresh key shared with the *current* member set.
		s.publishSpaceEvent(ctx, spaceID, "SESSION_ROTATE", map[string]interface{}{
			"room_id": id.Format(roomID),
		})
	}
	return nil
}

// voiceSettings overlays the requested user limit / bitrate on a voice room's current
// values and validates them. A bitrate of 0 means "client default".
func voiceSettings(room *rooms.Room, userLimit, bitrate *int) (int, int, error) {
	limit, rate := room.UserLimit, room.Bitrate
	if userLimit != nil {
		limit = *userLimit
		if limit < 0 || limit > rooms.MaxVoiceUserLimit {
			return 0, 0, ErrInvalidUserLimit
		}
	}
	if bitrate != nil {
		rate = *bitrate
		if rate != 0 && (rate < rooms.MinVoiceBitrate || rate > rooms.MaxVoiceBitrate) {
			return 0, 0, ErrInvalidBitrate
		}
	}
	return limit, rate, nil
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
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_DELETE", map[string]interface{}{
		"room_id": id.Format(roomID),
	})
	s.audit(ctx, spaceID, actorID, AuditRoomDelete, id.Format(roomID), nil, "")
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
	if utf8.RuneCountInString(name) > MaxRoomNameRunes {
		return nil, ErrInvalidRoomName
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
		// Off unless explicitly requested at creation, and only text rooms can opt in.
		e2ee := in.Type == rooms.TypeSpaceText && in.E2EEEnabled != nil && *in.E2EEEnabled
		room.E2EEEnabled = &e2ee
	}
	if in.Type == rooms.TypeSpaceVoice {
		userLimit, bitrate, err := voiceSettings(room, in.UserLimit, in.Bitrate)
		if err != nil {
			return nil, err
		}
		room.UserLimit, room.Bitrate = userLimit, bitrate
	}
	if err := s.roomRepo.CreateSpaceRoom(ctx, room); err != nil {
		return nil, err
	}
	s.invalidateSnapshot(ctx, spaceID)
	// Same shape as GET /spaces/:id/rooms (plus room_id), so clients can add the room to
	// their list straight from the event. A new room has no overrides yet.
	payload := AttachOverrides(RoomMap(room), RoomOverrides{})
	payload["room_id"] = id.Format(room.ID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_CREATE", payload)
	s.audit(ctx, spaceID, actorID, AuditRoomCreate, id.Format(room.ID), map[string]change{
		"name": {New: room.Name},
		"type": {New: room.Type},
	}, "")
	return room, nil
}

// RoomMap is the wire form of a space room, shared by REST, READY and SPACE_ROOM_CREATE.
func RoomMap(r *rooms.Room) map[string]interface{} {
	m := map[string]interface{}{
		"id":               id.Format(r.ID),
		"type":             r.Type,
		"name":             r.Name,
		"topic":            r.Topic,
		"slowmode_seconds": r.SlowmodeSeconds,
		"position":         r.Position,
		"e2ee_enabled":     r.E2EEEnabled != nil && *r.E2EEEnabled,
		"created_at":       r.CreatedAt,
		"updated_at":       r.UpdatedAt,
	}
	if r.Type == rooms.TypeSpaceVoice {
		m["user_limit"] = r.UserLimit
		m["bitrate"] = r.Bitrate
	}
	if r.SpaceID != nil {
		m["space_id"] = id.Format(*r.SpaceID)
	}
	if r.ParentID != nil {
		m["parent_id"] = id.Format(*r.ParentID)
	} else {
		m["parent_id"] = nil
	}
	if r.LastMessageID != nil {
		m["last_message_id"] = id.Format(*r.LastMessageID)
	}
	return m
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
	list, err := s.listRooms(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, room := range list {
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
	list, err := s.listRooms(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0)
	for _, room := range list {
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
