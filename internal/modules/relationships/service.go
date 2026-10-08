package relationships

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	ErrUserNotFound    = errors.New("user not found")
	ErrSelfRequest     = errors.New("cannot send request to yourself")
	ErrBotTarget       = errors.New("bots cannot be added as friends")
	ErrAlreadyFriends  = errors.New("already friends")
	ErrBlocked         = errors.New("blocked")
	ErrRequestExists   = errors.New("request already sent")
	ErrRequestNotFound = errors.New("request not found")
)

// Federator tells a remote user's home instance about relationship changes a local user
// made towards them. Implemented by internal/federation; nil when this instance doesn't
// federate. Hooks are best-effort and must not block the request - the local write has
// already happened.
type Federator interface {
	AfterRelationshipRequested(ctx context.Context, actor, target *auth.User)
	AfterRelationshipAccepted(ctx context.Context, actor, target *auth.User)
	AfterRelationshipRemoved(ctx context.Context, actor, target *auth.User)
}

type Service struct {
	repo      Repository
	user      auth.UserRepository
	redis     *redis.Client
	cfg       *config.Config
	federator Federator
}

func NewService(repo Repository, user auth.UserRepository, redis *redis.Client, cfg *config.Config) *Service {
	return &Service{repo: repo, user: user, redis: redis, cfg: cfg}
}

// SetFederator wires outbound federation for relationships.
func (s *Service) SetFederator(f Federator) {
	s.federator = f
}

// FindLocalUser looks a user up by username on this instance (case-insensitive).
func (s *Service) FindLocalUser(ctx context.Context, username string) (*auth.User, error) {
	return s.user.GetByUsername(ctx, username)
}

func isFriend(u *auth.User, otherID int64) bool {
	for _, r := range u.Relationships {
		if r == otherID {
			return true
		}
	}
	return false
}

// SendRequestTo sends a friend request from actor to target. The target may be the shadow
// of a user on another instance: their home instance is told and shows them the request.
func (s *Service) SendRequestTo(ctx context.Context, actorID int64, target *auth.User) error {
	if target == nil {
		return ErrUserNotFound
	}
	if target.ID == actorID {
		return ErrSelfRequest
	}
	// A bot is added to a space, never to a friends list; the official account is not a
	// person to befriend either.
	if target.Bot || target.System {
		return ErrBotTarget
	}

	me, err := s.user.GetByID(ctx, actorID)
	if err != nil || me == nil {
		return ErrUserNotFound
	}
	if s.blockedBetween(me, target) {
		return ErrBlocked
	}
	if isFriend(me, target.ID) {
		return ErrAlreadyFriends
	}

	exists, err := s.repo.HasRequest(ctx, actorID, target.ID)
	if err != nil {
		return err
	}
	if exists {
		return ErrRequestExists
	}
	// They already asked us - that's a match, not a second request.
	if exists, _ = s.repo.HasRequest(ctx, target.ID, actorID); exists {
		return s.acceptRequest(ctx, me, target)
	}

	if err := s.repo.CreateRequest(ctx, actorID, target.ID); err != nil {
		return err
	}
	s.publishRequest(ctx, me, target)
	if s.federator != nil && target.IsRemote() {
		s.federator.AfterRelationshipRequested(ctx, me, target)
	}
	return nil
}

// SendRequestByID sends a friend request by user id.
func (s *Service) SendRequestByID(ctx context.Context, actorID, targetID int64) error {
	if targetID == actorID {
		return ErrSelfRequest
	}
	target, err := s.user.GetByID(ctx, targetID)
	if err != nil || target == nil {
		return ErrUserNotFound
	}
	return s.SendRequestTo(ctx, actorID, target)
}

// AcceptRequest accepts the request fromUserID sent to actorID.
func (s *Service) AcceptRequest(ctx context.Context, actorID, fromUserID int64) error {
	actor, err := s.user.GetByID(ctx, actorID)
	if err != nil || actor == nil {
		return ErrUserNotFound
	}
	from, err := s.user.GetByID(ctx, fromUserID)
	if err != nil || from == nil {
		return ErrUserNotFound
	}
	return s.acceptRequest(ctx, actor, from)
}

