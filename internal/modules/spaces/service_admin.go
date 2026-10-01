package spaces

import (
	"context"
	"errors"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	// ErrAlreadyOwner is returned when a transfer names the current owner.
	ErrAlreadyOwner = errors.New("that member already owns this space")
	// ErrSpaceNameMismatch is the typed-name confirmation failing (see DeleteSpace).
	ErrSpaceNameMismatch = errors.New("the name you typed does not match this space")
)

// assertOwner loads the space and checks the actor owns it. ErrNotSpaceOwner (not
// ErrMissingPerm) guards the two things only the owner may do - hand the space over and
// delete it. Administrator is deliberately not enough; on Discord it isn't either.
func (s *Service) assertOwner(ctx context.Context, actorID, spaceID int64) (*Space, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, ErrSpaceNotFound
	}
	if sp.OwnerID != actorID {
		return nil, ErrNotSpaceOwner
	}
	return sp, nil
}

// TransferOwnership hands the space to another member. Owner only: an Administrator
// cannot give the space away, and cannot take it. The previous owner stays a member with
// whatever roles they hold, which is what Discord does - and since role hierarchy is
// evaluated against the *current* owner, they lose their ceiling-free standing at once.
func (s *Service) TransferOwnership(ctx context.Context, actorID, spaceID, newOwnerID int64) (*Space, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteTransferOwnership(ctx, origin, spaceID, actor, newOwnerID)
	}
	sp, err := s.assertOwner(ctx, actorID, spaceID)
	if err != nil {
		return nil, err
	}
	if newOwnerID == sp.OwnerID {
		return nil, ErrAlreadyOwner
	}
	member, err := s.repo.GetMember(ctx, spaceID, newOwnerID)
	if err != nil {
		return nil, err
	}
	if member == nil {
		return nil, ErrNotMember
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateSpaceFields(ctx, spaceID, map[string]interface{}{
		"owner_id":   newOwnerID,
		"updated_at": now,
	}); err != nil {
		return nil, err
	}
	// Every permission answer depends on who the owner is.
	s.invalidateSnapshot(ctx, spaceID)
	s.audit(ctx, spaceID, actorID, AuditOwnershipTransfer, id.Format(newOwnerID), map[string]change{
		"owner_id": {Old: id.Format(sp.OwnerID), New: id.Format(newOwnerID)},
	}, "")
	updated, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || updated == nil {
		return nil, ErrSpaceNotFound
	}
	if s.redis != nil {
		stargate.PublishToSpace(ctx, s.redis, spaceID, "SPACE_UPDATE", spaceToEventPayload(updated), s.stargateRegion())
	}
	s.fedSpaceUpdated(ctx, updated)
	return updated, nil
}

// DeleteSpace removes a space and everything in it. Owner only, and the caller must echo
// back the space's name - the same confirmation Discord asks for, here enforced by the
// server so it holds however the request was made.
//
// Order matters: members go first (each one's spaces_by_user row and their client's own
// copy), then the things keyed outside the space partition, then the partitions
// themselves. A failure part-way leaves a smaller space rather than orphaned rows in
// somebody's sidebar.
func (s *Service) DeleteSpace(ctx context.Context, actorID, spaceID int64, confirmName string) error {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return err
	} else if origin != "" {
		return s.fed.RemoteDeleteSpace(ctx, origin, spaceID, actor, confirmName)
	}
	sp, err := s.assertOwner(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if confirmName != sp.Name {
		return ErrSpaceNameMismatch
	}
	if s.fed != nil {
		s.fed.AfterSpaceDeleted(ctx, sp)
	}
	return s.deleteSpaceCascade(ctx, actorID, sp)
}

// TakeDownSpace removes a space on an instance administrator's authority, owner or not.
// The owner-only rule on DeleteSpace is about a space's *own* hierarchy - Administrator
// must not be enough to destroy someone's space. The person running the server is above
// that hierarchy, and this is how abuse reports about a space get acted on. Callers gate
// it (the instance module checks instance admin); it is recorded in the instance audit
// log there, not the space's, since the space is about to stop existing.
func (s *Service) TakeDownSpace(ctx context.Context, actorID, spaceID int64) error {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return ErrSpaceNotFound
	}
	if err := s.assertLocal(ctx, spaceID); err != nil {
		return err // a mirror is taken down by leaving it, or by its origin
	}
	if s.fed != nil {
		s.fed.AfterSpaceDeleted(ctx, sp)
	}
	return s.deleteSpaceCascade(ctx, actorID, sp)
}

// deleteSpaceCascade is the removal itself, shared by the owner's delete, an
// administrator's takedown and dropping a mirror. See DeleteSpace for why the order
// matters.
func (s *Service) deleteSpaceCascade(ctx context.Context, actorID int64, sp *Space) error {
	spaceID := sp.ID
	members, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil {
		return err
	}
	// Announce before the rows go, so a client that is listening drops the space in one
	// step instead of noticing at the next reload.
	if s.redis != nil {
		stargate.PublishToSpace(ctx, s.redis, spaceID, "SPACE_DELETE", map[string]interface{}{
			"space_id": id.Format(spaceID),
		}, s.stargateRegion())
	}
	for _, m := range members {
		if err := s.repo.RemoveMember(ctx, spaceID, m.UserID); err != nil {
			return err
		}
		if s.redis != nil {
			// The per-user channel is what a session that is not subscribed to the space
			// (another device, a reconnect in flight) hears.
			stargate.PublishToUser(ctx, s.redis, m.UserID, "SPACE_LEAVE", map[string]interface{}{
				"space_id": id.Format(spaceID),
			}, s.stargateRegion())
		}
	}

	// Rooms belong to the rooms module; deleting each one also clears its overrides.
	roomRows, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, row := range roomRows {
		if err := s.roomRepo.DeleteSpaceRoom(ctx, spaceID, row.RoomID); err != nil {
			return err
		}
	}

	// Invites and emoji each live in a second, differently-partitioned lookup table.
	invites, err := s.repo.ListInvites(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, inv := range invites {
		if err := s.repo.DeleteInvite(ctx, spaceID, inv.Code); err != nil {
			return err
		}
	}
	emojis, err := s.repo.ListEmojis(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, e := range emojis {
		if err := s.repo.DeleteEmoji(ctx, spaceID, e.ID); err != nil {
			return err
		}
	}

	if err := s.repo.DeleteSpace(ctx, spaceID); err != nil {
		return err
	}
	s.invalidateSnapshot(ctx, spaceID)
	logger.Info("spaces", "space %d (%q) deleted by %d, %d members removed", spaceID, sp.Name, actorID, len(members))
	return nil
}
