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
	ErrRoleNotFound       = errors.New("role not found")
	ErrCannotEditEveryone = errors.New("cannot rename or delete the @everyone role")
	ErrMissingPerm        = errors.New("missing permission")
	ErrInvalidRoom        = errors.New("room not in this space")
	ErrInvalidSlowmode    = errors.New("invalid slowmode")
	ErrInvalidRoomType    = errors.New("invalid room type")
	ErrInvalidReorder     = errors.New("invalid room order")
	ErrInvalidRoleName    = errors.New("role name must be 1-100 characters and not @everyone")
	ErrInvalidRoomName    = errors.New("room name must be at most 100 characters")
	ErrTooManyRoles       = errors.New("a space can have at most 250 roles")
	ErrTooManyRooms       = errors.New("a space can have at most 500 rooms")
	ErrInvalidTopic       = errors.New("topic must be at most 1024 characters")
	ErrInvalidUserLimit   = errors.New("user limit must be 0 (unlimited) to 99")
	ErrInvalidBitrate     = errors.New("bitrate must be 8000-384000 bits per second, or 0 for the default")
	// ErrEveryoneRole: every member holds @everyone, so it is never added or removed by hand.
	ErrEveryoneRole = errors.New("every member has the @everyone role; it cannot be added or removed")
	// ErrRoleHierarchy mirrors Discord: a non-owner can't create into, edit, move, delete, grant
	// or remove a role at or above their own highest role's position (whoever the member is -
	// granting depends on the role alone), and can't kick, ban or rename a member whose highest
	// role is at or above theirs. Otherwise a Manage Roles holder could create an Administrator
	// role and grant it to themselves, or demote staff above them.
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
	_, everyoneID, byID, actorHighest, err = s.roleContext(ctx, spaceID, actorID)
	return everyoneID, byID, actorHighest, err
}

// roleContext is memberRoleContext with the space it read.
func (s *Service) roleContext(ctx context.Context, spaceID, actorID int64) (sp *Space, everyoneID int64, byID map[int64]*SpaceRole, actorHighest int, err error) {
	sp, err = s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, 0, nil, 0, ErrSpaceNotFound
	}
	everyoneID, err = s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return nil, 0, nil, 0, err
	}
	roles, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, 0, nil, 0, err
	}
	byID = indexRoles(roles)
	actorHighest, err = s.actorHighestPosition(ctx, spaceID, actorID, everyoneID, byID)
	if err != nil {
		return nil, 0, nil, 0, err
	}
	return sp, everyoneID, byID, actorHighest, nil
}

