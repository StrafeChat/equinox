package spaces

import (
	"context"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// JoinListed adds the caller to a space hosted here without an invite: the join behind
// the Discover directory. The discover module has checked that an administrator listed
// the space; this applies what an invite join applies - the ban check, the @everyone
// role, the join notice and events. Already a member: nothing changes.
func (s *Service) JoinListed(ctx context.Context, actorID, spaceID int64) (*Space, error) {
	if err := s.assertLocal(ctx, spaceID); err != nil {
		return nil, err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, ErrSpaceNotFound
	}
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		if ban, err := s.repo.GetBan(ctx, spaceID, actorID); err != nil {
			return nil, err
		} else if ban != nil {
			return nil, ErrBanned
		}
		eid, err := s.ensureEveryoneRoleID(ctx, sp)
		if err != nil {
			return nil, err
		}
		if err := s.addMember(ctx, sp, actorID, []int64{eid}); err != nil {
			return nil, err
		}
	}
	s.attachFederation(ctx, sp)
	return sp, nil
}

// PublicCounts is how many members a space has and how many of them are online now -
// what a Discover card shows (the widget document computes the same for itself).
func (s *Service) PublicCounts(ctx context.Context, spaceID int64) (members, online int, err error) {
	rows, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil {
		return 0, 0, err
	}
	members = len(rows)
	if members == 0 {
		return 0, 0, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.UserID)
	}
	users, err := s.userRepo.GetByIDs(ctx, ids)
	if err != nil {
		return members, 0, nil
	}
	for _, u := range users {
		if u != nil && auth.ToPublicPresence(u.Presence, true).Status != "offline" {
			online++
		}
	}
	return members, online, nil
}
