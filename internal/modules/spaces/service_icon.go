package spaces

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// CanManageSpace: Manage Space or Administrator (owner implicitly). The icon handler
// checks this before accepting the upload so an unauthorised member can't fill Nebula.
func (s *Service) CanManageSpace(ctx context.Context, actorID, spaceID int64) error {
	// On a mirror this is the local copy of the roles, which is what the origin will
	// check too; the write itself is forwarded by the method that follows.
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if !permissions.Has(base, permissions.PermManageSpace) && !permissions.Has(base, permissions.PermAdministrator) {
		return ErrInsufficientSpacePermission
	}
	return nil
}

// UpdateSpaceIcon sets the space icon URL (caller uploads to Nebula first). Requires PermManageSpace or owner.
func (s *Service) UpdateSpaceIcon(ctx context.Context, actorID, spaceID int64, iconURL string) (*Space, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteSetSpaceImage(ctx, origin, spaceID, actor, "icon", iconURL)
	}
	if err := s.CanManageSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateSpaceIcon(ctx, spaceID, iconURL, now); err != nil {
		return nil, err
	}
	sp, err = s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	if s.redis != nil {
		stargate.PublishToSpace(ctx, s.redis, spaceID, "SPACE_UPDATE", spaceToEventPayload(sp), s.stargateRegion())
	}
	s.fedSpaceUpdated(ctx, sp)
	s.audit(ctx, spaceID, actorID, AuditSpaceUpdate, id.Format(spaceID), map[string]change{"icon": {New: iconURL}}, "")
	return sp, nil
}

// UpdateSpaceBanner sets the space banner URL (caller uploads to Nebula first). Requires PermManageSpace or owner.
func (s *Service) UpdateSpaceBanner(ctx context.Context, actorID, spaceID int64, bannerURL string) (*Space, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteSetSpaceImage(ctx, origin, spaceID, actor, "banner", bannerURL)
	}
	if err := s.CanManageSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateSpaceBanner(ctx, spaceID, bannerURL, now); err != nil {
		return nil, err
	}
	sp, err = s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	if s.redis != nil {
		stargate.PublishToSpace(ctx, s.redis, spaceID, "SPACE_UPDATE", spaceToEventPayload(sp), s.stargateRegion())
	}
	s.fedSpaceUpdated(ctx, sp)
	s.audit(ctx, spaceID, actorID, AuditSpaceUpdate, id.Format(spaceID), map[string]change{"banner": {New: bannerURL}}, "")
	return sp, nil
}
