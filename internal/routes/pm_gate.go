package routes

import (
	"context"

	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// sharedSpaceChecker implements rooms.SharedSpaceChecker: two accounts "share a space" when
// both are members of at least one. Used by PM_POLICY=shared to decide who may start a
// direct message.
type sharedSpaceChecker struct {
	spaces *spaces.Service
}

func (c *sharedSpaceChecker) ShareSpace(ctx context.Context, a, b int64) (bool, error) {
	la, err := c.spaces.ListSpacesForUser(ctx, a)
	if err != nil || len(la) == 0 {
		return false, err
	}
	lb, err := c.spaces.ListSpacesForUser(ctx, b)
	if err != nil {
		return false, err
	}
	set := make(map[int64]struct{}, len(la))
	for _, s := range la {
		set[s.SpaceID] = struct{}{}
	}
	for _, s := range lb {
		if _, ok := set[s.SpaceID]; ok {
			return true, nil
		}
	}
	return false, nil
}