// actorHighestPosition is the position of the actor's highest role among byID, excluding
// @everyone: -1 when they hold no other role or are not a member.
func (s *Service) actorHighestPosition(ctx context.Context, spaceID, actorID, everyoneID int64, byID map[int64]*SpaceRole) (int, error) {
	roleIDs, ok, err := s.repo.MemberRoleIDs(ctx, spaceID, actorID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return -1, nil
	}
	return highestRolePosition(roleIDs, everyoneID, byID), nil
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
//
// The role goes in at position 1, just above @everyone (Discord's default), and the run of
// roles directly above it moves up one. That puts it below every custom role there is - so a
// bot or moderator can hand out, edit and delete the role they just made - and it is why a
// non-owner must hold a custom role to create one: without one, the new role would sit above
// them. The response carries the role's final position.
func (s *Service) CreateSpaceRole(ctx context.Context, actorID, spaceID int64, in *CreateSpaceRoleInput) (*SpaceRole, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteCreateRole(ctx, origin, spaceID, actor, in)
	}
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
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return nil, err
	}
	unlock := lockRoles(spaceID)
	defer unlock()
	existing, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if len(existing) >= MaxRolesPerSpace {
		return nil, ErrTooManyRoles
	}
	if sp.OwnerID != actorID {
		// Position 1 is below every role at 1 or higher (placeRoles puts the new role first
		// among equals), so this is exactly "the new role ends up below the actor".
		highest, err := s.actorHighestPosition(ctx, spaceID, actorID, everyoneID, indexRoles(existing))
		if err != nil {
			return nil, err
		}
		if highest < 1 {
			return nil, ErrRoleHierarchy
		}
	}
	now := time.Now().UTC()
	role := SpaceRole{
		SpaceID:     spaceID,
		ID:          id.Next(),
		Name:        name,
		Permissions: in.Permissions,
		Position:    1,
		Color:       in.Color,
		Hoist:       in.Hoist,
		Mentionable: in.Mentionable,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	roles := append(existing, role)
	all, err := s.applyRoleOrder(ctx, sp, everyoneID, roles, placeRoles(roles, everyoneID, map[int64]int{role.ID: 1}), role.ID, true, true)
	if err != nil {
		return nil, err
	}
	created := findRole(all, role.ID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_CREATE", spaceRoleEventData(created))
	s.audit(ctx, spaceID, actorID, AuditRoleCreate, id.Format(created.ID), map[string]change{
		"name":        {New: created.Name},
		"permissions": {New: created.Permissions},
	}, "")
	return created, nil
}

// UpdateSpaceRole updates a role. @everyone may only have permissions changed (not name).
// Non-owners cannot edit a role at or above their own highest role (Discord-style hierarchy),
// nor move one to or above it. A new position is a swap (swapRoles): the role takes the slot
// it asked for and the role holding it takes the moved role's old one, so no two roles ever
// share a position and clients that reorder with one such request per role, reusing the slots
// the roles hold, end where they meant. (PATCH /spaces/:id/roles, the batch, inserts instead.)
func (s *Service) UpdateSpaceRole(ctx context.Context, actorID, spaceID, roleID int64, in *UpdateSpaceRoleInput) (*SpaceRole, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteUpdateRole(ctx, origin, spaceID, actor, roleID, in)
	}
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
	// Held for the whole read-modify-write, not only for a move: the role's row is written
	// whole, and a position read before another write moved it would be written back over it.
	unlock := lockRoles(spaceID)
	defer unlock()
	roles, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	byID := indexRoles(roles)
	role := byID[roleID]
	if role == nil {
		return nil, ErrRoleNotFound
	}
	before := *role
	if roleID != everyoneID && sp.OwnerID != actorID {
		actorHighest, err := s.actorHighestPosition(ctx, spaceID, actorID, everyoneID, byID)
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
			return nil, ErrCannotMoveEveryone
		}
	}
	if in.Position != nil && !validRolePosition(*in.Position) {
		return nil, ErrInvalidRolePosition
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
	if in.Color != nil {
		role.Color = *in.Color
	}
	if in.Hoist != nil {
		role.Hoist = *in.Hoist
	}
	if in.Mentionable != nil {
		role.Mentionable = *in.Mentionable
	}
	var placed map[int64]int
	if in.Position != nil {
		placed = swapRoles(roles, everyoneID, roleID, *in.Position)
	}
	all, err := s.applyRoleOrder(ctx, sp, everyoneID, roles, placed, roleID, false, true)
	if err != nil {
		return nil, err
	}
	updated := findRole(all, roleID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_UPDATE", spaceRoleEventData(updated))
	changes := map[string]change{}
	diff(changes, "name", before.Name, updated.Name)
	diff(changes, "permissions", before.Permissions, updated.Permissions)
	diff(changes, "position", before.Position, updated.Position)
	diff(changes, "color", before.Color, updated.Color)
	diff(changes, "hoist", before.Hoist, updated.Hoist)
	diff(changes, "mentionable", before.Mentionable, updated.Mentionable)
	if len(changes) > 0 {
		s.audit(ctx, spaceID, actorID, AuditRoleUpdate, id.Format(roleID), changes, "")
	}
	return updated, nil
}

// DeleteSpaceRole removes a custom role.
func (s *Service) DeleteSpaceRole(ctx context.Context, actorID, spaceID, roleID int64) error {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteDeleteRole(ctx, origin, spaceID, actor, roleID)
	}
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
	// Under the role lock: a reorder that read this role before the delete would otherwise
	// write its position back afterwards, recreating part of the row.
	unlock := lockRoles(spaceID)
	defer unlock()
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
	s.fedRoleChanged(ctx, spaceID, role, true)
	s.audit(ctx, spaceID, actorID, AuditRoleDelete, id.Format(roleID), map[string]change{"name": {Old: role.Name}}, "")
	return nil
}

