package spaces

import (
	"context"
	"errors"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

var ErrInvalidInvite = errors.New("invite expiry must be at most 30 days and uses at most 1000")

// canViewInvites: Manage space, Administrator, or owner - the same gate Discord puts on
// the server's invite list.
func (s *Service) canManageInvites(ctx context.Context, actorID, spaceID int64) (*Space, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, ErrSpaceNotFound
	}
	if sp.OwnerID == actorID {
		return sp, nil
	}
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return nil, err
	}
	if !permissions.Has(base, permissions.PermManageSpace) && !permissions.Has(base, permissions.PermAdministrator) {
		return nil, ErrMissingPerm
	}
	return sp, nil
}

// CreateInvite creates a new invite for the given space. Requires Create Invite (or
// Administrator) - granted to @everyone by default, matching Discord, so any member can
// invite unless it's been explicitly restricted. in is optional (nil = never expires,
// unlimited uses).
func (s *Service) CreateInvite(ctx context.Context, actorID, spaceID int64, in *CreateInviteInput) (*SpaceInvite, error) {
	if origin := s.mirrorOrigin(ctx, spaceID); origin != "" {
		// The origin mints it (and checks the member may); the code comes back as
		// code@origin so anyone can use it from any instance.
		return s.createRemoteInvite(ctx, actorID, spaceID, origin, in)
	}
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	space, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if space == nil {
		return nil, ErrSpaceNotFound
	}
	if space.OwnerID != actorID {
		base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
		if err != nil {
			return nil, err
		}
		if !permissions.Has(base, permissions.PermCreateInvite) && !permissions.Has(base, permissions.PermAdministrator) {
			return nil, ErrMissingPerm
		}
	}
	inv, err := s.mintInvite(ctx, spaceID, actorID, in)
	if err != nil {
		return nil, err
	}
	changes := map[string]change{}
	if inv.MaxUses > 0 {
		changes["max_uses"] = change{New: inv.MaxUses}
	}
	if inv.ExpiresAt != nil {
		changes["expires_at"] = change{New: inv.ExpiresAt.UTC().Format(time.RFC3339)}
	}
	s.audit(ctx, spaceID, actorID, AuditInviteCreate, inv.Code, changes, "")
	return inv, nil
}

// mintInvite writes a new invite row pair without any permission check (callers gate).
func (s *Service) mintInvite(ctx context.Context, spaceID, inviterID int64, in *CreateInviteInput) (*SpaceInvite, error) {
	maxAge, maxUses := 0, 0
	if in != nil {
		maxAge, maxUses = in.MaxAgeSeconds, in.MaxUses
	}
	if maxAge < 0 || maxAge > MaxInviteAgeSeconds || maxUses < 0 || maxUses > MaxInviteUses {
		return nil, ErrInvalidInvite
	}
	now := time.Now().UTC()
	code, err := s.generateInviteCode(ctx)
	if err != nil {
		return nil, err
	}
	inv := &SpaceInvite{
		Code:      code,
		SpaceID:   spaceID,
		InviterID: inviterID,
		MaxUses:   maxUses,
		CreatedAt: now,
	}
	if maxAge > 0 {
		exp := now.Add(time.Duration(maxAge) * time.Second)
		inv.ExpiresAt = &exp
	}
	if err := s.repo.CreateInvite(ctx, inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// ListInvites returns the space's live invites (expired ones are dropped as they are
// seen) with the inviters' profiles. Requires Manage space, Administrator, or owner.
func (s *Service) ListInvites(ctx context.Context, actorID, spaceID int64) ([]SpaceInvite, map[int64]*auth.User, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, nil, err
	} else if origin != "" {
		return s.fed.RemoteListInvites(ctx, origin, spaceID, actor)
	}
	if _, err := s.canManageInvites(ctx, actorID, spaceID); err != nil {
		return nil, nil, err
	}
	rows, err := s.repo.ListInvites(ctx, spaceID)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	live := rows[:0]
	for _, inv := range rows {
		if inv.Expired(now) || inv.Exhausted() {
			_ = s.repo.DeleteInvite(ctx, spaceID, inv.Code)
			continue
		}
		live = append(live, inv)
	}
	ids := make([]int64, 0, len(live))
	seen := map[int64]struct{}{}
	for _, inv := range live {
		if _, ok := seen[inv.InviterID]; !ok {
			seen[inv.InviterID] = struct{}{}
			ids = append(ids, inv.InviterID)
		}
	}
	users := make(map[int64]*auth.User, len(ids))
	if len(ids) > 0 {
		if list, err := s.userRepo.GetByIDs(ctx, ids); err == nil {
			for _, u := range list {
				if u != nil {
					users[u.ID] = u
				}
			}
		}
	}
	return live, users, nil
}

// DeleteInvite revokes an invite. Allowed for whoever can manage invites, and for the
// member who created it.
func (s *Service) DeleteInvite(ctx context.Context, actorID, spaceID int64, code string) error {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		// The client shows the code as code@origin; the origin knows the bare code.
		bare, _, perr := ParseInviteCode(code)
		if perr != nil {
			return ErrInviteNotFound
		}
		return s.fed.RemoteDeleteInvite(ctx, origin, spaceID, actor, bare)
	}
	inv, err := s.repo.GetInviteByCode(ctx, code)
	if err != nil {
		return err
	}
	if inv == nil || inv.SpaceID != spaceID {
		return ErrInviteNotFound
	}
	if inv.InviterID != actorID {
		if _, err := s.canManageInvites(ctx, actorID, spaceID); err != nil {
			return err
		}
	} else if ok, err := s.repo.IsMember(ctx, spaceID, actorID); err != nil || !ok {
		if err != nil {
			return err
		}
		return ErrNotMember
	}
	if err := s.repo.DeleteInvite(ctx, spaceID, code); err != nil {
		return err
	}
	// A revoked widget invite must not keep being advertised.
	if sp, _ := s.repo.GetByID(ctx, spaceID); sp != nil && sp.WidgetInviteCode == code {
		_ = s.repo.UpdateSpaceFields(ctx, spaceID, map[string]interface{}{"widget_invite_code": ""})
	}
	s.audit(ctx, spaceID, actorID, AuditInviteDelete, code, nil, "")
	return nil
}

// consumeInvite validates an invite for a join and records the use. Returns the invite
// (for the space id) or ErrInviteNotFound when it is missing, expired or used up.
func (s *Service) consumeInvite(ctx context.Context, code string, countUse bool) (*SpaceInvite, error) {
	if code == "" {
		return nil, ErrInviteNotFound
	}
	inv, err := s.repo.GetInviteByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		return nil, ErrInviteNotFound
	}
	now := time.Now().UTC()
	if inv.Expired(now) || inv.Exhausted() {
		_ = s.repo.DeleteInvite(ctx, inv.SpaceID, inv.Code)
		return nil, ErrInviteNotFound
	}
	if countUse {
		inv.CurrentUses++
		if inv.Exhausted() {
			// Last use: the invite is gone for everyone else, like Discord.
			_ = s.repo.DeleteInvite(ctx, inv.SpaceID, inv.Code)
		} else {
			_ = s.repo.SetInviteUses(ctx, inv.SpaceID, inv.Code, inv.CurrentUses)
		}
	}
	return inv, nil
}
