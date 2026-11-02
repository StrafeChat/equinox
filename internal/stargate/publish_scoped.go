package stargate

import (
	"context"
	"encoding/json"
	"maps"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
)

// Scope is what an event is about: the space it belongs to and the room it concerns, as the
// event frame's space_id / room_id (Discord's guild_id / channel_id) - not the channel it is
// published on. Subscribe lists the "space" channels the event gives its recipients.
type Scope struct {
	SpaceID   int64 // 0: no space (a PM, a group PM, an account event)
	RoomID    int64 // 0: the space as a whole
	Subscribe []int64
}

// InRoom scopes an event to a room; spaceID is the room's space, nil for a PM or group PM.
func InRoom(spaceID *int64, roomID int64) Scope {
	sc := Scope{RoomID: roomID}
	if spaceID != nil {
		sc.SpaceID = *spaceID
	}
	return sc
}

// InSpace scopes an event to a space as a whole.
func InSpace(spaceID int64) Scope {
	return Scope{SpaceID: spaceID}
}

// About narrows a space's scope to one of its rooms; 0 leaves it space-wide.
func (s Scope) About(roomID int64) Scope {
	s.RoomID = roomID
	return s
}

// Subscribing adds channels the event opens to whoever receives it.
func (s Scope) Subscribing(ids ...int64) Scope {
	s.Subscribe = append(append([]int64(nil), s.Subscribe...), ids...)
	return s
}

// PublishToRoom sends an event to a room's channel (stargate:space:<room>), which only the
// room's subscribers hear - a private room's events never reach the whole space.
func PublishToRoom(ctx context.Context, rdb *redis.Client, sc Scope, eventType string, data interface{}, region string) {
	if sc.RoomID == 0 {
		logger.Warn("stargate", "PublishToRoom without a room: dropped %s", eventType)
		return
	}
	routeID := id.Format(sc.RoomID)
	publishScoped(ctx, rdb, redisChannel("space", routeID), RedisEnvelope{SpaceID: routeID}, sc, eventType, scopedData(sc, data), region)
}

// PublishToSpaceChannel sends an event to a space's own channel (stargate:space:<space>),
// heard by every member subscribed to the space. sc.RoomID names a room only when the event
// concerns one that everyone in the space may see.
func PublishToSpaceChannel(ctx context.Context, rdb *redis.Client, sc Scope, eventType string, data interface{}, region string) {
	if sc.SpaceID == 0 {
		logger.Warn("stargate", "PublishToSpaceChannel without a space: dropped %s", eventType)
		return
	}
	routeID := id.Format(sc.SpaceID)
	publishScoped(ctx, rdb, redisChannel("space", routeID), RedisEnvelope{SpaceID: routeID}, sc, eventType, scopedData(sc, data), region)
}

// PublishToUserIn sends an event about a space or room to a user's own channel. Events about
// the account alone (relationships, devices, sessions, the user's own presence) stay on
// PublishToUser: they belong to no space.
func PublishToUserIn(ctx context.Context, rdb *redis.Client, userID int64, sc Scope, eventType string, data interface{}, region string) {
	publishToUserScoped(ctx, rdb, userID, sc, eventType, scopedData(sc, data), region)
}

// PublishToUsersIn is PublishToUserIn for several users.
func PublishToUsersIn(ctx context.Context, rdb *redis.Client, userIDs []int64, sc Scope, eventType string, data interface{}, region string) {
	if len(userIDs) == 0 {
		return
	}
	data = scopedData(sc, data) // one copy serves every recipient
	for _, uid := range userIDs {
		publishToUserScoped(ctx, rdb, uid, sc, eventType, data, region)
	}
}

func publishToUserScoped(ctx context.Context, rdb *redis.Client, userID int64, sc Scope, eventType string, data interface{}, region string) {
	routeID := id.Format(userID)
	publishScoped(ctx, rdb, redisChannel("user", routeID), RedisEnvelope{UserID: routeID}, sc, eventType, data, region)
}

// publishScoped completes env, which names the routing channel, with the scope and the
// (already scoped) payload, and publishes it on ch.
func publishScoped(ctx context.Context, rdb *redis.Client, ch string, env RedisEnvelope, sc Scope, eventType string, data interface{}, region string) {
	if rdb == nil {
		return
	}
	env.Type = eventType
	env.From = 0 // server-originated
	env.Data = data
	env.Region = defaultRegion(region)
	env.Scoped = true
	if sc.SpaceID != 0 {
		env.ScopeSpaceID = id.Format(sc.SpaceID)
	}
	if sc.RoomID != 0 {
		env.ScopeRoomID = id.Format(sc.RoomID)
	}
	for _, sub := range sc.Subscribe {
		if sub != 0 {
			env.Subscribe = append(env.Subscribe, id.Format(sub))
		}
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	if err := rdb.Publish(ctx, ch, raw).Err(); err != nil {
		logger.Err("stargate", err, map[string]any{"channel": ch, "event": eventType})
	}
}

// scopedData gives a map payload the scope's room_id and space_id (an event about a room
// carries both, Discord's channel_id and guild_id; neither is set to an empty value). The map
// is copied first: callers publish one map to several channels, and one space's id must not
// leak into the next publish. Any other payload goes out as it is.
func scopedData(sc Scope, data interface{}) interface{} {
	m, ok := data.(map[string]interface{})
	if !ok || m == nil || (sc.SpaceID == 0 && sc.RoomID == 0) {
		return data
	}
	out := maps.Clone(m)
	if sc.RoomID != 0 {
		out["room_id"] = id.Format(sc.RoomID)
	}
	if sc.SpaceID != 0 {
		out["space_id"] = id.Format(sc.SpaceID)
	}
	return out
}