// SetMemberRoles replaces a member's roles. Caller must ManageRoles. @everyone is added if missing.
//
// Discord's rule: what may be given or taken away depends on the roles alone, never on the
// member. A role strictly below the actor's highest can be given to or taken from anyone - the
// owner, a member who outranks the actor, the actor themself (which is how a moderator, or the
// owner, gives themselves a colour or a hoisted title). A role at or above the actor's highest
// can be neither: roles the member already holds up there are carried over untouched, matching
// the locked checkboxes the client draws for them. The owner is above the hierarchy and may
// change any role. Kick, ban and nicknames act on the member and still check the member's rank.
func (s *Service) SetMemberRoles(ctx context.Context, actorID, spaceID, targetUserID int64, roleIDs []int64) error {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteSetMemberRoles(ctx, origin, spaceID, actor, targetUserID, roleIDs)
	}
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return ErrSpaceNotFound
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
	// The event is the member's full state (roles and space profile), read back after the
	// write so it reflects anything else that changed meanwhile.
	if fresh, err := s.repo.GetMember(ctx, spaceID, targetUserID); err == nil && fresh != nil {
		mem = fresh
	} else {
		mem.RoleIDs = out
	}
	s.publishMemberUpdate(ctx, spaceID, mem)
	s.fedMemberUpdated(ctx, spaceID, targetUserID)
	s.audit(ctx, spaceID, actorID, AuditMemberRolesUpdate, id.Format(targetUserID), map[string]change{
		"roles": {New: formatRoleIDStrings(out)},
	}, "")
	return nil
}

// AddMemberRole gives a member one role (PUT /spaces/:id/members/:userId/roles/:roleId,
// Discord's Add Guild Member Role); RemoveMemberRole takes one away (DELETE). The rules are
// SetMemberRoles': Manage Roles, a role strictly below the actor's highest (the owner: any),
// never @everyone, never a bot's managed role, whatever the member's own rank. A member who
// already holds the role (or, removing, does not) is success with nothing written or sent.
// The write is one set operation, so single-role changes racing each other all land.
func (s *Service) AddMemberRole(ctx context.Context, actorID, spaceID, targetID, roleID int64) error {
	return s.changeMemberRole(ctx, actorID, spaceID, targetID, roleID, true)
}

// RemoveMemberRole: see AddMemberRole.
func (s *Service) RemoveMemberRole(ctx context.Context, actorID, spaceID, targetID, roleID int64) error {
	return s.changeMemberRole(ctx, actorID, spaceID, targetID, roleID, false)
}

