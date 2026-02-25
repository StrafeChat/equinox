package relationships

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
)

const relListCacheTTL = 3 * time.Minute

var (
	ErrUserNotFound        = errors.New("user not found")
	ErrInvalidDiscriminator = errors.New("invalid discriminator")
	ErrSelfRequest         = errors.New("cannot send request to yourself")
	ErrAlreadyFriends      = errors.New("already friends")
	ErrRequestExists       = errors.New("request already sent")
	ErrRequestNotFound     = errors.New("request not found")
)

type Service struct {
	repo   Repository
	user   auth.UserRepository
	redis  *redis.Client
	cfg    *config.Config
}

func NewService(repo Repository, user auth.UserRepository, redis *redis.Client, cfg *config.Config) *Service {
	return &Service{repo: repo, user: user, redis: redis, cfg: cfg}
}

func (s *Service) relListKey(userID int64) string {
	prefix := s.cfg.Database.Redis.CachePrefix
	if prefix != "" && !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}
	return prefix + "rel:" + strconv.FormatInt(userID, 10) + ":list"
}

func (s *Service) invalidateRelList(ctx context.Context, userIDs ...int64) {
	if s.redis == nil || !s.cfg.Database.Redis.CacheEnabled {
		return
	}
	for _, uid := range userIDs {
		_ = s.redis.Del(ctx, s.relListKey(uid))
	}
}