// acceptRequest turns from's pending request to actor into a friendship.
func (s *Service) acceptRequest(ctx context.Context, actor, from *auth.User) error {
	exists, err := s.repo.HasRequest(ctx, from.ID, actor.ID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrRequestNotFound
	}
	if err := s.befriend(ctx, actor, from); err != nil {
		return err
	}
	if s.federator != nil && from.IsRemote() {
		s.federator.AfterRelationshipAccepted(ctx, actor, from)
	}
	return nil
}

// befriend deletes from's request to actor, records the friendship on both rows and tells
// both local parties. Shared by a local accept and one relayed from another instance.
func (s *Service) befriend(ctx context.Context, actor, from *auth.User) error {
	if err := s.repo.DeleteRequest(ctx, from.ID, actor.ID); err != nil {
		return err
	}
	if err := s.user.UpdateRelationships(ctx, actor.ID, []int64{from.ID}, nil); err != nil {
		return err
	}
	if err := s.user.UpdateRelationships(ctx, from.ID, []int64{actor.ID}, nil); err != nil {
		return err
	}
	s.publishAdd(ctx, from, actor, TypeFriend)
	s.publishAdd(ctx, actor, from, TypeFriend)
	return nil
}

// RejectRequest declines the request fromUserID sent to actorID.
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
	// The sender's outgoing request is gone.
	from, _ := s.user.GetByID(ctx, fromUserID)
	s.publishRemoveTo(ctx, fromUserID, from, actorID)
	s.relayRemoved(ctx, actorID, from)
	return nil
}

// CancelRequest withdraws actorID's request to targetID.
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
	// The recipient's incoming request is gone.
	target, _ := s.user.GetByID(ctx, targetID)
	s.publishRemoveTo(ctx, targetID, target, actorID)
	s.relayRemoved(ctx, actorID, target)
	return nil
}

// RemoveFriend ends the friendship between actorID and friendID.
func (s *Service) RemoveFriend(ctx context.Context, actorID, friendID int64) error {
	u, err := s.user.GetByID(ctx, actorID)
	if err != nil || u == nil {
		return ErrUserNotFound
	}
	if !isFriend(u, friendID) {
		return ErrRequestNotFound
	}
	friend, _ := s.user.GetByID(ctx, friendID)
	if err := s.unfriend(ctx, u, friendID, friend); err != nil {
		return err
	}
	s.relayRemoved(ctx, actorID, friend)
	return nil
}

// unfriend drops the friendship from both rows and tells both local parties. friend may be
// nil (row gone); the ids are what matter.
func (s *Service) unfriend(ctx context.Context, actor *auth.User, friendID int64, friend *auth.User) error {
	if err := s.user.UpdateRelationships(ctx, actor.ID, nil, []int64{friendID}); err != nil {
		return err
	}
	if err := s.user.UpdateRelationships(ctx, friendID, nil, []int64{actor.ID}); err != nil {
		return err
	}
	s.publishRemoveTo(ctx, actor.ID, actor, friendID)
	s.publishRemoveTo(ctx, friendID, friend, actor.ID)
	return nil
}

// relayRemoved tells a remote party's home instance that actor withdrew a request,
// declined theirs, unfriended or blocked them - one message, the receiver's own state says
// which it was.
func (s *Service) relayRemoved(ctx context.Context, actorID int64, other *auth.User) {
	if s.federator == nil || other == nil || !other.IsRemote() {
		return
	}
	actor, err := s.user.GetByID(ctx, actorID)
	if err != nil || actor == nil {
		return
	}
	s.federator.AfterRelationshipRemoved(ctx, actor, other)
}

// ---- gateway events ---------------------------------------------------------------------
//
// A shadow row has no sessions on this instance, so events for it are skipped; its home
// instance delivers the equivalent to the real user.

// publishRequest shows `to` the new incoming request from `from`.
func (s *Service) publishRequest(ctx context.Context, from, to *auth.User) {
	if to.IsRemote() {
		return
	}
	payload := map[string]interface{}{
		"from_user_id": id.Format(from.ID),
		"to_user_id":   id.Format(to.ID),
		"created_at":   time.Now().UTC(),
		"from":         partialUser(from),
	}
	stargate.PublishToUser(ctx, s.redis, to.ID, "RELATIONSHIP_REQUEST", payload, s.cfg.Stargate.Region)
}

