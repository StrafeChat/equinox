package discover

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// RemoveListing deletes a listing and drops that directory's cached page, for something
// that stopped existing (an application deleted with its bot). It needs only the table and
// Redis, so callers without a discover service (the applications module's after-delete
// hook) can use it. prefix is the Redis cache prefix as configured (no trailing ':' added,
// matching Service.cacheKey).
func RemoveListing(ctx context.Context, repo Repository, rdb *redis.Client, prefix, kind string, id int64) error {
	if err := repo.Delete(ctx, kind, id); err != nil {
		return err
	}
	if rdb != nil {
		_ = rdb.Del(ctx, prefix+"discover:"+kind).Err()
	}
	return nil
}

// RemoveListing is the package function on this service's own repository and cache.
func (s *Service) RemoveListing(ctx context.Context, kind string, id int64) error {
	return RemoveListing(ctx, s.repo, s.redis, s.prefix, kind, id)
}
