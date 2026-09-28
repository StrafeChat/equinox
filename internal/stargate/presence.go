package stargate

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

const presenceUpdateEvent = "PRESENCE_UPDATE"

// OnlinePresenceKey is a Redis set of the user ids with at least one live gateway
// connection. Maintained here (SADD on connect, SREM on last disconnect) so a separate
// process - the API's admin stats - can read the count with SCARD without a database scan.
// Cleared on gateway startup (a fresh gateway has no clients), which self-heals a crash on
// a single-instance deployment; a multi-instance deployment would want per-node sets.
const OnlinePresenceKey = "presence:online"

// DefaultPresenceNotifier updates user presence in the DB and publishes to friends on connect/disconnect.
type DefaultPresenceNotifier struct {
	userRepo auth.UserRepository
	redis    *redis.Client
	region   string
}

func NewDefaultPresenceNotifier(userRepo auth.UserRepository, redis *redis.Client, region string) *DefaultPresenceNotifier {
	if region == "" {
		region = "default"
	}
	return &DefaultPresenceNotifier{userRepo: userRepo, redis: redis, region: region}
}

func (p *DefaultPresenceNotifier) OnConnect(ctx context.Context, userID int64) {
	p.setOnline(ctx, userID, true)
}

func (p *DefaultPresenceNotifier) OnDisconnect(ctx context.Context, userID int64) {
	p.setOnline(ctx, userID, false)
}

func (p *DefaultPresenceNotifier) setOnline(ctx context.Context, userID int64, online bool) {
	upd := &auth.ProfileUpdate{
		Presence: &auth.PresenceUpdate{Online: &online},
	}
	updated, err := p.userRepo.UpdateProfile(ctx, userID, upd)
	if err != nil {
		logger.Err("stargate", err, map[string]any{"user_id": userID, "online": online})
		return
	}
	// Track membership of the online set for the admin dashboard's live count. SADD is
	// idempotent (a second device re-adds harmlessly); SREM fires only on last disconnect,
	// because the hub calls OnDisconnect only when no other client for the user remains.
	if p.redis != nil {
		if online {
			p.redis.SAdd(ctx, OnlinePresenceKey, id.Format(userID))
		} else {
			p.redis.SRem(ctx, OnlinePresenceKey, id.Format(userID))
		}
	}
	if updated == nil {
		return
	}
	// Use public presence (status/custom_status only; invisible→offline for others)
	presenceSelf := auth.ToPublicPresence(updated.Presence, false)
	presenceOthers := auth.ToPublicPresence(updated.Presence, true)

	payloadSelf := map[string]interface{}{
		"user_id":  id.Format(updated.ID),
		"presence": presenceSelf,
	}
	payloadOthers := map[string]interface{}{
		"user_id":  id.Format(updated.ID),
		"presence": presenceOthers,
	}

	// Notify the user themselves (e.g. multiple devices)
	PublishToUser(ctx, p.redis, updated.ID, presenceUpdateEvent, payloadSelf, p.region)

	// Notify all friends
	friendIDs := updated.Relationships
	if len(friendIDs) > 0 {
		PublishToUsers(ctx, p.redis, friendIDs, presenceUpdateEvent, payloadOthers, p.region)
	}
}