func (s *Service) changeMemberRole(ctx context.Context, actorID, spaceID, targetID, roleID int64, add bool) error {
	origin, actor, err := s.remoteSpace(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	// On a mirror too: the checks read the mirrored roles, so a change the origin would refuse
	// is refused the same way here even when nothing would be forwarded (the member already
	// has the role) - and removing a role above the actor is refused, not sent as a list the
	// origin would quietly carry that role over in.
	mem, everyoneID, byID, err := s.checkMemberRoleChange(ctx, actorID, spaceID, targetID, roleID)
	if err != nil {
		return err
	}
	if hasRoleID(mem.RoleIDs, roleID) == add {
		return nil
	}
	next := withRoleID(mem.RoleIDs, byID, everyoneID, roleID, add)
	if origin != "" {
		// The origin makes the one change as a set operation, like the write below, so two
		// single-role changes racing each other both land there too; the result comes back as
		// a relay. An origin from before that operation takes the whole list instead, built
		// from the row here (the origin's relayed state): a read-modify-write that a change
		// racing it can undo, but one every origin understands.
		err := s.fed.RemoteChangeMemberRole(ctx, origin, spaceID, actor, targetID, roleID, add)
		if errors.Is(err, ErrOriginLacksOperation) {
			return s.fed.RemoteSetMemberRoles(ctx, origin, spaceID, actor, targetID, next)
		}
		return err
	}
	if add {
		err = s.repo.AddMemberRoleID(ctx, spaceID, targetID, roleID)
	} else {
		err = s.repo.RemoveMemberRoleID(ctx, spaceID, targetID, roleID)
	}
	if err != nil {
		return err
	}
	// The event is the member's full state, read back after the write (see SetMemberRoles).
	fresh, rerr := s.repo.GetMember(ctx, spaceID, targetID)
	if rerr != nil {
		fresh = mem
		fresh.RoleIDs = append([]int64{everyoneID}, next...)
	} else if fresh == nil {
		// Left or was removed meanwhile: there is no member to tell anyone about.
		return nil
	}
	s.publishMemberUpdate(ctx, spaceID, fresh)
	s.fedMemberUpdated(ctx, spaceID, targetID)
	s.audit(ctx, spaceID, actorID, AuditMemberRolesUpdate, id.Format(targetID), map[string]change{
		"roles": {New: formatRoleIDStrings(fresh.RoleIDs)},
	}, "")
	return nil
}

// checkMemberRoleChange applies the single-role rules (see AddMemberRole) and returns the
// target's member row, the space's @everyone id and its roles by id.
func (s *Service) checkMemberRoleChange(ctx context.Context, actorID, spaceID, targetID, roleID int64) (*SpaceMember, int64, map[int64]*SpaceRole, error) {
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return nil, 0, nil, err
	}
	sp, everyoneID, byID, actorHighest, err := s.roleContext(ctx, spaceID, actorID)
	if err != nil {
		return nil, 0, nil, err
	}
	if roleID == everyoneID {
		return nil, 0, nil, ErrEveryoneRole
	}
	role := byID[roleID]
	if role == nil {
		return nil, 0, nil, ErrRoleNotFound
	}
	if role.IsManaged() {
		return nil, 0, nil, ErrManagedRole
	}
	if sp.OwnerID != actorID && role.Position >= actorHighest {
		return nil, 0, nil, ErrRoleHierarchy
	}
	mem, err := s.repo.GetMember(ctx, spaceID, targetID)
	if err != nil {
		return nil, 0, nil, err
	}
	if mem == nil {
		return nil, 0, nil, ErrNotMember
	}
	return mem, everyoneID, byID, nil
}

func hasRoleID(roleIDs []int64, roleID int64) bool {
	for _, rid := range roleIDs {
		if rid == roleID {
			return true
		}
	}
	return false
}

// withRoleID is a member's role list with roleID added or removed - the list SetMemberRoles
// takes: without @everyone, which every write adds back, and without the ids of roles deleted
// since (a member row keeps them), which SetMemberRoles would refuse as unknown.
func withRoleID(roleIDs []int64, byID map[int64]*SpaceRole, everyoneID, roleID int64, add bool) []int64 {
	out := make([]int64, 0, len(roleIDs)+1)
	for _, rid := range roleIDs {
		if rid != everyoneID && rid != roleID && byID[rid] != nil {
			out = append(out, rid)
		}
	}
	if add {
		out = append(out, roleID)
	}
	return out
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

// Caps per space. Every role and room rides in READY, in the permission snapshot and in
// gateway fan-out; the federation receiver stops syncing above these, so the origin must
// never get there.
const (
	MaxRolesPerSpace = 250
	MaxRoomsPerSpace = 500
)

// ListRoomRoleOverrides returns overrides for a channel. Any member who can view it.
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
	if err := s.assertRoomView(ctx, actorID, spaceID, roomID); err != nil {
		return nil, err
	}
	return s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
}

// assertRoomView: a room's settings are read and changed only by members who can see it
// (the owner and Administrators always can). Manage Rooms is a space-wide bit, so without
// this a holder could list, rename, re-topic or delete a private room hidden from them.
func (s *Service) assertRoomView(ctx context.Context, actorID, spaceID, roomID int64) error {
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if permissions.Has(base, permissions.PermAdministrator) {
		return nil
	}
	effective, err := s.EffectiveChannelPermissions(ctx, actorID, spaceID, roomID)
	if err != nil {
		return err
	}
	if !permissions.Has(effective, permissions.PermViewRoom) {
		return ErrMissingPerm
	}
	return nil
}

