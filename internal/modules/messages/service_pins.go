package messages

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// Pinned messages, Discord's model. A room keeps up to MaxPinsPerRoom pinned messages that
// everyone in it sees, listed newest pin first. In a space channel pinning needs Manage
// Messages; in a PM or group anyone in it may pin. A pin posts a "X pinned a message" system
// notice (Discord's CHANNEL_PINNED_MESSAGE); every change reaches the room's clients as
// ROOM_PINS_UPDATE - Discord's CHANNEL_PINS_UPDATE plus which message and whether it was
// pinned or unpinned, so a client can patch its copy without refetching. A deleted message
// loses its pin.

// MaxPinsPerRoom is Discord's per-channel cap.
const MaxPinsPerRoom = 50

// SystemMessagePinned is the system_type of the notice posted when a message is pinned; its
// payload is {"actor_id", "message_id"}.
const SystemMessagePinned = "message_pinned"

var ErrTooManyPins = errors.New("this room already has the maximum of 50 pinned messages")

// PinnedMessage is one entry of a room's pin list.
type PinnedMessage struct {
	Pin
	Message   Message
	Reactions []ReactionSummary
}

// Pin pins a message for everyone in the room and returns when it was pinned. Pinning an
// already pinned message is a no-op, like Discord's PUT.
func (s *Service) Pin(ctx context.Context, userID, roomID, msgID int64) (time.Time, error) {
	room, participants, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory)
	if err != nil {
		return time.Time{}, err
	}
	if origin := s.remoteOrigin(ctx, room); origin != "" {
		return time.Time{}, s.federator.PinRemote(ctx, origin, room, userID, msgID, false)
	}
	if err := s.checkPinPerms(ctx, userID, roomID, room); err != nil {
		return time.Time{}, err
	}
	msg, err := s.getLiveMessage(ctx, roomID, msgID)
	if err != nil {
		return time.Time{}, err
	}
	if msg.PinnedAt != nil && !msg.PinnedAt.IsZero() {
		return *msg.PinnedAt, nil
	}
	n, err := s.repo.CountPins(ctx, roomID)
	if err != nil {
		return time.Time{}, err
	}
	if n >= MaxPinsPerRoom {
		return time.Time{}, ErrTooManyPins
	}
	// Millisecond precision: that is what Scylla stores, and the room_pins key has to match
	// exactly when the pin is removed again.
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.repo.Pin(ctx, roomID, msgID, userID, now); err != nil {
		return time.Time{}, err
	}
	s.publishPinsUpdate(ctx, roomID, msgID, userID, &now, true)
	s.postPinNotice(ctx, room, participants, userID, msgID)
	if s.federator != nil {
		s.federator.AfterMessagePinned(ctx, roomID, participants, msgID, userID, now, false)
	}
	return now, nil
}

// Unpin removes a pin for everyone in the room. Unpinning a message that is not pinned is a
// no-op.
func (s *Service) Unpin(ctx context.Context, userID, roomID, msgID int64) error {
	room, participants, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory)
	if err != nil {
		return err
	}
	if origin := s.remoteOrigin(ctx, room); origin != "" {
		return s.federator.PinRemote(ctx, origin, room, userID, msgID, true)
	}
	if err := s.checkPinPerms(ctx, userID, roomID, room); err != nil {
		return err
	}
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil {
		return err
	}
	if msg == nil || msg.PinnedAt == nil || msg.PinnedAt.IsZero() {
		return nil
	}
	at := *msg.PinnedAt
	if err := s.repo.Unpin(ctx, roomID, msgID, at); err != nil {
		return err
	}
	s.publishPinsUpdate(ctx, roomID, msgID, userID, nil, false)
	if s.federator != nil {
		s.federator.AfterMessagePinned(ctx, roomID, participants, msgID, userID, at, true)
	}
	return nil
}

// ListPins returns the room's pinned messages, newest pin first, as userID sees them.
func (s *Service) ListPins(ctx context.Context, userID, roomID int64) ([]PinnedMessage, error) {
	room, _, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory)
	if err != nil {
		return nil, err
	}
	// A channel hosted elsewhere: this instance only holds what was relayed to it, so the
	// list comes from the origin, as history does.
	if origin := s.remoteOrigin(ctx, room); origin != "" {
		return s.federator.ListPinsRemote(ctx, origin, room, userID)
	}
	return s.listPinsLocal(ctx, userID, roomID)
}

func (s *Service) listPinsLocal(ctx context.Context, userID, roomID int64) ([]PinnedMessage, error) {
	pins, err := s.repo.ListPins(ctx, roomID, MaxPinsPerRoom)
	if err != nil || len(pins) == 0 {
		return nil, err
	}
	ids := make([]int64, 0, len(pins))
	for _, p := range pins {
		ids = append(ids, p.MessageID)
	}
	msgs, err := s.repo.GetByIDs(ctx, roomID, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]Message, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}
	reactions := s.reactionsFor(ctx, roomID, msgs, userID)
	out := make([]PinnedMessage, 0, len(pins))
	for _, p := range pins {
		m, ok := byID[p.MessageID]
		if !ok {
			continue // deleted since; the delete path drops the pin, this is just the race
		}
		out = append(out, PinnedMessage{Pin: p, Message: m, Reactions: reactions[m.ID]})
	}
	return out, nil
}

