package spaces

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// Bot installs and OAuth2-driven joins: what the oauth module calls when a user approves
// an application's `bot` scope (add its bot to a space they manage, with a role carrying
// the permissions they granted) or when an application with `spaces.join` adds a user to
// a space its bot is in. Both end in the same member-add path an invite join uses, so the
// join notice, gateway events and unread bookkeeping are identical.

var (
	ErrInvalidBot   = errors.New("that account is not a bot")
	ErrNotLocalUser = errors.New("that user does not belong to this instance")
)

// InstallTarget is a space a user may add a bot to, with the permission bits they may hand
// it (everything for the owner and Administrators, otherwise exactly what they hold).
type InstallTarget struct {
	Space     *Space
	Grantable int64
}

// SpaceSummary is the light per-space row of GET /users/@me/spaces: what an OAuth2 app
// with the `spaces` scope (or a bot listing where it lives) needs, and nothing more.
type SpaceSummary struct {
	Space       *Space
	Owner       bool
	Permissions int64
}

// canInstallBots is Discord's rule for adding a bot: the owner, an Administrator, or
// someone with Manage Space. On a mirror this reads the mirrored roles, which is what the
// consent screen's picker needs; the install itself is decided by the origin.
func (s *Service) canInstallBots(ctx context.Context, actorID, spaceID int64) (*Space, int64, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, 0, err
	}
	if sp == nil {
		return nil, 0, ErrSpaceNotFound
	}
	if sp.OwnerID == actorID {
		return sp, permissions.AllSpace, nil
	}
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return nil, 0, err
	}
	if permissions.Has(base, permissions.PermAdministrator) {
		return sp, permissions.AllSpace, nil
	}
	if !permissions.Has(base, permissions.PermManageSpace) {
		return nil, 0, ErrMissingPerm
	}
	return sp, base, nil
}

// BotInstallTargets lists the spaces userID may add a bot to, for the consent screen's
// space picker, each with the permissions that user could grant there.
func (s *Service) BotInstallTargets(ctx context.Context, userID int64) ([]InstallTarget, error) {
	summaries, err := s.ListSpaceSummaries(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]InstallTarget, 0, len(summaries))
	for _, sm := range summaries {
		switch {
		case sm.Owner, permissions.Has(sm.Permissions, permissions.PermAdministrator):
			out = append(out, InstallTarget{Space: sm.Space, Grantable: permissions.AllSpace})
		case permissions.Has(sm.Permissions, permissions.PermManageSpace):
			out = append(out, InstallTarget{Space: sm.Space, Grantable: sm.Permissions})
		default:
			continue
		}
		// A space hosted elsewhere is a target too (the bot is added through its origin);
		// the picker says where it lives.
		s.attachFederation(ctx, sm.Space)
	}
	return out, nil
}

// ListSpaceSummaries is every space the user is in with their space-wide permissions.
func (s *Service) ListSpaceSummaries(ctx context.Context, userID int64) ([]SpaceSummary, error) {
	rows, err := s.repo.ListSpacesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.SpaceID)
	}
	spaceObjs, err := s.repo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]SpaceSummary, 0, len(spaceObjs))
	for _, sp := range spaceObjs {
		if sp == nil {
			continue
		}
		sm := SpaceSummary{Space: sp, Owner: sp.OwnerID == userID}
		if sm.Owner {
			sm.Permissions = permissions.AllSpace
		} else if base, err := s.SpacePermissionBase(ctx, userID, sp.ID); err == nil {
			sm.Permissions = base
		}
		out = append(out, sm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Space.Name < out[j].Space.Name })
	return out, nil
}

