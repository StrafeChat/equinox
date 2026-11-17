package stargate

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
)

// SessionRevokedDeleted is the SESSION_REVOKED reason an account receives when an instance
// administrator deletes it (the hub closes its sockets as for any other reason).
const SessionRevokedDeleted = "deleted"

// ForgetUser drops the gateway's Redis state for an account that no longer exists: its
// entry in the online set, its activity hash (and a relayed federated one) and its place in
// the activity index. prefix is REDIS_CACHE_PREFIX as configured (the activity store's).
func ForgetUser(ctx context.Context, rdb *redis.Client, prefix string, userID int64) error {
	if rdb == nil {
		return nil
	}
	uid := id.Format(userID)
	pipe := rdb.Pipeline()
	pipe.SRem(ctx, OnlinePresenceKey, uid)
	pipe.Del(ctx, prefix+"presence:activity:"+uid, prefix+"presence:activity:fed:"+uid)
	pipe.SRem(ctx, prefix+"presence:activity:users", uid)
	_, err := pipe.Exec(ctx)
	return err
}