// publishAdd sends `to` a RELATIONSHIP_ADD about `about`.
func (s *Service) publishAdd(ctx context.Context, to, about *auth.User, relType int) {
	if to.IsRemote() {
		return
	}
	payload := map[string]interface{}{
		"id":   id.Format(about.ID),
		"type": relType,
		"user": partialUser(about),
	}
	stargate.PublishToUser(ctx, s.redis, to.ID, "RELATIONSHIP_ADD", payload, s.cfg.Stargate.Region)
}

// publishRemoveTo sends toID a RELATIONSHIP_REMOVE about aboutID. `to` is the row for toID
// when the caller has it; nil means "unknown", which is delivered rather than dropped.
func (s *Service) publishRemoveTo(ctx context.Context, toID int64, to *auth.User, aboutID int64) {
	if to != nil && to.IsRemote() {
		return
	}
	stargate.PublishToUser(ctx, s.redis, toID, "RELATIONSHIP_REMOVE", map[string]interface{}{"id": id.Format(aboutID)}, s.cfg.Stargate.Region)
}

// ---- inbound from other instances -------------------------------------------------------
//
// actor is the shadow row of a user on another instance and target one of ours. The peer's
// own write triggered the call, so nothing here relays back - except the accept a crossed
// request produces, which the peer has not seen yet.

// ApplyRemoteRequest records a friend request a remote user sent one of our users.
func (s *Service) ApplyRemoteRequest(ctx context.Context, actor, target *auth.User) error {
	if target.Bot || target.System {
		return ErrBotTarget
	}
	// A blocked sender gets nothing back, not even a refusal; a duplicate is a no-op.
	if s.blockedBetween(target, actor) || isFriend(target, actor.ID) {
		return nil
	}
	if exists, err := s.repo.HasRequest(ctx, actor.ID, target.ID); err != nil || exists {
		return err
	}
	if exists, _ := s.repo.HasRequest(ctx, target.ID, actor.ID); exists {
		// Ours had already asked them: their request is an acceptance. Their instance
		// still holds an unanswered outgoing request, so it is told ours accepted.
		if err := s.befriend(ctx, actor, target); err != nil {
			return err
		}
		if s.federator != nil {
			s.federator.AfterRelationshipAccepted(ctx, target, actor)
		}
		return nil
	}
	if err := s.repo.CreateRequest(ctx, actor.ID, target.ID); err != nil {
		return err
	}
	s.publishRequest(ctx, actor, target)
	return nil
}

// ApplyRemoteAccept records that a remote user accepted the request one of ours sent them.
func (s *Service) ApplyRemoteAccept(ctx context.Context, actor, target *auth.User) error {
	exists, err := s.repo.HasRequest(ctx, target.ID, actor.ID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrRequestNotFound
	}
	return s.befriend(ctx, actor, target)
}

