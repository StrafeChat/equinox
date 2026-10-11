package push

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/logger"
)

var (
	ErrDisabled        = errors.New("push is disabled")
	ErrNoSubscription  = errors.New("no push subscription for this session")
	ErrSessionRequired = errors.New("push subscriptions belong to a signed-in session")
)

const (
	// maxFailures is how many consecutive rejected deliveries a subscription survives.
	maxFailures = 5
	// successWriteEvery throttles the last_success_at write: a busy account would otherwise
	// write its subscription row on every message.
	successWriteEvery = 10 * time.Minute
)

// Service owns subscriptions and delivery.
type Service struct {
	repo    Repository
	dir     Directory
	redis   *redis.Client
	sender  *Sender
	vapid   *VAPID
	checker *endpointChecker
	cfg     config.PushConfig
	now     func() time.Time
}

// NewService wires the push service. vapid is the instance identity (LoadVAPID).
func NewService(repo Repository, dir Directory, rdb *redis.Client, cfg config.PushConfig, vapid *VAPID) *Service {
	return &Service{
		repo:    repo,
		dir:     dir,
		redis:   rdb,
		sender:  NewSender(cfg, vapid),
		vapid:   vapid,
		checker: newEndpointChecker(cfg),
		cfg:     cfg,
		now:     time.Now,
	}
}

// VAPIDPublicKey is the instance's application-server key, base64url.
func (s *Service) VAPIDPublicKey() string { return s.vapid.PublicKey() }

// Subscribe upserts the session's subscription: re-subscribing (a new endpoint after a
// token refresh, new keys after a reinstall) replaces it.
func (s *Service) Subscribe(ctx context.Context, userID, sessionID int64, in *SubscribeInput) (*Subscription, error) {
	in.Endpoint = strings.TrimSpace(in.Endpoint)
	prefs, err := s.checker.validateSubscribe(ctx, in)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	sub := &Subscription{
		UserID: userID, SessionID: sessionID,
		Endpoint: in.Endpoint, P256dh: strings.TrimSpace(in.Keys.P256dh), Auth: strings.TrimSpace(in.Keys.Auth), Kind: in.Kind,
		CreatedAt: now, UpdatedAt: now,
	}
	sub.setPrefs(prefs)
	if prev, err := s.repo.Get(ctx, userID, sessionID); err == nil && prev != nil && usable(prev) {
		sub.CreatedAt = prev.CreatedAt
		// The same endpoint and keys keep their delivery history; anything new starts clean.
		if prev.Endpoint == sub.Endpoint && prev.P256dh == sub.P256dh && prev.Auth == sub.Auth {
			sub.LastSuccessAt, sub.LastError, sub.Failures = prev.LastSuccessAt, prev.LastError, prev.Failures
		}
	}
	if err := s.repo.Put(ctx, sub); err != nil {
		return nil, err
	}
	markSubscribed(ctx, s.redis, userID)
	return sub, nil
}

