package stargate

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

const presenceUpdateEvent = "PRESENCE_UPDATE"

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
	if updated == nil {
		return
	}

	payload := map[string]interface{}{
		"user_id":  id.Format(updated.ID),
		"presence": updated.Presence,
	}

	// Notify the user themselves (e.g. multiple devices)
	PublishToUser(ctx, p.redis, updated.ID, presenceUpdateEvent, payload, p.region)

	// Notify all friends
	friendIDs := updated.Relationships
	if len(friendIDs) > 0 {
		PublishToUsers(ctx, p.redis, friendIDs, presenceUpdateEvent, payload, p.region)
	}
}
