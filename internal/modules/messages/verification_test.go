package messages

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// levelUsers is a user table of fresh, unverified accounts: a person, a bot and the system one.
type levelUsers struct{ auth.UserRepository }

func (levelUsers) GetByID(_ context.Context, uid int64) (*auth.User, error) {
	return &auth.User{ID: uid, CreatedAt: time.Now(), Bot: uid == 2, System: uid == 3}, nil
}

// A verification level slows down new people. A bot has no email to verify and was let in by
// someone who could manage the space, so it is held to no level, as on Discord; neither is the
// instance's system account. A person with no role still is.
func TestVerificationLevelsHoldPeopleNotBots(t *testing.T) {
	cfg := &config.Config{}
	cfg.Mail.Enabled = true
	s := &Service{userRepo: levelUsers{}, cfg: cfg}
	p := &spaces.SendPolicy{VerificationLevel: spaces.VerificationHigh, JoinedAt: time.Now()}
	ctx := context.Background()
	if err := s.enforceVerificationLevel(ctx, 1, p); !errors.Is(err, ErrVerificationLevel) {
		t.Fatalf("a fresh, unverified person with no role: %v, want the level's refusal", err)
	}
	if err := s.enforceVerificationLevel(ctx, 2, p); err != nil {
		t.Fatalf("a bot: %v", err)
	}
	if err := s.enforceVerificationLevel(ctx, 3, p); err != nil {
		t.Fatalf("the system account: %v", err)
	}
}
