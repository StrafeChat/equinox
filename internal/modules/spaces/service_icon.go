package spaces

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// UpdateSpaceIcon sets the space icon URL (caller uploads to Nebula first). Requires PermManageSpace or owner.
func (s *Service) UpdateSpaceIcon(ctx context.Context, actorID, spaceID int64, iconURL string) (*Space, error) {
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return nil, err
	}
	if !permissions.Has(base, permissions.PermManageSpace) && !permissions.Has(base, permissions.PermAdministrator) {
		return nil, ErrInsufficientSpacePermission
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
	return sp, nil
}
