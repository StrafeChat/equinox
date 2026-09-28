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
	PublishPresenceUpdate(ctx, p.redis, p.region, updated)
}

// PublishPresenceUpdate broadcasts a user's presence to everyone who should see it: the
// user themselves (their real status, so multiple devices stay in sync), their friends, and
// every space they belong to (reaching all co-members subscribed to that space). Friends and
// spaces get the "others" view, where invisible reads as offline. Broadcasting to spaces -
// not just friends - is what keeps a member's status correct for people who share a space
// with them but are not friends; without it those statuses only ever reflected load time.
func PublishPresenceUpdate(ctx context.Context, rdb *redis.Client, region string, user *auth.User) {
	if rdb == nil || user == nil {
		return
	}
	region = defaultRegion(region)
	presenceSelf := auth.ToPublicPresence(user.Presence, false)
	presenceOthers := auth.ToPublicPresence(user.Presence, true)
	payloadSelf := map[string]interface{}{"user_id": id.Format(user.ID), "presence": presenceSelf}
	payloadOthers := map[string]interface{}{"user_id": id.Format(user.ID), "presence": presenceOthers}

	// The user's own devices (real status).
	PublishToUser(ctx, rdb, user.ID, presenceUpdateEvent, payloadSelf, region)
	// Friends (DM list / friends list).
	if len(user.Relationships) > 0 {
		PublishToUsers(ctx, rdb, user.Relationships, presenceUpdateEvent, payloadOthers, region)
	}
	// Every space the user is a member of - one publish per space, received by every
	// co-member subscribed to it. user.Spaces is maintained on join/leave.
	for _, spaceID := range user.Spaces {
		PublishToSpace(ctx, rdb, spaceID, presenceUpdateEvent, payloadOthers, region)
	}
}