// InstallBot adds botUserID to the space with a role carrying `permissions` (masked to what
// the actor may grant - never more than they hold themselves), and returns the bits that
// were actually granted. Already a member: nothing changes and 0 is returned, matching a
// re-authorisation of an installed bot.
func (s *Service) InstallBot(ctx context.Context, actorID, spaceID, botUserID, perms int64) (int64, error) {
	bot, err := s.userRepo.GetByID(ctx, botUserID)
	if err != nil {
		return 0, err
	}
	if bot == nil || !bot.Bot {
		return 0, ErrInvalidBot
	}
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return 0, err
	} else if origin != "" {
		// A space hosted elsewhere: its origin adds the bot as this member, with every
		// check a local install gets, and what it relays back builds the member and the
		// managed role here. Only a bot of this instance can be offered - the consent
		// screen is its home's, like the member's.
		if bot.IsRemote() {
			return 0, ErrInvalidBot
		}
		return s.fed.RemoteInstallBot(ctx, origin, spaceID, actor, bot, perms)
	}
	sp, grantable, err := s.canInstallBots(ctx, actorID, spaceID)
	if err != nil {
		return 0, err
	}
	granted := perms & grantable & permissions.AllSpace
	if ok, err := s.repo.IsMember(ctx, spaceID, botUserID); err != nil {
		return 0, err
	} else if ok {
		// Re-authorising an installed bot re-applies its permissions to its role, the way
		// Discord does; with nothing requested it is a no-op.
		if granted == 0 {
			return 0, nil
		}
		return s.updateBotRole(ctx, sp, bot, granted)
	}
	if ban, err := s.repo.GetBan(ctx, spaceID, botUserID); err != nil {
		return 0, err
	} else if ban != nil {
		return 0, ErrBanned
	}
	eid, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return 0, err
	}
	roleIDs := []int64{eid}
	var roleCreated bool
	if granted != 0 {
		role, err := s.createBotRole(ctx, sp, eid, bot, granted)
		if err != nil {
			return 0, err
		}
		roleIDs = append(roleIDs, role.ID)
		roleCreated = true
	}
	if err := s.addMember(ctx, sp, botUserID, roleIDs); err != nil {
		return 0, err
	}
	if roleCreated {
		// The bot's role went in at the bottom, pushing up the run of roles directly above
		// it: one SPACE_UPDATE with the full role list brings every client both.
		s.invalidateSnapshot(ctx, spaceID)
		if roles, err := s.SpaceRoles(ctx, spaceID); err == nil {
			s.publishRoleOrder(ctx, sp, eid, roles)
		}
	}
	s.audit(ctx, spaceID, actorID, AuditBotAdd, id.Format(botUserID), map[string]change{
		"permissions": {New: granted},
	}, "")
	return granted, nil
}

