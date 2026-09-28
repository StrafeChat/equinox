package captcha

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisReplayGuard shares spent-challenge state across API instances.
type redisReplayGuard struct {
	rdb    *redis.Client
	prefix string
}

// NewRedisReplayGuard returns a ReplayGuard backed by Redis. prefix namespaces the keys
// alongside the rest of the instance's cache entries.
func NewRedisReplayGuard(rdb *redis.Client, prefix string) ReplayGuard {
	return &redisReplayGuard{rdb: rdb, prefix: prefix + "captcha:spent:"}
}

// Consume is one atomic SET NX with an expiry: the first caller to claim an id wins, and
// the key disappears on its own once the challenge could no longer be valid anyway.
func (r *redisReplayGuard) Consume(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	return r.rdb.SetNX(ctx, r.prefix+id, 1, ttl).Result()
}
