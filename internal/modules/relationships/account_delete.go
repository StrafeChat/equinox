package relationships

import (
	"context"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// PurgeCounts is what PurgeUser tore down, for the deletion's audit line.
type PurgeCounts struct {
	Friends  int
	Requests int
}

// PurgeUser tears down every relationship of an account that is being deleted (a local
// account an instance administrator deletes, or on a peer the shadow of a remote account
// its home deleted): each friendship on both rows, with RELATIONSHIP_REMOVE to both sides
// and - for a remote friend - the `remove` relay to their home; each pending request either
// way, the same; and the nickname each friend gave the account. Blocks *by* the account go
// with its row; blocks *of* it have no reverse index and are pruned lazily by
// ListRelationships. The account's own nickname partition is dropped with its other
// partitions by the caller. Best-effort per counterpart: one failure is logged and the rest
// still run; the first error is returned at the end.
func (s *Service) PurgeUser(ctx context.Context, u *auth.User) (PurgeCounts, error) {
	var counts PurgeCounts
	var firstErr error
	note := func(stage string, other int64, err error) {
		if err == nil {
			return
		}
		logger.Err("relationships", err, map[string]any{"stage": stage, "user_id": u.ID, "other_id": other})
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, friendID := range u.Relationships {
		friend, _ := s.user.GetByID(ctx, friendID)
		if err := s.unfriend(ctx, u, friendID, friend); err != nil {
			note("purge_unfriend", friendID, err)
			continue
		}
		counts.Friends++
		note("purge_nickname", friendID, s.repo.DeleteNickname(ctx, friendID, u.ID))
		s.relayRemovedBy(ctx, u, friend)
	}

	incoming, err := s.repo.GetIncoming(ctx, u.ID)
	note("purge_incoming", 0, err)
	for _, req := range incoming {
		if err := s.repo.DeleteRequest(ctx, req.FromUserID, u.ID); err != nil {
			note("purge_request", req.FromUserID, err)
			continue
		}
		counts.Requests++
		from, _ := s.user.GetByID(ctx, req.FromUserID)
		s.publishRemoveTo(ctx, req.FromUserID, from, u.ID)
		s.relayRemovedBy(ctx, u, from)
	}

	outgoing, err := s.repo.GetOutgoing(ctx, u.ID)
	note("purge_outgoing", 0, err)
	for _, req := range outgoing {
		if err := s.repo.DeleteRequest(ctx, u.ID, req.ToUserID); err != nil {
			note("purge_request", req.ToUserID, err)
			continue
		}
		counts.Requests++
		to, _ := s.user.GetByID(ctx, req.ToUserID)
		s.publishRemoveTo(ctx, req.ToUserID, to, u.ID)
		s.relayRemovedBy(ctx, u, to)
	}
	return counts, firstErr
}

// relayRemovedBy is relayRemoved with the actor already in hand (the account being purged
// may be a shadow, and its row is about to go).
func (s *Service) relayRemovedBy(ctx context.Context, actor, other *auth.User) {
	if s.federator == nil || actor == nil || actor.IsRemote() || other == nil || !other.IsRemote() {
		return
	}
	s.federator.AfterRelationshipRemoved(ctx, actor, other)
}

// pruneDeleted drops friend and block ids whose user row no longer exists (an account an
// administrator deleted; ids never recur) from the account's own sets. Called only after a
// successful read showed them missing, and only ever removes - a removal writes a tombstone
// and cannot recreate a deleted row.
func (s *Service) pruneDeleted(ctx context.Context, u *auth.User, byID map[int64]*auth.User) {
	var friends, blocks []int64
	for _, fid := range u.Relationships {
		if byID[fid] == nil {
			friends = append(friends, fid)
		}
	}
	for _, bid := range u.Blocks {
		if byID[bid] == nil {
			blocks = append(blocks, bid)
		}
	}
	if len(friends) > 0 {
		if err := s.user.UpdateRelationships(ctx, u.ID, nil, friends); err != nil {
			logger.Err("relationships", err, map[string]any{"stage": "prune_friends", "user_id": u.ID})
		}
	}
	if len(blocks) > 0 {
		if err := s.user.UpdateBlocks(ctx, u.ID, nil, blocks); err != nil {
			logger.Err("relationships", err, map[string]any{"stage": "prune_blocks", "user_id": u.ID})
		}
	}
}

// RequestCounterparts lists the accounts with a pending friend request to userID (incoming)
// and from it (outgoing) - the deletion's snapshot, read before PurgeUser ends them.
func (s *Service) RequestCounterparts(ctx context.Context, userID int64) (incoming, outgoing []int64, err error) {
	in, err := s.repo.GetIncoming(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	out, err := s.repo.GetOutgoing(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range in {
		incoming = append(incoming, r.FromUserID)
	}
	for _, r := range out {
		outgoing = append(outgoing, r.ToUserID)
	}
	return incoming, outgoing, nil
}
