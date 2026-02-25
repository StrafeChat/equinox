package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
)

const userCacheTTL = 10 * time.Minute

// cachedUser is User without PasswordHash, for safe Redis storage.
type cachedUser struct {
	ID            int64        `json:"id"`
	Email         string       `json:"email"`
	Username      string       `json:"username"`
	Discriminator int          `json:"discriminator"`
	DisplayName   string       `json:"display_name"`
	Avatar        string       `json:"avatar"`
	Banner        string       `json:"banner"`
	Bot           bool         `json:"bot"`
	Bots          []string     `json:"bots"`
	System        bool         `json:"system"`
	Bio           string       `json:"bio"`
	Flags         int          `json:"flags"`
	Relationships []int64      `json:"relationships"`
	Spaces        []int64      `json:"spaces"`
	DateOfBirth   time.Time    `json:"date_of_birth"`
	VerifiedEmail bool         `json:"verified_email"`
	AboutMe       string       `json:"about_me"`
	AccentColor   string       `json:"accent_color"`
	Locale        string       `json:"locale"`
	Presence      UserPresence `json:"presence"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

func userToCached(u *User) *cachedUser {
	if u == nil {
		return nil
	}
	return &cachedUser{
		ID:            u.ID,
		Email:         u.Email,
		Username:      u.Username,
		Discriminator: u.Discriminator,
		DisplayName:   u.DisplayName,
		Avatar:        u.Avatar,
		Banner:        u.Banner,
		Bot:           u.Bot,
		Bots:          u.Bots,
		System:        u.System,
		Bio:           u.Bio,
		Flags:         u.Flags,
		Relationships: u.Relationships,
		Spaces:        u.Spaces,
		DateOfBirth:   u.DateOfBirth,
		VerifiedEmail: u.VerifiedEmail,
		AboutMe:       u.AboutMe,
		AccentColor:   u.AccentColor,
		Locale:        u.Locale,
		Presence:      u.Presence,
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
	}
}

func cachedToUser(c *cachedUser, passwordHash string) *User {
	if c == nil {
		return nil
	}
	return &User{
		ID:            c.ID,
		Email:         c.Email,
		PasswordHash:  passwordHash,
		Username:      c.Username,
		Discriminator: c.Discriminator,
		DisplayName:   c.DisplayName,
		Avatar:        c.Avatar,
		Banner:        c.Banner,
		Bot:           c.Bot,
		Bots:          c.Bots,
		System:        c.System,
		Bio:           c.Bio,
		Flags:         c.Flags,
		Relationships: c.Relationships,
		Spaces:        c.Spaces,
		DateOfBirth:   c.DateOfBirth,
		VerifiedEmail: c.VerifiedEmail,
		AboutMe:       c.AboutMe,
		AccentColor:   c.AccentColor,
		Locale:        c.Locale,
		Presence:      c.Presence,
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
}

// CachedUserRepository wraps a UserRepository with Redis cache-aside for GetByID and GetByUsernameDiscriminator.
type CachedUserRepository struct {
	repo   UserRepository
	redis  *redis.Client
	prefix string
}

// NewCachedUserRepository returns a user repo that caches GetByID and GetByUsernameDiscriminator.
func NewCachedUserRepository(repo UserRepository, redis *redis.Client, cfg *config.Config) UserRepository {
	if redis == nil || !cfg.Database.Redis.CacheEnabled {
		return repo
	}
	prefix := cfg.Database.Redis.CachePrefix
	if prefix != "" && !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}
	return &CachedUserRepository{
		repo:   repo,
		redis:  redis,
		prefix: prefix,
	}
}

func (r *CachedUserRepository) invalidateUser(ctx context.Context, id int64) {
	_ = r.redis.Del(ctx, r.userKey(id))
}

func (r *CachedUserRepository) userKey(id int64) string {
	return r.prefix + "user:" + fmt.Sprintf("%d", id)
}

func (r *CachedUserRepository) userUDKey(username string, discriminator int) string {
	return r.prefix + "user:ud:" + strings.ToLower(strings.TrimSpace(username)) + ":" + fmt.Sprintf("%d", discriminator)
}

func (r *CachedUserRepository) Create(ctx context.Context, u *User) error {
	return r.repo.Create(ctx, u)
}

func (r *CachedUserRepository) GetByID(ctx context.Context, id int64) (*User, error) {
	key := r.userKey(id)
	val, err := r.redis.Get(ctx, key).Bytes()
	if err == nil {
		var c cachedUser
		if json.Unmarshal(val, &c) == nil {
			return cachedToUser(&c, ""), nil
		}
	}

	u, err := r.repo.GetByID(ctx, id)
	if err != nil || u == nil {
		return u, err
	}

	b, _ := json.Marshal(userToCached(u))
	_ = r.redis.Set(ctx, key, b, userCacheTTL)
	return u, nil
}

func (r *CachedUserRepository) GetByIDs(ctx context.Context, ids []int64) ([]*User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]bool)
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	out := make([]*User, len(unique))
	for i, id := range unique {
		u, err := r.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out[i] = u
	}
	byID := make(map[int64]*User)
	for _, u := range out {
		if u != nil {
			byID[u.ID] = u
		}
	}
	result := make([]*User, len(ids))
	for i, id := range ids {
		result[i] = byID[id]
	}
	return result, nil
}

func (r *CachedUserRepository) GetByEmail(ctx context.Context, email string) (*User, error) {
	return r.repo.GetByEmail(ctx, email)
}

func (r *CachedUserRepository) GetByUsernameDiscriminator(ctx context.Context, username string, discriminator int) (*User, error) {
	key := r.userUDKey(username, discriminator)
	val, err := r.redis.Get(ctx, key).Bytes()
	if err == nil {
		var c cachedUser
		if json.Unmarshal(val, &c) == nil {
			return cachedToUser(&c, ""), nil
		}
	}

	u, err := r.repo.GetByUsernameDiscriminator(ctx, username, discriminator)
	if err != nil || u == nil {
		return u, err
	}

	b, _ := json.Marshal(userToCached(u))
	_ = r.redis.Set(ctx, key, b, userCacheTTL)
	_ = r.redis.Set(ctx, r.userKey(u.ID), b, userCacheTTL)
	return u, nil
}

func (r *CachedUserRepository) EmailExists(ctx context.Context, email string) (bool, error) {
	return r.repo.EmailExists(ctx, email)
}

func (r *CachedUserRepository) DiscriminatorsForUsername(ctx context.Context, username string) ([]int, error) {
	return r.repo.DiscriminatorsForUsername(ctx, username)
}

func (r *CachedUserRepository) UpdateRelationships(ctx context.Context, userID int64, add, remove []int64) error {
	if err := r.repo.UpdateRelationships(ctx, userID, add, remove); err != nil {
		return err
	}
	r.invalidateUser(context.Background(), userID)
	for _, id := range add {
		r.invalidateUser(context.Background(), id)
	}
	for _, id := range remove {
		r.invalidateUser(context.Background(), id)
	}
	return nil
}

func (r *CachedUserRepository) UpdateProfile(ctx context.Context, userID int64, upd *ProfileUpdate) (*User, error) {
	u, err := r.repo.UpdateProfile(ctx, userID, upd)
	if err != nil {
		return u, err
	}
	if u != nil {
		r.invalidateUser(context.Background(), userID)
	}
	return u, nil
}
