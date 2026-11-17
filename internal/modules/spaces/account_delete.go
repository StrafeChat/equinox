package spaces

import (
	"context"
	"errors"
	"fmt"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
)

// The spaces side of an instance administrator deleting an account
// (internal/modules/instance/account_delete.go), and of a peer forgetting a remote account
// its home deleted (federation/account_delete.go).

// OwnedSpace is a space an account owns, for the deletion preview's blockers. HostedOn is
// the origin's domain for a mirrored space ("" for one hosted here): such a space cannot be
// taken down from here and its owner cannot leave it.
type OwnedSpace struct {
	ID          int64
	Name        string
	MemberCount int
	HostedOn    string
}

// AccountSpaceIDs is every space userID is in: the spaces_by_user rows plus known (the
// users.spaces set the caller already holds), deduplicated.
func (s *Service) AccountSpaceIDs(ctx context.Context, userID int64, known []int64) ([]int64, error) {
	rows, err := s.repo.ListSpacesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []int64
	add := func(sid int64) {
		if sid != 0 && !seen[sid] {
			seen[sid] = true
			out = append(out, sid)
		}
	}
	for _, r := range rows {
		add(r.SpaceID)
	}
	for _, sid := range known {
		add(sid)
	}
	return out, nil
}

// OwnedSpaces lists the spaces among spaceIDs that userID owns.
func (s *Service) OwnedSpaces(ctx context.Context, userID int64, spaceIDs []int64) ([]OwnedSpace, error) {
	var out []OwnedSpace
	for _, sid := range spaceIDs {
		sp, err := s.repo.GetByID(ctx, sid)
		if err != nil {
			return nil, err
		}
		if sp == nil || sp.OwnerID != userID {
			continue
		}
		n, err := s.repo.CountMembers(ctx, sid)
		if err != nil {
			n = -1
		}
		out = append(out, OwnedSpace{ID: sid, Name: sp.Name, MemberCount: n, HostedOn: s.mirrorOrigin(ctx, sid)})
	}
	return out, nil
}

// mirrorDropper is the optional half of the Federator that drops a mirror nobody here is
// in any more (federation.Service has it; test fakes need not).
type mirrorDropper interface {
	DropMirrorIfEmpty(ctx context.Context, spaceID int64)
}

// RemoveAccount takes a deleted (or deleting) account out of a space: the full leave -
// member row and space-profile images, a bot's managed roles, SPACE_MEMBER_REMOVE, the
// voice hook, the "left" line and the relays to mirrors - with two differences from
// LeaveSpace:
//
//   - On a mirror whose origin refuses or cannot be reached, the account is removed here
//     anyway (and the mirror dropped if nobody here is left in it); the returned warning
//     says so. The account-deleted notice the pipeline sends later lets the origin catch up.
//   - On a space hosted here, the account's per-room user overrides and the invites it
//     created go too - except the space's widget invite, which belongs to the space by now.
//
// The owner of a space hosted here is refused (ErrOwnerCannotLeave): the pipeline takes such
// spaces down first or refuses the deletion. Idempotent: a space already gone or a
// membership already ended is not an error.
func (s *Service) RemoveAccount(ctx context.Context, userID, spaceID int64) (string, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return "", err
	}
	if sp == nil {
		return "", nil
	}
	if origin := s.mirrorOrigin(ctx, spaceID); origin != "" {
		return s.removeAccountFromMirror(ctx, userID, sp, origin)
	}
	if sp.OwnerID == userID {
		return "", ErrOwnerCannotLeave
	}
	m, err := s.repo.GetMember(ctx, spaceID, userID)
	if err != nil {
		return "", err
	}
	if m != nil {
		if err := s.removeMemberRow(ctx, spaceID, userID, m); err != nil {
			return "", err
		}
		s.afterMemberRemoved(ctx, spaceID, userID)
		s.announceLeave(ctx, spaceID, userID, "left")
		s.fedMemberRemoved(ctx, spaceID, userID, "left")
	}
	s.dropAccountOverrides(ctx, spaceID, userID)
	s.dropAccountInvites(ctx, sp, userID)
	return "", nil
}

func (s *Service) removeAccountFromMirror(ctx context.Context, userID int64, sp *Space, origin string) (string, error) {
	warning := ""
	if s.fed != nil {
		err := s.leaveRemote(ctx, userID, sp.ID, origin)
		switch {
		case err == nil:
			return "", nil
		case errors.Is(err, ErrNotMember):
			// Not a member here any more; nothing to tell the origin.
		default:
			logger.Err("spaces", err, map[string]any{"stage": "account_delete_leave_remote", "space_id": sp.ID, "user_id": userID})
			warning = fmt.Sprintf("%s could not be reached to leave %q; left locally, it will be told", origin, sp.Name)
		}
	}
	if err := s.RemoveMirrorMember(ctx, sp.ID, userID); err != nil {
		return warning, err
	}
	if d, ok := s.fed.(mirrorDropper); ok {
		d.DropMirrorIfEmpty(ctx, sp.ID)
	}
	return warning, nil
}

// dropAccountOverrides removes the user overrides that name userID in any room of a space
// hosted here. Harmless if left (the id never recurs) but they show as unknown rows in the
// room permission editors and travel to mirrors with every room relay.
func (s *Service) dropAccountOverrides(ctx context.Context, spaceID, userID int64) {
	rows, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		logger.Err("spaces", err, map[string]any{"stage": "account_delete_overrides", "space_id": spaceID})
		return
	}
	changed := false
	for _, r := range rows {
		ovs, err := s.repo.ListRoomUserOverrides(ctx, spaceID, r.RoomID)
		if err != nil {
			continue
		}
		for _, o := range ovs {
			if o.UserID != userID {
				continue
			}
			if err := s.repo.DeleteRoomUserOverride(ctx, spaceID, r.RoomID, userID); err != nil {
				logger.Err("spaces", err, map[string]any{"stage": "account_delete_override", "room_id": r.RoomID})
				continue
			}
			changed = true
			s.invalidateSnapshot(ctx, spaceID)
			s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_DELETE", map[string]interface{}{
				"room_id": id.Format(r.RoomID),
				"user_id": id.Format(userID),
			})
			s.fedRoomChanged(ctx, spaceID, r.RoomID, false)
		}
	}
	if changed {
		s.invalidateSnapshot(ctx, spaceID)
	}
}

// dropAccountInvites deletes the invites userID created in a space hosted here: they admit
// people in a deleted account's name. The space's widget invite stays - by the time it is
// on the space's widget it is the space's, not its creator's.
func (s *Service) dropAccountInvites(ctx context.Context, sp *Space, userID int64) {
	invites, err := s.repo.ListInvites(ctx, sp.ID)
	if err != nil {
		logger.Err("spaces", err, map[string]any{"stage": "account_delete_invites", "space_id": sp.ID})
		return
	}
	for _, inv := range invites {
		if inv.InviterID != userID || (sp.WidgetInviteCode != "" && inv.Code == sp.WidgetInviteCode) {
			continue
		}
		if err := s.repo.DeleteInvite(ctx, sp.ID, inv.Code); err != nil {
			logger.Err("spaces", err, map[string]any{"stage": "account_delete_invite", "space_id": sp.ID})
		}
	}
}
