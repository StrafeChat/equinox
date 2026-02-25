package stargate

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/logger"
)

func defaultRegion(region string) string {
	if region == "" {
		return "default"
	}
	return region
}

// PublishToUsers publishes the same event to multiple users (e.g. RELATIONSHIP_ADD to both parties).
func PublishToUsers(ctx context.Context, redis *redis.Client, userIDs []int64, eventType string, data interface{}, region string) {
	region = defaultRegion(region)
	for _, uid := range userIDs {
		PublishToUser(ctx, redis, uid, eventType, data, region)
	}
}

// PublishToSpace sends an event to a room/space WebSocket channel.
// Clients subscribed to space:{room_id} receive it.
func PublishToSpace(ctx context.Context, redis *redis.Client, roomID int64, eventType string, data interface{}, region string) {
	region = defaultRegion(region)
	env := RedisEnvelope{
		Type:    eventType,
		SpaceID: strconv.FormatInt(roomID, 10),
		From:    0,
		Data:   data,
		Region: region,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	ch := redisChannel("space", strconv.FormatInt(roomID, 10))
	if err := redis.Publish(ctx, ch, raw).Err(); err != nil {
		logger.Err("stargate", err, map[string]any{"channel": ch, "event": eventType})
	}
}

// PublishToUser sends an event to a user's WebSocket channel.
// Recipients must be subscribed to user:{their_id} to receive.
func PublishToUser(ctx context.Context, redis *redis.Client, userID int64, eventType string, data interface{}, region string) {
	region = defaultRegion(region)
	env := RedisEnvelope{
		Type:   eventType,
		UserID: strconv.FormatInt(userID, 10),
		From:   0, // server-originated
		Data:   data,
		Region: region,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	ch := redisChannel("user", strconv.FormatInt(userID, 10))
	if err := redis.Publish(ctx, ch, raw).Err(); err != nil {
		logger.Err("stargate", err, map[string]any{"channel": ch, "event": eventType})
	}
}
