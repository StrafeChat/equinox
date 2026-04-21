package spaces

import (
	"context"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// PatchSpace updates allowed fields (name / acronym). Requires Manage space, Administrator, or owner.
func (s *Service) PatchSpace(ctx context.Context, actorID, spaceID int64, in *PatchSpaceInput) (*Space, error) {
	if in == nil || in.Name == nil {
		return nil, ErrNothingToPatch
	}
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return nil, err
	}
	if !permissions.Has(base, permissions.PermManageSpace) && !permissions.Has(base, permissions.PermAdministrator) {
		return nil, ErrInsufficientSpacePermission
	}
	name := strings.TrimSpace(*in.Name)
	if name == "" {
		return nil, ErrInvalidSpaceName
	}
	if len(name) > 100 {
		name = name[:100]
	}
	ac := nameAcronym(name)
	now := time.Now().UTC()
	if err := s.repo.UpdateSpaceName(ctx, spaceID, name, ac, now); err != nil {
		return nil, err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	if s.redis != nil {
		stargate.PublishToSpace(ctx, s.redis, spaceID, "SPACE_UPDATE", spaceToEventPayload(sp), s.stargateRegion())
	}
	return sp, nil
}
