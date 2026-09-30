package stargate

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// BotTokenResolver maps a hashed bot token to its bot account - the applications
// repository's ResolveBotToken. Kept as an interface so the gateway never imports the
// developer-platform module.
type BotTokenResolver interface {
	ResolveBotToken(ctx context.Context, tokenHash string) (botUserID, appID int64, err error)
}

// Resolver validates session tokens (and, when a BotTokenResolver is wired in, bot
// tokens) using auth repositories.
type Resolver struct {
	sessionRepo auth.SessionRepository
	userRepo    auth.UserRepository
	bots        BotTokenResolver
}

func NewResolver(sessionRepo auth.SessionRepository, userRepo auth.UserRepository) *Resolver {
	return &Resolver{
		sessionRepo: sessionRepo,
		userRepo:    userRepo,
	}
}

// WithBots enables `Authorization: Bot <token>` on the gateway.
func (r *Resolver) WithBots(b BotTokenResolver) *Resolver {
	r.bots = b
	return r
}

// Resolve validates tokenHash (SHA256 of hex-decoded token) and returns user + session.
func (r *Resolver) Resolve(ctx context.Context, tokenHash string) (*auth.User, *auth.Session, error) {
	sess, err := r.sessionRepo.GetByTokenHash(ctx, tokenHash)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	if !sess.RevokedAt.IsZero() {
		return nil, nil, nil
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		return nil, nil, nil
	}

	u, err := r.userRepo.GetByID(ctx, sess.UserID)
	if err != nil {
		return nil, nil, err
	}
	if u == nil {
		return nil, nil, nil
	}

	return u, sess, nil
}

// ResolveBot validates a hashed bot token (same hashing as a session token) and returns
// the bot account, or nil when bots are not enabled or the token is unknown.
func (r *Resolver) ResolveBot(ctx context.Context, tokenHash string) (*auth.User, error) {
	if r.bots == nil {
		return nil, nil
	}
	botID, _, err := r.bots.ResolveBotToken(ctx, tokenHash)
	if err != nil || botID == 0 {
		return nil, err
	}
	u, err := r.userRepo.GetByID(ctx, botID)
	if err != nil || u == nil || !u.Bot {
		return nil, err
	}
	return u, nil
}