// parseDiscriminator parses string discriminator (e.g. "1234", "0") to int (1-9999 or 0).
func parseDiscriminator(s string) (int, error) {
	if s == "" {
		return 0, ErrInvalidDiscriminator
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 9999 {
		return 0, ErrInvalidDiscriminator
	}
	return n, nil
}

// SendRequest sends a friend request from actor to target. Both username and discriminator are required.
func (s *Service) SendRequest(ctx context.Context, actorID int64, in SendRequestInput) error {
	discriminator, err := parseDiscriminator(in.Discriminator)
	if err != nil {
		return err
	}
	target, err := s.user.GetByUsernameDiscriminator(ctx, in.Username, discriminator)
	if err != nil || target == nil {
		return ErrUserNotFound
	}
	targetID := target.ID

	if targetID == actorID {
		return ErrSelfRequest
	}

	me, err := s.user.GetByID(ctx, actorID)
	if err != nil || me == nil {
		return ErrUserNotFound
	}

	// Already friends?
	for _, r := range me.Relationships {
		if r == targetID {
			return ErrAlreadyFriends
		}
	}

	exists, err := s.repo.HasRequest(ctx, actorID, targetID)
	if err != nil {
		return err
	}
	if exists {
		return ErrRequestExists
	}

	// Check inverse (they sent to us)
	exists, _ = s.repo.HasRequest(ctx, targetID, actorID)
	if exists {
		// They already sent us a request - auto-accept
		return s.AcceptRequest(ctx, actorID, targetID)
	}

	if err := s.repo.CreateRequest(ctx, actorID, targetID); err != nil {
		return err
	}

	// Publish to recipient for real-time delivery
	payload := map[string]interface{}{
		"from_user_id": id.Format(actorID),
		"to_user_id":   id.Format(targetID),
		"created_at":   time.Now().UTC(),
		"from": map[string]interface{}{
			"id":            id.Format(me.ID),
			"username":      me.Username,
			"discriminator": me.Discriminator,
			"display_name":  me.DisplayName,
		},
	}
	stargate.PublishToUser(ctx, s.redis, targetID, "RELATIONSHIP_REQUEST", payload, s.cfg.Stargate.Region)
	s.invalidateRelList(ctx, actorID, targetID)

	return nil
}

// SendRequestByID sends a friend request by user id.
func (s *Service) SendRequestByID(ctx context.Context, actorID, targetID int64) error {
	if targetID == actorID {
		return ErrSelfRequest
	}

	me, err := s.user.GetByID(ctx, actorID)
	if err != nil || me == nil {
		return ErrUserNotFound
	}

	target, err := s.user.GetByID(ctx, targetID)
	if err != nil || target == nil {
		return ErrUserNotFound
	}

	for _, r := range me.Relationships {
		if r == targetID {
			return ErrAlreadyFriends
		}
	}

	exists, err := s.repo.HasRequest(ctx, actorID, targetID)
	if err != nil {
		return err
	}
	if exists {
		return ErrRequestExists
	}

	// They sent to us - accept
	exists, _ = s.repo.HasRequest(ctx, targetID, actorID)
	if exists {
		return s.AcceptRequest(ctx, actorID, targetID)
	}

	// We're sending to them
	if err := s.repo.CreateRequest(ctx, actorID, targetID); err != nil {
		return err
	}

	payload := map[string]interface{}{
		"from_user_id": id.Format(actorID),
		"to_user_id":   id.Format(targetID),
		"created_at":   time.Now().UTC(),
		"from": map[string]interface{}{
			"id":            id.Format(me.ID),
			"username":      me.Username,
			"discriminator": me.Discriminator,
			"display_name":  me.DisplayName,
		},
	}
	stargate.PublishToUser(ctx, s.redis, targetID, "RELATIONSHIP_REQUEST", payload, s.cfg.Stargate.Region)
	s.invalidateRelList(ctx, actorID, targetID)

	return nil
}

// AcceptRequest accepts a request from fromUserID.
func (s *Service) AcceptRequest(ctx context.Context, actorID, fromUserID int64) error {
	exists, err := s.repo.HasRequest(ctx, fromUserID, actorID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrRequestNotFound
	}

	if err := s.repo.DeleteRequest(ctx, fromUserID, actorID); err != nil {
		return err
	}

	if err := s.user.UpdateRelationships(ctx, actorID, []int64{fromUserID}, nil); err != nil {
		return err
	}
	if err := s.user.UpdateRelationships(ctx, fromUserID, []int64{actorID}, nil); err != nil {
		return err
	}

	// Real-time: both users receive RELATIONSHIP_ADD
	actor, _ := s.user.GetByID(ctx, actorID)
	fromUser, _ := s.user.GetByID(ctx, fromUserID)
	payload := map[string]interface{}{
		"id":   id.Format(fromUserID),
		"type": TypeFriend,
		"user": partialUser(actor),
	}
	stargate.PublishToUser(ctx, s.redis, fromUserID, "RELATIONSHIP_ADD", payload, s.cfg.Stargate.Region)
	payload2 := map[string]interface{}{
		"id":   id.Format(actorID),
		"type": TypeFriend,
		"user": partialUser(fromUser),
	}
	stargate.PublishToUser(ctx, s.redis, actorID, "RELATIONSHIP_ADD", payload2, s.cfg.Stargate.Region)
	s.invalidateRelList(ctx, actorID, fromUserID)
	return nil
}

// RejectRequest rejects a request.
func (s *Service) RejectRequest(ctx context.Context, actorID, fromUserID int64) error {
	exists, err := s.repo.HasRequest(ctx, fromUserID, actorID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrRequestNotFound
	}
	if err := s.repo.DeleteRequest(ctx, fromUserID, actorID); err != nil {
		return err
	}
	// Sender receives RELATIONSHIP_REMOVE (their outgoing request was rejected)
	payload := map[string]interface{}{"id": id.Format(actorID)}
	stargate.PublishToUser(ctx, s.redis, fromUserID, "RELATIONSHIP_REMOVE", payload, s.cfg.Stargate.Region)
	s.invalidateRelList(ctx, actorID, fromUserID)
	return nil
}

// partialUser builds a partial user object.
func partialUser(u *auth.User) map[string]interface{} {
	if u == nil {
		return nil
	}
	return map[string]interface{}{
		"id":            id.Format(u.ID),
		"username":      u.Username,
		"discriminator": fmt.Sprintf("%04d", u.Discriminator),
		"display_name":  u.DisplayName,
		"avatar":        u.Avatar,
	}
}

func buildRelationshipFromUser(u *auth.User, targetID int64, relType int, since *time.Time, nickname *string, userIgnored bool) *Relationship {
	if u == nil {
		return nil
	}
	rel := &Relationship{
		ID:              id.Format(targetID),
		Type:            relType,
		User:            partialUser(u),
		Nickname:        nickname,
		UserIgnored:     userIgnored,
		IsSpamRequest:   false,
		StrangerRequest: false,
	}
	if since != nil {
		iso := since.UTC().Format(time.RFC3339Nano)
		rel.Since = &iso
	}
	return rel
}

// ListRelationships returns all relationships. Batch-fetches users for performance.
func (s *Service) ListRelationships(ctx context.Context, actorID int64) ([]Relationship, error) {
	if s.redis != nil && s.cfg.Database.Redis.CacheEnabled {
		key := s.relListKey(actorID)
		val, err := s.redis.Get(ctx, key).Bytes()
		if err == nil {
			var out []Relationship
			if json.Unmarshal(val, &out) == nil {
				return out, nil
			}
		}
	}

	u, err := s.user.GetByID(ctx, actorID)
	if err != nil || u == nil {
		return nil, ErrUserNotFound
	}

	var ids []int64
	ids = append(ids, u.Relationships...)

	inReqs, err := s.repo.GetIncoming(ctx, actorID)
	if err != nil {
		return nil, err
	}
	for _, req := range inReqs {
		ids = append(ids, req.FromUserID)
	}

	outReqs, err := s.repo.GetOutgoing(ctx, actorID)
	if err != nil {
		return nil, err
	}
	for _, req := range outReqs {
		ids = append(ids, req.ToUserID)
	}

	users, err := s.user.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*auth.User)
	for i, id := range ids {
		if i < len(users) && users[i] != nil {
			byID[id] = users[i]
		}
	}

	nicknames, err := s.repo.GetNicknames(ctx, actorID, u.Relationships)
	if err != nil {
		return nil, err
	}

	var out []Relationship
	for _, friendID := range u.Relationships {
		var nick *string
		if n, ok := nicknames[friendID]; ok {
			nick = &n
		}
		if r := buildRelationshipFromUser(byID[friendID], friendID, TypeFriend, nil, nick, false); r != nil {
			out = append(out, *r)
		}
	}
	for _, req := range inReqs {
		if r := buildRelationshipFromUser(byID[req.FromUserID], req.FromUserID, TypeIncomingRequest, &req.CreatedAt, nil, false); r != nil {
			out = append(out, *r)
		}
	}
	for _, req := range outReqs {
		if r := buildRelationshipFromUser(byID[req.ToUserID], req.ToUserID, TypeOutgoingRequest, &req.CreatedAt, nil, false); r != nil {
			out = append(out, *r)
		}
	}

	if s.redis != nil && s.cfg.Database.Redis.CacheEnabled {
		if b, err := json.Marshal(out); err == nil {
			_ = s.redis.Set(ctx, s.relListKey(actorID), b, relListCacheTTL)
		}
	}
	return out, nil
}