// ListRoomUserOverrides returns user-specific overrides for a channel. Any member who can view it.
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
	if err := s.assertRoomView(ctx, actorID, spaceID, roomID); err != nil {
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

// assertCanClearOverrides is the gate for an operation that removes every override a room
// has at once (syncing it to its section): the actor must be able to view the room, and each
// override being removed must pass the grant-subset rule, exactly as deleting it one by one
// would. Owners and Administrators pass as they do everywhere.
func (s *Service) assertCanClearOverrides(ctx context.Context, actorID, spaceID, roomID int64) error {
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if permissions.Has(base, permissions.PermAdministrator) {
		return nil
	}
	effective, err := s.EffectiveChannelPermissions(ctx, actorID, spaceID, roomID)
	if err != nil {
		return err
	}
	if !permissions.Has(effective, permissions.PermViewRoom) {
		return ErrMissingPerm
	}
	roleOv, err := s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
	if err != nil {
		return err
	}
	for i := range roleOv {
		if (roleOv[i].Allow|roleOv[i].Deny)&permissions.AllRoom&^effective != 0 {
			return ErrPermissionEscalation
		}
	}
	userOv, err := s.repo.ListRoomUserOverrides(ctx, spaceID, roomID)
	if err != nil {
		return err
	}
	for i := range userOv {
		if (userOv[i].Allow|userOv[i].Deny)&permissions.AllRoom&^effective != 0 {
			return ErrPermissionEscalation
		}
	}
	return nil
}

// PutRoomRoleOverride sets allow/deny for a role in a room. Requires ManageRoles: as on
// Discord, room permission overrides are part of role management, not room management.
func (s *Service) PutRoomRoleOverride(ctx context.Context, actorID, spaceID, roomID, roleID int64, in *PutRoomRoleOverrideInput) error {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteOverride(ctx, origin, spaceID, actor, roomID, roleID, 0, in.Allow, in.Deny, false)
	}
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
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
	s.republishRoomVisibility(ctx, spaceID, s.visibilityAffectedRooms(ctx, spaceID, roomID))
	s.fedRoomChanged(ctx, spaceID, roomID, false)
	s.audit(ctx, spaceID, actorID, AuditOverrideUpdate, id.Format(roomID)+":role:"+id.Format(roleID), map[string]change{
		"allow": {New: o.Allow},
		"deny":  {New: o.Deny},
	}, "")
	return nil
}

// PutRoomUserOverride sets allow/deny for a user in a room. Requires ManageRoles (see
// PutRoomRoleOverride).
func (s *Service) PutRoomUserOverride(ctx context.Context, actorID, spaceID, roomID, targetUserID int64, in *PutRoomUserOverrideInput) error {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteOverride(ctx, origin, spaceID, actor, roomID, 0, targetUserID, in.Allow, in.Deny, false)
	}
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
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
	s.republishRoomVisibility(ctx, spaceID, s.visibilityAffectedRooms(ctx, spaceID, roomID))
	s.fedRoomChanged(ctx, spaceID, roomID, false)
	s.audit(ctx, spaceID, actorID, AuditOverrideUpdate, id.Format(roomID)+":user:"+id.Format(targetUserID), map[string]change{
		"allow": {New: o.Allow},
		"deny":  {New: o.Deny},
	}, "")
	return nil
}

// DeleteRoomRoleOverride removes an override row. Requires ManageRoles, and - like
// writing one - only for bits the actor holds in that room: removing a deny grants exactly
// what writing the matching allow would, so the grant-subset rule applies to both.
func (s *Service) DeleteRoomRoleOverride(ctx context.Context, actorID, spaceID, roomID, roleID int64) error {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteOverride(ctx, origin, spaceID, actor, roomID, roleID, 0, 0, 0, true)
	}
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	existing, err := s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
	if err != nil {
		return err
	}
	var cur *SpaceRoomRoleOverride
	for i := range existing {
		if existing[i].RoleID == roleID {
			cur = &existing[i]
			break
		}
	}
	if cur == nil {
		return nil
	}
	if _, _, err := s.checkOverrideGrant(ctx, actorID, spaceID, roomID, cur.Allow, cur.Deny); err != nil {
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
	s.republishRoomVisibility(ctx, spaceID, s.visibilityAffectedRooms(ctx, spaceID, roomID))
	s.fedRoomChanged(ctx, spaceID, roomID, false)
	s.audit(ctx, spaceID, actorID, AuditOverrideDelete, id.Format(roomID)+":role:"+id.Format(roleID), map[string]change{
		"allow": {Old: cur.Allow},
		"deny":  {Old: cur.Deny},
	}, "")
	return nil
}