// ApplyRemoteRemove records that a remote user withdrew their request to one of ours,
// declined ours, or ended the friendship. Idempotent: whatever still stands is torn down.
func (s *Service) ApplyRemoteRemove(ctx context.Context, actor, target *auth.User) error {
	if isFriend(target, actor.ID) {
		return s.unfriend(ctx, target, actor.ID, actor)
	}
	changed := false
	for _, pair := range [][2]int64{{actor.ID, target.ID}, {target.ID, actor.ID}} {
		if exists, _ := s.repo.HasRequest(ctx, pair[0], pair[1]); exists {
			if err := s.repo.DeleteRequest(ctx, pair[0], pair[1]); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		s.publishRemoveTo(ctx, target.ID, target, actor.ID)
	}
	return nil
}

// ---- blocks -----------------------------------------------------------------------------

// blockedBetween reports whether either user has blocked the other.
func (s *Service) blockedBetween(a, b *auth.User) bool {
	if a == nil || b == nil {
		return false
	}
	for _, uid := range a.Blocks {
		if uid == b.ID {
			return true
		}
	}
	for _, uid := range b.Blocks {
		if uid == a.ID {
			return true
		}
	}
	return false
}

// Block adds targetID to the actor's block set, first tearing down any friendship or pending
// request between the two. Blocks are one-directional and the target is not notified; only
// the blocker's own devices receive the new blocked relationship. A remote target's home
// instance hears about the teardown (so their side stops showing the tie), never the block.
func (s *Service) Block(ctx context.Context, actorID, targetID int64) error {
	if actorID == targetID {
		return ErrSelfRequest
	}
	actor, err := s.user.GetByID(ctx, actorID)
	if err != nil || actor == nil {
		return ErrUserNotFound
	}
	target, err := s.user.GetByID(ctx, targetID)
	if err != nil || target == nil {
		return ErrUserNotFound
	}
	hadTie := false
	if isFriend(actor, targetID) {
		if err := s.unfriend(ctx, actor, targetID, target); err != nil {
			return err
		}
		hadTie = true
	}
	for _, pair := range [][2]int64{{actorID, targetID}, {targetID, actorID}} {
		if exists, _ := s.repo.HasRequest(ctx, pair[0], pair[1]); exists {
			_ = s.repo.DeleteRequest(ctx, pair[0], pair[1])
			hadTie = true
		}
	}
	if err := s.user.UpdateBlocks(ctx, actorID, []int64{targetID}, nil); err != nil {
		return err
	}
	s.publishAdd(ctx, actor, target, TypeBlocked)
	if hadTie {
		s.relayRemoved(ctx, actorID, target)
	}
	return nil
}

// Unblock removes targetID from the actor's block set.
func (s *Service) Unblock(ctx context.Context, actorID, targetID int64) error {
	if err := s.user.UpdateBlocks(ctx, actorID, nil, []int64{targetID}); err != nil {
		return err
	}
	stargate.PublishToUser(ctx, s.redis, actorID, "RELATIONSHIP_REMOVE", map[string]interface{}{"id": id.Format(targetID)}, s.cfg.Stargate.Region)
	return nil
}

// partialUser builds a partial user object. Presence uses status/custom_status only (never online).
func partialUser(u *auth.User) map[string]interface{} {
	if u == nil {
		return nil
	}
	m := map[string]interface{}{
		"id":           id.Format(u.ID),
		"username":     u.Username,
		"display_name": u.DisplayName,
		"avatar":       u.Avatar,
		"banner":       u.Banner,
		"bio":          u.Bio,
		"about_me":     u.AboutMe,
	}
	auth.MergeProfilePublicExtras(m, u)
	if u.HomeDomain != "" {
		m["home_domain"] = u.HomeDomain
	}
	pub := auth.ToPublicPresence(u.Presence, true)
	m["presence"] = pub
	return m
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

// ListRelationships returns all relationships, batch-fetching the user rows. Built fresh
// every time: the list embeds each person's presence, and a cached copy went stale for
// minutes whenever a read raced a write (the read re-populated the cache after the write
// had invalidated it). Four reads per call is cheap enough not to need one.
func (s *Service) ListRelationships(ctx context.Context, actorID int64) ([]Relationship, error) {
	u, err := s.user.GetByID(ctx, actorID)
	if err != nil || u == nil {
		return nil, ErrUserNotFound
	}

	var ids []int64
	ids = append(ids, u.Relationships...)
	ids = append(ids, u.Blocks...)

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
	for _, blockedID := range u.Blocks {
		if r := buildRelationshipFromUser(byID[blockedID], blockedID, TypeBlocked, nil, nil, false); r != nil {
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

// Patch updates relationship metadata (nickname).
func (s *Service) Patch(ctx context.Context, actorID, targetID int64, nickname *string) error {
	u, err := s.user.GetByID(ctx, actorID)
	if err != nil || u == nil {
		return ErrUserNotFound
	}
	if !isFriend(u, targetID) {
		return ErrRequestNotFound
	}
	if nickname == nil {
		return nil // no change requested
	}
	if *nickname == "" {
		return s.repo.DeleteNickname(ctx, actorID, targetID)
	}
	return s.repo.SetNickname(ctx, actorID, targetID, *nickname)
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
			continue
		}
		if from, _ := s.user.GetByID(ctx, req.FromUserID); from != nil && from.IsRemote() {
			s.relayRemoved(ctx, actorID, from)
		}
	}
	return lastErr
}
