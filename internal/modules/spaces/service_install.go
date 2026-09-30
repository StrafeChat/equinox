package spaces

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// Bot installs and OAuth2-driven joins: what the oauth module calls when a user approves
// an application's `bot` scope (add its bot to a space they manage, with a role carrying
// the permissions they granted) or when an application with `spaces.join` adds a user to
// a space its bot is in. Both end in the same member-add path an invite join uses, so the
// join notice, gateway events and unread bookkeeping are identical.

var ErrInvalidBot = errors.New("that account is not a bot")

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
// someone with Manage Space.
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
		}
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
	sp, grantable, err := s.canInstallBots(ctx, actorID, spaceID)
	if err != nil {
		return 0, err
	}
	bot, err := s.userRepo.GetByID(ctx, botUserID)
	if err != nil {
		return 0, err
	}
	if bot == nil || !bot.Bot {
		return 0, ErrInvalidBot
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
		// The bot's role went in at the bottom and every other custom role moved up one:
		// one SPACE_UPDATE with the full role list keeps every client's copy exact.
		s.invalidateSnapshot(ctx, spaceID)
		if roles, err := s.SpaceRoles(ctx, spaceID); err == nil {
			payload := spaceToEventPayload(sp)
			payload["everyone_role_id"] = id.Format(eid)
			payload["roles"] = RoleMaps(roles)
			s.publishSpaceEvent(ctx, spaceID, "SPACE_UPDATE", payload)
		}
	}
	s.audit(ctx, spaceID, actorID, AuditBotAdd, id.Format(botUserID), map[string]change{
		"permissions": {New: granted},
	}, "")
	return granted, nil
}

// createBotRole inserts the bot's role just above @everyone (position 1), shifting every
// other custom role up by one - Discord's placement, and what keeps the role below the
// person who installed the bot so they can still edit or delete it.
func (s *Service) createBotRole(ctx context.Context, sp *Space, everyoneID int64, bot *auth.User, perms int64) (*SpaceRole, error) {
	existing, err := s.repo.ListSpaceRoles(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range existing {
		r := &existing[i]
		if r.ID == everyoneID {
			continue
		}
		r.Position++
		r.UpdatedAt = now
		if err := s.repo.UpdateSpaceRole(ctx, r); err != nil {
			return nil, err
		}
	}
	name := bot.DisplayName
	if name == "" {
		name = bot.Username
	}
	role := &SpaceRole{
		SpaceID:     sp.ID,
		ID:          id.Next(),
		Name:        name,
		Permissions: perms,
		Position:    1,
		BotID:       bot.ID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.repo.InsertSpaceRole(ctx, role); err != nil {
		return nil, err
	}
	return role, nil
}

// updateBotRole re-applies granted permissions to an installed bot's managed role (or
// creates the role when the bot was first added with none).
func (s *Service) updateBotRole(ctx context.Context, sp *Space, bot *auth.User, granted int64) (int64, error) {
	roles, err := s.repo.ListSpaceRoles(ctx, sp.ID)
	if err != nil {
		return 0, err
	}
	for i := range roles {
		r := &roles[i]
		if r.BotID != bot.ID {
			continue
		}
		r.Permissions = granted
		r.UpdatedAt = time.Now().UTC()
		if err := s.repo.UpdateSpaceRole(ctx, r); err != nil {
			return 0, err
		}
		s.invalidateSnapshot(ctx, sp.ID)
		s.publishSpaceEvent(ctx, sp.ID, "SPACE_ROLE_UPDATE", spaceRoleEventData(r))
		return granted, nil
	}
	eid, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return 0, err
	}
	role, err := s.createBotRole(ctx, sp, eid, bot, granted)
	if err != nil {
		return 0, err
	}
	mem, err := s.repo.GetMember(ctx, sp.ID, bot.ID)
	if err != nil || mem == nil {
		return 0, ErrNotMember
	}
	if err := s.repo.SetMemberRoleIDs(ctx, sp.ID, bot.ID, append(mem.RoleIDs, role.ID)); err != nil {
		return 0, err
	}
	s.invalidateSnapshot(ctx, sp.ID)
	if all, err := s.SpaceRoles(ctx, sp.ID); err == nil {
		payload := spaceToEventPayload(sp)
		payload["everyone_role_id"] = id.Format(eid)
		payload["roles"] = RoleMaps(all)
		s.publishSpaceEvent(ctx, sp.ID, "SPACE_UPDATE", payload)
	}
	s.publishSpaceEvent(ctx, sp.ID, "SPACE_MEMBER_UPDATE", map[string]interface{}{
		"user_id":  id.Format(bot.ID),
		"role_ids": formatRoleIDStrings(append(mem.RoleIDs, role.ID)),
	})
	return granted, nil
}

// afterMemberRemoved is what every removal (kick, ban, leave) does once the member row is
// gone: a bot's managed roles go with it, then everyone is told.
func (s *Service) afterMemberRemoved(ctx context.Context, spaceID, userID int64) {
	s.cleanupBotRoles(ctx, spaceID, userID)
	s.publishMemberRemoved(ctx, spaceID, userID)
}

// cleanupBotRoles deletes the roles a bot install created for userID (none for a person)
// and closes the gap they leave in the position order, undoing the shift-up their
// creation did, so repeated installs don't march every other role's position upward.
func (s *Service) cleanupBotRoles(ctx context.Context, spaceID, userID int64) {
	roles, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return
	}
	var gone []int
	for i := range roles {
		if roles[i].BotID != userID {
			continue
		}
		if err := s.repo.DeleteSpaceRole(ctx, spaceID, roles[i].ID); err != nil {
			continue
		}
		gone = append(gone, roles[i].Position)
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_DELETE", map[string]interface{}{
			"role_id": id.Format(roles[i].ID),
		})
	}
	if len(gone) == 0 {
		return
	}
	now := time.Now().UTC()
	for i := range roles {
		r := &roles[i]
		if r.BotID == userID || r.Position == 0 {
			continue
		}
		shift := 0
		for _, p := range gone {
			if r.Position > p {
				shift++
			}
		}
		if shift == 0 {
			continue
		}
		r.Position -= shift
		r.UpdatedAt = now
		_ = s.repo.UpdateSpaceRole(ctx, r)
	}
	s.invalidateSnapshot(ctx, spaceID)
	if sp, err := s.repo.GetByID(ctx, spaceID); err == nil && sp != nil {
		if all, err := s.SpaceRoles(ctx, spaceID); err == nil {
			payload := spaceToEventPayload(sp)
			if sp.EveryoneRoleID != 0 {
				payload["everyone_role_id"] = id.Format(sp.EveryoneRoleID)
			}
			payload["roles"] = RoleMaps(all)
			s.publishSpaceEvent(ctx, spaceID, "SPACE_UPDATE", payload)
		}
	}
}