// DeleteRoomUserOverride removes a user override row. Requires ManageRoles and, as for
// DeleteRoomRoleOverride, only for bits the actor holds in that room.
func (s *Service) DeleteRoomUserOverride(ctx context.Context, actorID, spaceID, roomID, targetUserID int64) error {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteOverride(ctx, origin, spaceID, actor, roomID, 0, targetUserID, 0, 0, true)
	}
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	existing, err := s.repo.ListRoomUserOverrides(ctx, spaceID, roomID)
	if err != nil {
		return err
	}
	var cur *SpaceRoomUserOverride
	for i := range existing {
		if existing[i].UserID == targetUserID {
			cur = &existing[i]
			break
		}
	}
	if cur == nil {
		return nil
	}
	if _, _, err := s.checkOverrideGrant(ctx, actorID, spaceID, roomID, cur.Allow, cur.Deny); err != nil {
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
	s.republishRoomVisibility(ctx, spaceID, s.visibilityAffectedRooms(ctx, spaceID, roomID))
	s.fedRoomChanged(ctx, spaceID, roomID, false)
	s.audit(ctx, spaceID, actorID, AuditOverrideDelete, id.Format(roomID)+":user:"+id.Format(targetUserID), map[string]change{
		"allow": {Old: cur.Allow},
		"deny":  {Old: cur.Deny},
	}, "")
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
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteUpdateRoom(ctx, origin, spaceID, actor, roomID, in)
	}
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if in == nil || (in.Name == nil && in.Topic == nil && in.SlowmodeSeconds == nil && in.E2EEEnabled == nil &&
		in.UserLimit == nil && in.Bitrate == nil && in.PermissionsSynced == nil) {
		return ErrNothingToPatch
	}
	room, err := s.roomRepo.GetByID(ctx, roomID)
	if err != nil || room == nil || room.SpaceID == nil || *room.SpaceID != spaceID {
		return ErrInvalidRoom
	}
	if err := s.assertRoomView(ctx, actorID, spaceID, roomID); err != nil {
		return err
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

	// Category permission sync: follow the parent section's overrides, or stop following them.
	// Syncing deletes every override the room has, so it is override management: Manage
	// Roles (not Manage Rooms), View in that room, and nothing removed that the actor could
	// not have written - the same three rules PutRoomRoleOverride / DeleteRoomRoleOverride
	// apply. Without them Manage Rooms alone could open a private room it cannot see.
	if in.PermissionsSynced != nil {
		if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
			return err
		}
		if err := s.assertCanClearOverrides(ctx, actorID, spaceID, roomID); err != nil {
			return err
		}
		var parent int64
		if room.ParentID != nil {
			parent = *room.ParentID
		}
		rs := &roomForSync{ID: room.ID, Type: room.Type, ParentID: parent, Synced: room.PermissionsSynced != nil && *room.PermissionsSynced}
		if _, err := s.applyPermissionsSync(ctx, spaceID, rs, *in.PermissionsSynced); err != nil {
			return err
		}
	}

	updated, _ := s.roomRepo.GetByID(ctx, roomID)
	if updated != nil {
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_UPDATE", map[string]interface{}{
			"room_id":            id.Format(updated.ID),
			"name":               updated.Name,
			"topic":              updated.Topic,
			"slowmode_seconds":   updated.SlowmodeSeconds,
			"e2ee_enabled":       updated.E2EEOn(),
			"user_limit":         updated.UserLimit,
			"bitrate":            updated.Bitrate,
			"permissions_synced": updated.PermissionsSynced != nil && *updated.PermissionsSynced,
			"updated_at":         updated.UpdatedAt,
		})
		changes := map[string]change{}
		diff(changes, "name", room.Name, updated.Name)
		diff(changes, "topic", room.Topic, updated.Topic)
		diff(changes, "slowmode_seconds", room.SlowmodeSeconds, updated.SlowmodeSeconds)
		diff(changes, "e2ee_enabled", room.E2EEEnabled != nil && *room.E2EEEnabled, updated.E2EEEnabled != nil && *updated.E2EEEnabled)
		diff(changes, "user_limit", room.UserLimit, updated.UserLimit)
		diff(changes, "bitrate", room.Bitrate, updated.Bitrate)
		diff(changes, "permissions_synced", room.PermissionsSynced != nil && *room.PermissionsSynced, updated.PermissionsSynced != nil && *updated.PermissionsSynced)
		if len(changes) > 0 {
			s.audit(ctx, spaceID, actorID, AuditRoomUpdate, id.Format(roomID), changes, "")
		}
		s.fedRoomChanged(ctx, spaceID, roomID, false)
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
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteDeleteRoom(ctx, origin, spaceID, actor, roomID)
	}
	if err := s.canManageRooms(ctx, actorID, spaceID); err != nil {
		return err
	}
	if err := s.assertRoomInSpace(ctx, spaceID, roomID); err != nil {
		return err
	}
	if err := s.assertRoomView(ctx, actorID, spaceID, roomID); err != nil {
		return err
	}
	// Deleting a section orphans its channels - they move to the top level and keep the perms
	// they had (synced ones get the section's overrides copied down first), the way Discord
	// leaves a category's channels in place when the category is removed.
	if target, _ := s.roomRepo.GetByID(ctx, roomID); target != nil && target.Type == rooms.TypeRoomSection {
		all, err := s.listRooms(ctx, spaceID)
		if err != nil {
			return err
		}
		for _, child := range all {
			if child.ParentID == nil || *child.ParentID != roomID {
				continue
			}
			if child.PermissionsSynced != nil && *child.PermissionsSynced {
				if err := s.copyOverridesFromParent(ctx, spaceID, child.ID, roomID); err != nil {
					return err
				}
				if err := s.roomRepo.UpdateRoomPermissionsSynced(ctx, child.ID, false); err != nil {
					return err
				}
			}
			if err := s.roomRepo.UpdateSpaceRoomParentAndPosition(ctx, spaceID, child.ID, nil, child.Position); err != nil {
				return err
			}
			s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_UPDATE", map[string]interface{}{
				"room_id":            id.Format(child.ID),
				"space_id":           id.Format(spaceID),
				"parent_id":          nil,
				"position":           child.Position,
				"permissions_synced": false,
			})
		}
	}
	// A channel's threads go with it: they live inside it and nothing else can hold them.
	if target, _ := s.roomRepo.GetByID(ctx, roomID); target != nil && target.Type == rooms.TypeSpaceText {
		all, err := s.listRooms(ctx, spaceID)
		if err != nil {
			return err
		}
		for _, child := range all {
			if child.Type != rooms.TypeThread || child.ParentID == nil || *child.ParentID != roomID {
				continue
			}
			if err := s.roomRepo.DeleteThread(ctx, spaceID, child.ID); err != nil {
				return err
			}
			s.publishSpaceEvent(ctx, spaceID, "THREAD_DELETE", map[string]interface{}{
				"room_id": id.Format(child.ID), "parent_id": id.Format(roomID),
			})
		}
	}
	if err := s.roomRepo.DeleteSpaceRoom(ctx, spaceID, roomID); err != nil {
		return err
	}
	s.invalidateSnapshot(ctx, spaceID)
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_DELETE", map[string]interface{}{
		"room_id": id.Format(roomID),
	})
	s.fedRoomChanged(ctx, spaceID, roomID, true)
	s.audit(ctx, spaceID, actorID, AuditRoomDelete, id.Format(roomID), nil, "")
	return nil
}