// Delete removes a relationship.
func (s *Service) Delete(ctx context.Context, actorID, targetID int64) error {
	// Try unfriend first
	if err := s.RemoveFriend(ctx, actorID, targetID); err == nil {
		return nil
	}
	// Try reject (they sent to us)
	if err := s.RejectRequest(ctx, actorID, targetID); err == nil {
		return nil
	}
	// Try cancel (we sent to them)
	return s.CancelRequest(ctx, actorID, targetID)
}

// CancelRequest cancels an outgoing request.
func (s *Service) CancelRequest(ctx context.Context, actorID, targetID int64) error {
	exists, err := s.repo.HasRequest(ctx, actorID, targetID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrRequestNotFound
	}
	if err := s.repo.DeleteRequest(ctx, actorID, targetID); err != nil {
		return err
	}
	// Recipient receives RELATIONSHIP_REMOVE (incoming request was cancelled)
	payload := map[string]interface{}{"id": id.Format(actorID)}
	stargate.PublishToUser(ctx, s.redis, targetID, "RELATIONSHIP_REMOVE", payload, s.cfg.Stargate.Region)
	s.invalidateRelList(ctx, actorID, targetID)
	return nil
}

// Patch updates relationship metadata (nickname).
func (s *Service) Patch(ctx context.Context, actorID, targetID int64, nickname *string) error {
	u, err := s.user.GetByID(ctx, actorID)
	if err != nil || u == nil {
		return ErrUserNotFound
	}
	var isFriend bool
	for _, r := range u.Relationships {
		if r == targetID {
			isFriend = true
			break
		}
	}
	if !isFriend {
		return ErrRequestNotFound
	}
	if nickname == nil {
		return nil // no change requested
	}
	if *nickname == "" {
		err = s.repo.DeleteNickname(ctx, actorID, targetID)
	} else {
		err = s.repo.SetNickname(ctx, actorID, targetID, *nickname)
	}
	if err != nil {
		return err
	}
	s.invalidateRelList(ctx, actorID)
	return nil
}

// RemoveFriend removes a friend.
func (s *Service) RemoveFriend(ctx context.Context, actorID, friendID int64) error {
	u, err := s.user.GetByID(ctx, actorID)
	if err != nil || u == nil {
		return ErrUserNotFound
	}
	found := false
	for _, r := range u.Relationships {
		if r == friendID {
			found = true
			break
		}
	}
	if !found {
		return ErrRequestNotFound // or ErrNotFriends
	}

	if err := s.user.UpdateRelationships(ctx, actorID, nil, []int64{friendID}); err != nil {
		return err
	}
	if err := s.user.UpdateRelationships(ctx, friendID, nil, []int64{actorID}); err != nil {
		return err
	}

	// Real-time: both users receive RELATIONSHIP_REMOVE
	payload := map[string]interface{}{"id": id.Format(friendID)}
	stargate.PublishToUser(ctx, s.redis, actorID, "RELATIONSHIP_REMOVE", payload, s.cfg.Stargate.Region)
	stargate.PublishToUser(ctx, s.redis, friendID, "RELATIONSHIP_REMOVE", map[string]interface{}{"id": id.Format(actorID)}, s.cfg.Stargate.Region)
	s.invalidateRelList(ctx, actorID, friendID)
	return nil
}

// BulkDelete removes multiple relationships.
func (s *Service) BulkDelete(ctx context.Context, actorID int64, relationshipType int) error {
	if relationshipType != TypeIncomingRequest {
		return ErrRequestNotFound
	}
	incoming, err := s.repo.GetIncoming(ctx, actorID)
	if err != nil {
		return err
	}
	var lastErr error
	for _, req := range incoming {
		if err := s.repo.DeleteRequest(ctx, req.FromUserID, req.ToUserID); err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		s.invalidateRelList(ctx, actorID)
	}
	return lastErr
}