// PinFederated applies a pin or unpin relayed by another instance - the channel's origin, or
// a PM participant's instance. The permission was checked where the action happened, and the
// "pinned a message" notice arrives as its own relayed system message.
func (s *Service) PinFederated(ctx context.Context, roomID, msgID, userID int64, at time.Time, remove bool) error {
	at = at.UTC().Truncate(time.Millisecond)
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil {
		return err
	}
	if remove {
		if msg != nil && msg.PinnedAt != nil && !msg.PinnedAt.IsZero() {
			if err := s.repo.Unpin(ctx, roomID, msgID, *msg.PinnedAt); err != nil {
				return err
			}
		}
		s.publishPinsUpdate(ctx, roomID, msgID, userID, nil, false)
		return nil
	}
	// A mirror may not hold the message at all (it stores only what was relayed since one of
	// its members joined); its clients still learn of the pin, and read the list from the
	// origin anyway.
	if msg != nil && (msg.PinnedAt == nil || msg.PinnedAt.IsZero()) {
		if err := s.repo.Pin(ctx, roomID, msgID, userID, at); err != nil {
			return err
		}
	}
	s.publishPinsUpdate(ctx, roomID, msgID, userID, &at, true)
	return nil
}

// checkPinPerms: Manage Messages in a space channel; anyone in a PM or group.
func (s *Service) checkPinPerms(ctx context.Context, userID, roomID int64, room *rooms.Room) error {
	if room.SpaceID == nil {
		return nil
	}
	return s.checkChannelPerms(ctx, userID, roomID, room, permissions.PermManageMessages)
}

// dropPinOnDelete removes a deleted message's pin (Discord drops it too) and tells clients,
// since MESSAGE_DELETE alone would leave their pin lists stale.
func (s *Service) dropPinOnDelete(ctx context.Context, roomID int64, msg *Message, actorID int64) {
	if msg == nil || msg.PinnedAt == nil || msg.PinnedAt.IsZero() {
		return
	}
	_ = s.repo.Unpin(ctx, roomID, msg.ID, *msg.PinnedAt)
	s.publishPinsUpdate(ctx, roomID, msg.ID, actorID, nil, false)
}

// publishPinsUpdate is ROOM_PINS_UPDATE: Discord's CHANNEL_PINS_UPDATE (room + the time of the
// newest remaining pin) plus the message concerned and whether it is now pinned.
func (s *Service) publishPinsUpdate(ctx context.Context, roomID, msgID, userID int64, at *time.Time, pinned bool) {
	if s.redis == nil || s.cfg == nil {
		return
	}
	payload := map[string]interface{}{
		"room_id":    id.Format(roomID),
		"message_id": id.Format(msgID),
		"user_id":    id.Format(userID),
		"pinned":     pinned,
	}
	if at != nil {
		payload["pinned_at"] = at.UTC()
	}
	if newest, err := s.repo.ListPins(ctx, roomID, 1); err == nil {
		if len(newest) > 0 {
			payload["last_pin_timestamp"] = newest[0].PinnedAt.UTC()
		} else {
			payload["last_pin_timestamp"] = nil
		}
	}
	stargate.PublishToSpace(ctx, s.redis, roomID, "ROOM_PINS_UPDATE", payload, s.cfg.Stargate.Region)
}

// postPinNotice stores and announces the "X pinned a message" system message, the way Discord
// posts CHANNEL_PINNED_MESSAGE: a real message in the room - it marks the room unread and is
// relayed to peers like any other - with no sender and a payload naming who and what.
func (s *Service) postPinNotice(ctx context.Context, room *rooms.Room, participants []int64, actorID, msgID int64) {
	payload, _ := json.Marshal(map[string]string{"actor_id": id.Format(actorID), "message_id": id.Format(msgID)})
	notice := &Message{RoomID: room.ID, ID: id.Next(), SystemType: SystemMessagePinned, SystemPayload: string(payload)}
	if err := s.repo.Create(ctx, notice); err != nil {
		return
	}
	fanout := s.spaceParticipants(ctx, room, participants)
	_ = s.rooms.UpdateLastMessageID(ctx, room.ID, fanout, notice.ID)
	if s.redis != nil && s.cfg != nil {
		stargate.PublishToSpace(ctx, s.redis, room.ID, "MESSAGE_CREATE", messageEventPayload(notice), s.cfg.Stargate.Region)
	}
	if s.federator != nil {
		s.federator.AfterMessageCreated(ctx, room.ID, participants, notice)
	}
}
