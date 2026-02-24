package stargate

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// Resolver validates session tokens using auth repositories.
type Resolver struct {
	sessionRepo auth.SessionRepository
	userRepo    auth.UserRepository
}

func NewResolver(sessionRepo auth.SessionRepository, userRepo auth.UserRepository) *Resolver {
	return &Resolver{
		sessionRepo: sessionRepo,
		userRepo:    userRepo,
	}
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