// Update changes the preferences of the session's subscription.
func (s *Service) Update(ctx context.Context, userID, sessionID int64, in *UpdateInput) (*Subscription, error) {
	sub, err := s.Get(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	prefs, err := applyPrefs(sub.Prefs(), in.Prefs)
	if err != nil {
		return nil, err
	}
	sub.setPrefs(prefs)
	sub.UpdatedAt = s.now().UTC()
	if err := s.repo.UpdatePrefs(ctx, userID, sessionID, prefs, sub.UpdatedAt); err != nil {
		return nil, err
	}
	return sub, nil
}

// Get returns the session's subscription, or ErrNoSubscription.
func (s *Service) Get(ctx context.Context, userID, sessionID int64) (*Subscription, error) {
	sub, err := s.repo.Get(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	if sub == nil || !usable(sub) {
		return nil, ErrNoSubscription
	}
	return sub, nil
}

// Delete removes the session's subscription. Idempotent.
func (s *Service) Delete(ctx context.Context, userID, sessionID int64) error {
	remaining, err := s.repo.Delete(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	if remaining == 0 {
		markUnsubscribed(ctx, s.redis, userID)
	}
	return nil
}

// ForgetSession is the session-revocation hook (auth.SetSessionRevokedHook): a signed-out,
// revoked or banned session's device stops getting pushes at once, not at its next failed
// delivery.
func (s *Service) ForgetSession(ctx context.Context, userID, sessionID int64) {
	if err := s.Delete(ctx, userID, sessionID); err != nil {
		logger.Err("push", err, map[string]any{"user_id": userID, "step": "forget revoked session"})
	}
}

// Test sends a "test" push to the session's subscription right away, once, and reports
// what the push service answered - the settings screen's "send a test notification".
func (s *Service) Test(ctx context.Context, userID, sessionID int64) (*TestResult, error) {
	sub, err := s.Get(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(Payload{V: PayloadVersion, T: TypeTest, UID: formatID(userID), TS: s.now().UnixMilli()})
	if err != nil {
		return nil, err
	}
	out := s.sender.Send(ctx, sub, payload, deliveryFor(TypeTest, 0), false)
	s.record(ctx, sub, out)
	if out.kind == delivered {
		return &TestResult{Delivered: true}, nil
	}
	reason := out.reason
	if out.kind == gone {
		reason = "the push service no longer knows this subscription; it was removed"
	}
	return &TestResult{Delivered: false, Status: out.status, Error: reason}, nil
}

// record applies a delivery outcome to the subscription: a success clears its failure
// streak, a vanished or refused endpoint deletes it, and a rejection counts toward the
// five that delete it.
func (s *Service) record(ctx context.Context, sub *Subscription, out outcome) {
	switch out.kind {
	case delivered:
		now := s.now().UTC()
		if sub.Failures > 0 || sub.LastError != "" || sub.LastSuccessAt == nil || now.Sub(*sub.LastSuccessAt) > successWriteEvery {
			if err := s.repo.RecordSuccess(ctx, sub.UserID, sub.SessionID, now); err == nil {
				sub.LastSuccessAt, sub.LastError, sub.Failures = &now, "", 0
			}
		}
	case gone, refused:
		logger.Info("push", "dropping subscription of user %d at %s: %s", sub.UserID, endpointHost(sub.Endpoint), out.reason)
		_ = s.Delete(ctx, sub.UserID, sub.SessionID)
	case rejected, unavailable:
		failures := sub.Failures + 1
		if out.kind == rejected && failures >= maxFailures {
			logger.Info("push", "dropping subscription of user %d at %s after %d rejected deliveries (%s)", sub.UserID, endpointHost(sub.Endpoint), failures, out.reason)
			_ = s.Delete(ctx, sub.UserID, sub.SessionID)
			return
		}
		if err := s.repo.RecordFailure(ctx, sub.UserID, sub.SessionID, out.reason, failures); err == nil {
			sub.LastError, sub.Failures = out.reason, failures
		}
	}
}

// usable reports whether a stored row is a real subscription. An outcome written just
// after the row was deleted can leave a row holding only delivery bookkeeping.
func usable(sub *Subscription) bool {
	return sub.Endpoint != "" && sub.P256dh != "" && sub.Auth != ""
}

// RebuildIndex fills push:users from the subscriptions table, once per Redis (a flag
// marks it done), so the producers' filter is right after a Redis flush or on the first
// start with this feature.
func (s *Service) RebuildIndex(ctx context.Context) error {
	if s.redis == nil {
		return nil
	}
	if n, err := s.redis.Exists(ctx, usersReadyKey).Result(); err != nil || n > 0 {
		return err
	}
	count := 0
	err := s.repo.ForEachUser(ctx, func(uid int64) error {
		count++
		return s.redis.SAdd(ctx, usersKey, formatID(uid)).Err()
	})
	if err != nil {
		return err
	}
	logger.Info("push", "indexed %d accounts with push subscriptions", count)
	return s.redis.Set(ctx, usersReadyKey, "1", 0).Err()
}