// createBotRole adds the bot's managed role at position 1, just above @everyone - Discord's
// placement, which keeps it below the person who installed the bot so they can still edit or
// delete it (and the owner drags it above the roles the bot should hand out). placeRoles
// pushes up only the run of roles directly above it. Nothing is announced here: the caller
// sends one SPACE_UPDATE once the bot holds the role.
func (s *Service) createBotRole(ctx context.Context, sp *Space, everyoneID int64, bot *auth.User, perms int64) (*SpaceRole, error) {
	unlock := lockRoles(sp.ID)
	defer unlock()
	existing, err := s.repo.ListSpaceRoles(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	return s.insertBotRole(ctx, sp, everyoneID, bot, perms, existing)
}

// insertBotRole is createBotRole for a caller already holding the role lock, with the space's
// roles as it read them.
func (s *Service) insertBotRole(ctx context.Context, sp *Space, everyoneID int64, bot *auth.User, perms int64, existing []SpaceRole) (*SpaceRole, error) {
	if len(existing) >= MaxRolesPerSpace {
		return nil, ErrTooManyRoles
	}
	name := bot.DisplayName
	if name == "" {
		name = bot.Username
	}
	now := time.Now().UTC()
	role := SpaceRole{
		SpaceID:     sp.ID,
		ID:          id.Next(),
		Name:        name,
		Permissions: perms,
		Position:    1,
		BotID:       bot.ID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	roles := append(existing, role)
	all, err := s.applyRoleOrder(ctx, sp, everyoneID, roles, placeRoles(roles, everyoneID, map[int64]int{role.ID: 1}), role.ID, true, false)
	if err != nil {
		return nil, err
	}
	return findRole(all, role.ID), nil
}

// updateBotRole re-applies granted permissions to an installed bot's managed role (or
// creates the role when the bot was first added with none).
func (s *Service) updateBotRole(ctx context.Context, sp *Space, bot *auth.User, granted int64) (int64, error) {
	eid, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return 0, err
	}
	mem, err := s.repo.GetMember(ctx, sp.ID, bot.ID)
	if err != nil || mem == nil {
		return 0, ErrNotMember
	}
	// The role lock covers the read and the write: the role's row is written whole.
	unlock := lockRoles(sp.ID)
	roles, err := s.repo.ListSpaceRoles(ctx, sp.ID)
	if err != nil {
		unlock()
		return 0, err
	}
	for i := range roles {
		r := &roles[i]
		if r.BotID != bot.ID {
			continue
		}
		r.Permissions = granted
		r.UpdatedAt = time.Now().UTC()
		err := s.repo.UpdateSpaceRole(ctx, r)
		unlock()
		if err != nil {
			return 0, err
		}
		s.invalidateSnapshot(ctx, sp.ID)
		s.publishSpaceEvent(ctx, sp.ID, "SPACE_ROLE_UPDATE", spaceRoleEventData(r))
		s.fedRoleChanged(ctx, sp.ID, r, false)
		return granted, nil
	}
	role, err := s.insertBotRole(ctx, sp, eid, bot, granted, roles)
	unlock()
	if err != nil {
		return 0, err
	}
	if err := s.repo.AddMemberRoleID(ctx, sp.ID, bot.ID, role.ID); err != nil {
		return 0, err
	}
	if fresh, err := s.repo.GetMember(ctx, sp.ID, bot.ID); err == nil && fresh != nil {
		mem = fresh
	} else {
		mem.RoleIDs = append(mem.RoleIDs, role.ID)
	}
	s.invalidateSnapshot(ctx, sp.ID)
	if all, err := s.SpaceRoles(ctx, sp.ID); err == nil {
		s.publishRoleOrder(ctx, sp, eid, all)
	}
	s.publishMemberUpdate(ctx, sp.ID, mem)
	s.fedMemberUpdated(ctx, sp.ID, bot.ID)
	return granted, nil
}

// afterMemberRemoved is what every removal (kick, ban, leave) does once the member row is
// gone: a bot's managed roles go with it, then everyone is told.
func (s *Service) afterMemberRemoved(ctx context.Context, spaceID, userID int64) {
	s.cleanupBotRoles(ctx, spaceID, userID)
	s.publishMemberRemoved(ctx, spaceID, userID)
	if s.memberRemoved != nil {
		s.memberRemoved(ctx, spaceID, userID)
	}
}

// cleanupBotRoles deletes the roles a bot install created for userID (none for a person)
// and closes the gap they leave in the position order, so the roles above move back down to
// where they were before the bot's role pushed them up and repeated installs don't march
// every other role's position upward. Best-effort: the member is already gone.
func (s *Service) cleanupBotRoles(ctx context.Context, spaceID, userID int64) {
	unlock := lockRoles(spaceID)
	defer unlock()
	roles, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return
	}
	var gone []int
	kept := make([]SpaceRole, 0, len(roles))
	for i := range roles {
		if roles[i].BotID != userID {
			kept = append(kept, roles[i])
			continue
		}
		if err := s.repo.DeleteSpaceRole(ctx, spaceID, roles[i].ID); err != nil {
			kept = append(kept, roles[i])
			continue
		}
		gone = append(gone, roles[i].Position)
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_DELETE", map[string]interface{}{
			"role_id": id.Format(roles[i].ID),
		})
		s.fedRoleChanged(ctx, spaceID, &roles[i], true)
	}
	if len(gone) == 0 {
		return
	}
	s.invalidateSnapshot(ctx, spaceID)
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil || sp.EveryoneRoleID == 0 {
		return
	}
	// Each role above a removed one moves down by how many were removed below it, never past
	// the role ranked under it (closeRoleGaps), and is written like every other position.
	placed := closeRoleGaps(kept, sp.EveryoneRoleID, gone)
	if _, err := s.applyRoleOrder(ctx, sp, sp.EveryoneRoleID, kept, placed, 0, false, true); err != nil {
		logger.Err("spaces", err, map[string]any{"space_id": spaceID, "step": "close the gap a bot's roles left"})
	}
}

