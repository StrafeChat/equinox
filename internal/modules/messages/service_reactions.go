package messages

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// getLiveMessage fetches a message and rejects it if it's missing or soft-deleted -
// reacting to (or reading reactions on) a deleted message makes no sense.
func (s *Service) getLiveMessage(ctx context.Context, roomID, msgID int64) (*Message, error) {
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil {
		return nil, err
	}
	if msg == nil || (msg.DeletedAt != nil && !msg.DeletedAt.IsZero()) {
		return nil, ErrMessageNotFound
	}
	return msg, nil
}

// addReactionRow records userID's reaction unless it is already there, enforcing the
// distinct-emoji cap. Returns the rows as they stood before the write and whether a row
// was written.
func (s *Service) addReactionRow(ctx context.Context, roomID, msgID, userID int64, emoji string) ([]Reaction, bool, error) {
	existing, err := s.repo.ListReactions(ctx, roomID, msgID)
	if err != nil {
		return nil, false, err
	}
	distinct := make(map[string]bool, len(existing))
	for _, r := range existing {
		distinct[r.Emoji] = true
		if r.Emoji == emoji && r.UserID == userID {
			return existing, false, nil
		}
	}
	if !distinct[emoji] && len(distinct) >= MaxReactionsPerMessage {
		return nil, false, ErrTooManyReactions
	}
	if err := s.repo.AddReaction(ctx, roomID, msgID, userID, emoji); err != nil {
		return nil, false, err
	}
	return existing, true, nil
}

func (s *Service) publishReaction(ctx context.Context, event string, roomID, msgID, userID int64, emoji string) {
	if s.redis == nil || s.cfg == nil {
		return
	}
	stargate.PublishToSpace(ctx, s.redis, roomID, event, map[string]interface{}{
		"room_id":    id.Format(roomID),
		"message_id": id.Format(msgID),
		"user_id":    id.Format(userID),
		"emoji":      emoji,
	}, s.region())
}

// AddReaction adds userID's reaction to a message and returns the message's full,
// up-to-date reaction summary. Idempotent: reacting twice with the same emoji is a no-op
// that just returns the current state, matching Discord's own PUT semantics.
func (s *Service) AddReaction(ctx context.Context, userID, roomID, msgID int64, rawEmoji string) ([]ReactionSummary, error) {
	emoji, err := ValidateReactionEmoji(rawEmoji)
	if err != nil {
		return nil, err
	}
	room, participants, err := s.authorize(ctx, userID, roomID, permissions.PermAddReactions)
	if err != nil {
		return nil, err
	}
	if _, err := s.getLiveMessage(ctx, roomID, msgID); err != nil {
		return nil, err
	}
	existing, added, err := s.addReactionRow(ctx, roomID, msgID, userID, emoji)
	if err != nil {
		return nil, err
	}
	if !added {
		return summarize(existing, userID), nil
	}
	s.publishReaction(ctx, "MESSAGE_REACTION_ADD", roomID, msgID, userID, emoji)
	if s.federator != nil && room.SpaceID == nil {
		s.federator.AfterReactionAdded(ctx, roomID, participants, msgID, userID, emoji)
	}
	existing = append(existing, Reaction{RoomID: roomID, MessageID: msgID, Emoji: emoji, UserID: userID, CreatedAt: time.Now().UTC()})
	return summarize(existing, userID), nil
}