// AddMemberViaOAuth is the `spaces.join` scope: the actor (a bot, typically) with Create
// Invite adds userID - who authorised it - to the space. Returns false when the user was
// already a member.
func (s *Service) AddMemberViaOAuth(ctx context.Context, actorID, spaceID, userID int64) (bool, error) {
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
	if err := s.repo.AddMember(ctx, sp.ID, userID, roleIDs); err != nil {
		return err
	}
	if err := s.repo.AddSpaceToUserSet(ctx, userID, sp.ID); err != nil {
		return err
	}
	s.markPreJoinHistoryRead(ctx, sp.ID, userID)
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
	payload := map[string]interface{}{
		"space_id":      id.Format(sp.ID),
		"id":            id.Format(u.ID),
		"username":      u.Username,
		"discriminator": u.Discriminator,
		"display_name":  u.DisplayName,
		"avatar":        u.Avatar,
		"banner":        u.Banner,
		"bio":           u.Bio,
		"about_me":      u.AboutMe,
		"bot":           u.Bot,
		"public_flags":  auth.PublicFlags(u),
		"presence":      auth.ToPublicPresence(u.Presence, true),
	}
	if mem, _ := s.repo.GetMember(ctx, sp.ID, userID); mem != nil {
		payload["role_ids"] = formatRoleIDStrings(mem.RoleIDs)
	}
	stargate.PublishToSpace(ctx, s.redis, sp.ID, "SPACE_MEMBER_ADD", payload, s.stargateRegion())

	space := spaceToEventPayload(sp)
	if roles, err := s.SpaceRoles(ctx, sp.ID); err == nil {
		space["roles"] = RoleMaps(roles)
	}
	if sp.EveryoneRoleID != 0 {
		space["everyone_role_id"] = id.Format(sp.EveryoneRoleID)
	}
	stargate.PublishToUser(ctx, s.redis, userID, "SPACE_CREATE", space, s.stargateRegion())
}