// AddMemberViaOAuth is the `spaces.join` scope: the actor (a bot, typically) with Create
// Invite adds userID - who authorised it - to the space. Returns false when the user was
// already a member.
func (s *Service) AddMemberViaOAuth(ctx context.Context, actorID, spaceID, userID int64) (bool, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return false, err
	} else if origin != "" {
		// The origin adds the user (one of this instance's, who consented here) as the
		// actor, with the same permission check a local add gets.
		u, err := s.userRepo.GetByID(ctx, userID)
		if err != nil {
			return false, err
		}
		if u == nil || u.IsRemote() {
			return false, ErrNotLocalUser
		}
		return s.fed.RemoteAddMember(ctx, origin, spaceID, actor, u)
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return false, err
	}
	if sp == nil {
		return false, ErrSpaceNotFound
	}
	if sp.OwnerID != actorID {
		base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
		if err != nil {
			return false, err
		}
		if !permissions.Has(base, permissions.PermCreateInvite) && !permissions.Has(base, permissions.PermAdministrator) &&
			!permissions.Has(base, permissions.PermManageSpace) {
			return false, ErrMissingPerm
		}
	}
	if ok, err := s.repo.IsMember(ctx, spaceID, userID); err != nil {
		return false, err
	} else if ok {
		return false, nil
	}
	if ban, err := s.repo.GetBan(ctx, spaceID, userID); err != nil {
		return false, err
	} else if ban != nil {
		return false, ErrBanned
	}
	eid, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return false, err
	}
	if err := s.addMember(ctx, sp, userID, []int64{eid}); err != nil {
		return false, err
	}
	return true, nil
}

// addMember is the one place a member row is created: the membership itself, the user's
// space set, the "history before you joined is read" bookkeeping, the join notice in the
// system room, SPACE_MEMBER_ADD to everyone in the space, and SPACE_CREATE to the new
// member's own channel so their other sessions (or a bot's gateway connection) learn of
// the space without a refresh.
func (s *Service) addMember(ctx context.Context, sp *Space, userID int64, roleIDs []int64) error {
	if err := s.repo.AddMember(ctx, sp.ID, userID, roleIDs, MemberProfile{}); err != nil {
		return err
	}
	if err := s.repo.AddSpaceToUserSet(ctx, userID, sp.ID); err != nil {
		return err
	}
	s.markPreJoinHistoryRead(ctx, sp.ID, userID)
	// Tell the other instances first: a mirror must know the member before the join
	// notice (relayed as a message in the same per-peer order) names them.
	s.fedMemberAdded(ctx, sp.ID, userID)
	s.postSystemMessage(ctx, sp, SystemRoomFlagSuppressJoin, SystemMemberJoin, map[string]interface{}{
		"user_id": id.Format(userID),
	})
	s.publishMemberAdd(ctx, sp, userID)
	return nil
}

func (s *Service) publishMemberAdd(ctx context.Context, sp *Space, userID int64) {
	if s.redis == nil {
		return
	}
	u, _ := s.userRepo.GetByID(ctx, userID)
	if u == nil {
		return
	}
	payload := s.memberUserFields(u, stargate.VisibleActivitiesOf(ctx, u))
	payload["space_id"] = id.Format(sp.ID)
	if mem, _ := s.repo.GetMember(ctx, sp.ID, userID); mem != nil {
		payload["role_ids"] = formatRoleIDStrings(mem.RoleIDs)
		payload["joined_at"] = mem.JoinedAt
		// A mirror's copy of an existing member arrives with their space profile.
		for k, v := range memberProfileFields(mem) {
			payload[k] = v
		}
	}
	stargate.PublishToSpace(ctx, s.redis, sp.ID, "SPACE_MEMBER_ADD", payload, s.stargateRegion())

	s.attachFederation(ctx, sp)
	space := spaceToEventPayload(sp)
	if roles, err := s.SpaceRoles(ctx, sp.ID); err == nil {
		space["roles"] = RoleMaps(roles)
	}
	if sp.EveryoneRoleID != 0 {
		space["everyone_role_id"] = id.Format(sp.EveryoneRoleID)
	}
	stargate.PublishToUser(ctx, s.redis, userID, "SPACE_CREATE", space, s.stargateRegion())
}
