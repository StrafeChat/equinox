package auth

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
)

const sessionCacheMaxTTL = 24 * time.Hour

// CachedSessionRepository wraps a SessionRepository with Redis cache-aside for GetByTokenHash.
type CachedSessionRepository struct {
	repo   SessionRepository
	redis  *redis.Client
	prefix string
}

// NewCachedSessionRepository returns a session repo that caches GetByTokenHash.
func NewCachedSessionRepository(repo SessionRepository, redis *redis.Client, cfg *config.Config) SessionRepository {
	if redis == nil || !cfg.Database.Redis.CacheEnabled {
		return repo
	}
	prefix := cfg.Database.Redis.CachePrefix
	if prefix != "" && !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}
	return &CachedSessionRepository{
		repo:   repo,
		redis:  redis,
		prefix: prefix + "session:",
	}
}

func (r *CachedSessionRepository) Create(ctx context.Context, s *Session) error {
	return r.repo.Create(ctx, s)
}

func (r *CachedSessionRepository) GetByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	key := r.prefix + tokenHash
	val, err := r.redis.Get(ctx, key).Bytes()
	if err == nil {
		var s Session
		if json.Unmarshal(val, &s) == nil {
			if !s.RevokedAt.IsZero() || time.Now().UTC().After(s.ExpiresAt) {
				_ = r.redis.Del(ctx, key)
				return nil, nil
			}
			return &s, nil
		}
	}

	s, err := r.repo.GetByTokenHash(ctx, tokenHash)
	if err != nil || s == nil {
		return s, err
	}

	ttl := time.Until(s.ExpiresAt)
	if ttl <= 0 {
		return s, nil
	}
	if ttl > sessionCacheMaxTTL {
		ttl = sessionCacheMaxTTL
	}

	b, _ := json.Marshal(s)
	_ = r.redis.Set(ctx, key, b, ttl)
	return s, nil
}

func (r *CachedSessionRepository) GetByUserSession(ctx context.Context, userID, sessionID int64) (*Session, error) {
	return r.repo.GetByUserSession(ctx, userID, sessionID)
}

func (r *CachedSessionRepository) ListByUser(ctx context.Context, userID int64) ([]Session, error) {
	return r.repo.ListByUser(ctx, userID)
}

func (r *CachedSessionRepository) Revoke(ctx context.Context, userID, sessionID int64) error {
	sess, err := r.repo.GetByUserSession(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	if err := r.repo.Revoke(ctx, userID, sessionID); err != nil {
		return err
	}
	if sess != nil && sess.TokenHash != "" {
		_ = r.redis.Del(ctx, r.prefix+sess.TokenHash)
	}
	return nil
}

func (r *CachedSessionRepository) RevokeAllForUser(ctx context.Context, userID int64) error {
	sessions, err := r.repo.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	if err := r.repo.RevokeAllForUser(ctx, userID); err != nil {
		return err
	}
	for _, s := range sessions {
		if s.RevokedAt.IsZero() && s.TokenHash != "" {
			_ = r.redis.Del(ctx, r.prefix+s.TokenHash)
		}
	}
	return nil
}