// CreateRoom creates a new room or section in the space. Requires ManageRooms.
func (s *Service) CreateRoom(ctx context.Context, actorID, spaceID int64, in *CreateRoomInput) (*rooms.Room, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteCreateRoom(ctx, origin, spaceID, actor, in)
	}
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
	if len(rows) >= MaxRoomsPerSpace {
		return nil, ErrTooManyRooms
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
	// A channel created inside a section follows the section's permissions by default, the way
	// a new Discord channel inherits its category. Sections themselves are never synced.
	if in.ParentID != nil && (in.Type == rooms.TypeSpaceText || in.Type == rooms.TypeSpaceVoice) {
		synced := true
		room.PermissionsSynced = &synced
	}
	if err := s.roomRepo.CreateSpaceRoom(ctx, room); err != nil {
		return nil, err
	}
	s.invalidateSnapshot(ctx, spaceID)
	// Same shape as GET /spaces/:id/rooms (plus room_id), so clients can add the room to
	// their list straight from the event. A new room has no overrides yet.
	payload := AttachOverrides(RoomMap(room), RoomOverrides{})
	payload["room_id"] = id.Format(room.ID)
	// A room created private - directly denied to @everyone, or synced into a private section -
	// must not be announced space-wide, or its name/metadata would leak to members who can't see
	// it. Announce it space-wide only when @everyone can view it; otherwise deliver it per-member
	// (viewers get SPACE_ROOM_CREATE, everyone else nothing).
	if s.everyoneCanView(ctx, spaceID, room.ID) {
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_CREATE", payload)
	} else {
		s.republishRoomVisibility(ctx, spaceID, []int64{room.ID})
	}
	s.fedRoomChanged(ctx, spaceID, room.ID, false)
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
		"e2ee_enabled":     r.E2EEOn(),
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
	m["permissions_synced"] = r.PermissionsSynced != nil && *r.PermissionsSynced
	if r.Type == rooms.TypeThread {
		m["thread"] = ThreadMap(r)
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
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteReorderRooms(ctx, origin, spaceID, actor, in)
	}
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
	s.fedRoomsReordered(ctx, spaceID)
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
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteMoveChannel(ctx, origin, spaceID, actor, channelRoomID, newParentSectionID, beforeRoomID)
	}
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

	// Moving to a different section (or out to the top level) ends the channel's sync to its
	// old category - it keeps the perms it had, now as its own, matching Discord.
	if ch.PermissionsSynced != nil && *ch.PermissionsSynced {
		if oldParent != nil {
			if err := s.copyOverridesFromParent(ctx, spaceID, channelRoomID, *oldParent); err != nil {
				return err
			}
		}
		if err := s.roomRepo.UpdateRoomPermissionsSynced(ctx, channelRoomID, false); err != nil {
			return err
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
			"room_id":            id.Format(rid),
			"position":           room.Position,
			"space_id":           id.Format(spaceID),
			"permissions_synced": room.PermissionsSynced != nil && *room.PermissionsSynced,
			"updated_at":         room.UpdatedAt,
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
	s.fedRoomsReordered(ctx, spaceID)
	return nil
}

// ThreadMap is the `thread` block of a thread room's wire form - Discord's thread_metadata
// plus the owner: archive and lock state, type, auto-archive timer, who started it and from
// which message. The counts and the viewer's membership are added by AttachThreadState.
func ThreadMap(r *rooms.Room) map[string]interface{} {
	m := map[string]interface{}{
		"archived":             r.ThreadIsArchived(),
		"locked":               r.ThreadIsLocked(),
		"private":              r.ThreadIsPrivate(),
		"invitable":            r.ThreadIsInvitable(),
		"auto_archive_minutes": r.ThreadAutoArchiveMin,
		"owner_id":             id.Format(r.CreatorID),
		"archived_at":          nil,
	}
	if r.ThreadArchivedAt != nil {
		m["archived_at"] = r.ThreadArchivedAt.UTC()
	}
	if r.ThreadLastActiveAt != nil {
		m["last_active_at"] = r.ThreadLastActiveAt.UTC()
	}
	if r.ThreadStarterMessageID != nil {
		m["starter_message_id"] = id.Format(*r.ThreadStarterMessageID)
	}
	return m
}

// AttachThreadState adds message_count, member_count and the viewer's `joined` to a thread's
// wire form (a no-op for a room that is not a thread).
func AttachThreadState(m map[string]interface{}, st ThreadState) {
	t, _ := m["thread"].(map[string]interface{})
	if t == nil {
		return
	}
	t["message_count"] = st.MessageCount
	t["member_count"] = st.MemberCount
	t["joined"] = st.Joined
}
