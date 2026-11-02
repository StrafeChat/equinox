package spaces

import (
	"context"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// publishSpaceEvent sends an event to the whole space (its own channel). An event about one
// of its rooms names it in data["room_id"], which scopes the frame to that room too.
func (s *Service) publishSpaceEvent(ctx context.Context, spaceID int64, eventType string, data map[string]interface{}) {
	s.publishToSpaceChannel(ctx, stargate.InSpace(spaceID).About(roomFromData(data)), eventType, data)
}

func (s *Service) publishToSpaceChannel(ctx context.Context, sc stargate.Scope, eventType string, data map[string]interface{}) {
	if s.redis == nil {
		return
	}
	if data == nil {
		data = make(map[string]interface{})
	}
	data["space_id"] = id.Format(sc.SpaceID)
	stargate.PublishToSpaceChannel(ctx, s.redis, sc, eventType, data, s.stargateRegion())
}

// publishOpenRoomCreate announces a new room on the space's own channel, and subscribes
// everyone who hears it to the room. Only a room open to everyone in the space is announced
// this way (any other goes to its viewers, publishRoomCreate), which is why the gateway does
// not re-check the subscription for each member.
func (s *Service) publishOpenRoomCreate(ctx context.Context, spaceID, roomID int64, payload map[string]interface{}) {
	s.publishToSpaceChannel(ctx, stargate.InSpace(spaceID).About(roomID).Subscribing(roomID), "SPACE_ROOM_CREATE", payload)
	// The room was open when the caller looked, and the gateway does not check this grant per
	// member. A View deny that landed in between (on the room's section, or @everyone losing
	// View) was announced before the room was there to drop, so look again: if it is no longer
	// open, tell each member where they stand, which ends the grant for anyone now kept out.
	if !s.OpenToEveryone(ctx, spaceID, roomID) {
		s.republishRoomVisibility(ctx, spaceID, []int64{roomID})
	}
}

// roomFromData is the room an event's data names (room_id), 0 when it names none.
func roomFromData(data map[string]interface{}) int64 {
	raw, _ := data["room_id"].(string)
	if raw == "" {
		return 0
	}
	rid, err := id.Parse(raw)
	if err != nil {
		return 0
	}
	return rid
}

// publishRoomEvent is publishSpaceEvent for an event that describes a room: it goes to the
// whole space only when nobody is kept out of the room, otherwise to the members who can view
// it, so a private room's name and topic reach no one else.
func (s *Service) publishRoomEvent(ctx context.Context, spaceID, roomID int64, eventType string, data map[string]interface{}) {
	if s.redis == nil {
		return
	}
	sc := stargate.InSpace(spaceID).About(roomID)
	viewers, everyone := s.ViewersOf(ctx, spaceID, roomID)
	if everyone {
		s.publishToSpaceChannel(ctx, sc, eventType, data)
		return
	}
	if data == nil {
		data = make(map[string]interface{})
	}
	data["space_id"] = id.Format(spaceID)
	stargate.PublishToUsersIn(ctx, s.redis, viewers, sc, eventType, data, s.stargateRegion())
}

func spaceRoleEventData(r *SpaceRole) map[string]interface{} {
	return RoleMap(r)
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

func roomUserOverrideEventData(o *SpaceRoomUserOverride) map[string]interface{} {
	return map[string]interface{}{
		"space_id":   id.Format(o.SpaceID),
		"room_id":    id.Format(o.RoomID),
		"user_id":    id.Format(o.UserID),
		"allow":      o.Allow,
		"deny":       o.Deny,
		"created_at": o.CreatedAt,
		"updated_at": o.UpdatedAt,
	}
}
