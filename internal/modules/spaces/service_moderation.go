package spaces

import "context"

// SendPolicy is the messages module's one read for the verification level and automod
// (messages.SpaceChannelAuth). The member row is only fetched when the space has something
// on, so a space with no moderation settings costs one read per message, not two.
func (s *Service) SendPolicy(ctx context.Context, spaceID, userID int64) (*SendPolicy, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, ErrSpaceNotFound
	}
	p := &SendPolicy{
		VerificationLevel:   sp.VerificationLevel,
		AutomodFlags:        sp.AutomodFlags,
		AutomodMentionLimit: sp.AutomodMentionLimit,
	}
	if !p.Enforced() {
		return p, nil
	}
	m, err := s.repo.GetMember(ctx, spaceID, userID)
	if err != nil {
		return nil, err
	}
	if m != nil {
		p.JoinedAt = m.JoinedAt
		for _, rid := range m.RoleIDs {
			if rid != sp.EveryoneRoleID {
				p.HasRole = true
				break
			}
		}
	}
	return p, nil
}

// InviteBelongsToSpace is the automod invite-link rule's question: is this code one of
// the space's own invites (fine to post) or someone else's (blocked).
func (s *Service) InviteBelongsToSpace(ctx context.Context, code string, spaceID int64) (bool, error) {
	inv, err := s.repo.GetInviteByCode(ctx, code)
	if err != nil {
		return false, err
	}
	return inv != nil && inv.SpaceID == spaceID, nil
}