// RemoveReaction removes userID's own reaction (a no-op if they hadn't reacted with that
// emoji) and returns the message's updated reaction summary.
func (s *Service) RemoveReaction(ctx context.Context, userID, roomID, msgID int64, rawEmoji string) ([]ReactionSummary, error) {
	emoji, err := ValidateReactionEmoji(rawEmoji)
	if err != nil {
		return nil, err
	}
	room, participants, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom)
	if err != nil {
		return nil, err
	}
	if _, err := s.getLiveMessage(ctx, roomID, msgID); err != nil {
		return nil, err
	}
	if err := s.repo.RemoveReaction(ctx, roomID, msgID, userID, emoji); err != nil {
		return nil, err
	}
	s.publishReaction(ctx, "MESSAGE_REACTION_REMOVE", roomID, msgID, userID, emoji)
	if s.federator != nil && room.SpaceID == nil {
		s.federator.AfterReactionRemoved(ctx, roomID, participants, msgID, userID, emoji)
	}
	remaining, err := s.repo.ListReactions(ctx, roomID, msgID)
	if err != nil {
		return nil, err
	}
	return summarize(remaining, userID), nil
}

// AddReactionFederated applies a reaction relayed by another instance. The caller has
// already checked the reactor is a participant, and a PM or group has no channel
// permissions to consult; the cap, idempotency and gateway event match a local reaction.
// Nothing relays back.
func (s *Service) AddReactionFederated(ctx context.Context, roomID, msgID, userID int64, rawEmoji string) error {
	emoji, err := ValidateReactionEmoji(rawEmoji)
	if err != nil {
		return err
	}
	if _, err := s.getLiveMessage(ctx, roomID, msgID); err != nil {
		return err
	}
	_, added, err := s.addReactionRow(ctx, roomID, msgID, userID, emoji)
	if err != nil {
		return err
	}
	if added {
		s.publishReaction(ctx, "MESSAGE_REACTION_ADD", roomID, msgID, userID, emoji)
	}
	return nil
}

// RemoveReactionFederated applies a reaction withdrawal relayed by another instance.
func (s *Service) RemoveReactionFederated(ctx context.Context, roomID, msgID, userID int64, rawEmoji string) error {
	emoji, err := ValidateReactionEmoji(rawEmoji)
	if err != nil {
		return err
	}
	if _, err := s.getLiveMessage(ctx, roomID, msgID); err != nil {
		return err
	}
	if err := s.repo.RemoveReaction(ctx, roomID, msgID, userID, emoji); err != nil {
		return err
	}
	s.publishReaction(ctx, "MESSAGE_REACTION_REMOVE", roomID, msgID, userID, emoji)
	return nil
}

// Reactions returns one message's reaction summary as userID would see it (its own "me"
// flags). Used by GET .../messages/:msg_id.
func (s *Service) Reactions(ctx context.Context, userID, roomID, msgID int64) ([]ReactionSummary, error) {
	rows, err := s.repo.ListReactions(ctx, roomID, msgID)
	if err != nil {
		return nil, err
	}
	return summarize(rows, userID), nil
}

// ReactionsForMessages batches Reactions across a whole page of history (GET .../messages).
func (s *Service) ReactionsForMessages(ctx context.Context, roomID int64, messageIDs []int64, viewerID int64) (map[int64][]ReactionSummary, error) {
	byMsg, err := s.repo.ListReactionsForMessages(ctx, roomID, messageIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]ReactionSummary, len(byMsg))
	for mid, rows := range byMsg {
		out[mid] = summarize(rows, viewerID)
	}
	return out, nil
}

// ListReactors returns the profiles of everyone who reacted to a message with a specific
// emoji - the "N, M and 3 others reacted with 👍" tooltip.
func (s *Service) ListReactors(ctx context.Context, userID, roomID, msgID int64, rawEmoji string) ([]*auth.User, error) {
	emoji, err := ValidateReactionEmoji(rawEmoji)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory); err != nil {
		return nil, err
	}
	rows, err := s.repo.ListReactions(ctx, roomID, msgID)
	if err != nil {
		return nil, err
	}
	var reactorIDs []int64
	for _, r := range rows {
		if r.Emoji == emoji {
			reactorIDs = append(reactorIDs, r.UserID)
		}
	}
	if len(reactorIDs) == 0 || s.userRepo == nil {
		return nil, nil
	}
	return s.userRepo.GetByIDs(ctx, reactorIDs)
}
