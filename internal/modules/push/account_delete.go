package push

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// Purger removes a deleted account's push subscriptions. It needs only the table and
// Redis, not a running push service, so the account-deletion pipeline can use it whether
// or not PUSH_ENABLED is on (subscriptions may predate turning it off).
type Purger struct {
	repo  Repository
	redis *redis.Client
}

func NewPurger(repo Repository, rdb *redis.Client) *Purger { return &Purger{repo: repo, redis: rdb} }

// PurgeUser removes every push subscription of a deleted account and takes it out of the
// push:users index. Session revocation already dropped each session's subscription through
// the revoke hook (only installed in the API process with push on); this catches the rest.
// Returns how many there were.
func (p *Purger) PurgeUser(ctx context.Context, userID int64) (int, error) {
	subs, err := p.repo.ListByUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	var firstErr error
	for _, sub := range subs {
		if _, err := p.repo.Delete(ctx, userID, sub.SessionID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	markUnsubscribed(ctx, p.redis, userID)
	return len(subs), firstErr
}

// CountSubscriptions is the deletion preview's push-subscription count.
func (p *Purger) CountSubscriptions(ctx context.Context, userID int64) (int, error) {
	subs, err := p.repo.ListByUser(ctx, userID)
	return len(subs), err
}
