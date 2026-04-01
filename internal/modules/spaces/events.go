package spaces

import (
	"context"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/stargate"
)

func (s *Service) publishSpaceEvent(ctx context.Context, spaceID int64, eventType string, data map[string]interface{}) {
	if s.redis == nil {
		return
	}
	if data == nil {
		data = make(map[string]interface{})
	}
	data["space_id"] = id.Format(spaceID)
	stargate.PublishToSpace(ctx, s.redis, spaceID, eventType, data, s.stargateRegion())
}

func spaceRoleEventData(r *SpaceRole) map[string]interface{} {
	return map[string]interface{}{
		"id":            id.Format(r.ID),
		"name":          r.Name,
		"permissions":   r.Permissions,
		"position":      r.Position,
		"color":         r.Color,
		"hoist":         r.Hoist,
		"mentionable":   r.Mentionable,
		"created_at":    r.CreatedAt,
		"updated_at":    r.UpdatedAt,
	}
}

func formatRoleIDStrings(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, x := range ids {
		out = append(out, id.Format(x))
	}
	return out
}

func roomOverrideEventData(o *SpaceRoomRoleOverride) map[string]interface{} {
	return map[string]interface{}{
		"space_id":   id.Format(o.SpaceID),
		"room_id":    id.Format(o.RoomID),
		"role_id":    id.Format(o.RoleID),
		"allow":      o.Allow,
		"deny":       o.Deny,
		"created_at": o.CreatedAt,
		"updated_at": o.UpdatedAt,
	}
}
