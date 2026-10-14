package messages

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/safego"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// roleMentionUpdateTimeout bounds the background pass that tells role holders their new
// mention badge; a role can be held by most of a large space.
const roleMentionUpdateTimeout = 30 * time.Second

// roleMentionRecipients is who should get a live badge update for a new message's role
// mentions: everyone in the notify set (sender already excluded, deduped) who holds a
// mentioned role. A direct mention or @everyone the client sees for itself in
// MESSAGE_CREATE; a role it can only judge with that space's member list loaded.
func roleMentionRecipients(notify, fromRoles []int64) []int64 {
	if len(notify) == 0 || len(fromRoles) == 0 {
		return nil
	}
	held := make(map[int64]struct{}, len(fromRoles))
	for _, uid := range fromRoles {
		held[uid] = struct{}{}
	}
	var out []int64
	for _, uid := range notify {
		if _, ok := held[uid]; ok {
			out = append(out, uid)
		}
	}
	return out
}

// publishRoleMentionCounts sends MESSAGE_MENTION_UPDATE to each user a new message
// mentioned through a role, with their badge for the room as it stands after the
// increment. Call it after MESSAGE_CREATE is published, so the absolute count lands after
// any local +1 the client makes for the same message. It runs off the request: each
// holder costs a permission check and two reads.
func (s *Service) publishRoleMentionCounts(room *rooms.Room, msgID int64, userIDs []int64) {
	if s.redis == nil || s.cfg == nil || len(userIDs) == 0 {
		return
	}
	region := s.region()
	safego.Go("messages", func() {
		// Not the request's context: Fiber recycles it once the handler returns.
		ctx, cancel := context.WithTimeout(context.Background(), roleMentionUpdateTimeout)
		defer cancel()
		s.eachRoleMentionUpdate(ctx, room, msgID, userIDs, func(uid int64, payload map[string]interface{}) {
			stargate.PublishToUser(ctx, s.redis, uid, "MESSAGE_MENTION_UPDATE", payload, region)
		})
	})
}

// eachRoleMentionUpdate builds each user's MESSAGE_MENTION_UPDATE and hands it to emit
// straight away, so the count read and the publish stay close together for every user
// however long the list is. Only counts go out - never anything of the message, whose
// content in an encrypted room is not the server's to read. Users who cannot view the
// room are skipped: the event names it, and a private room is not theirs to learn of.
// message_id says which message the count is as of, so a client that has already read
// past it can ignore a count that raced its own ack.
func (s *Service) eachRoleMentionUpdate(ctx context.Context, room *rooms.Room, msgID int64, userIDs []int64, emit func(uid int64, payload map[string]interface{})) {
	if room == nil || room.SpaceID == nil || s.spaceAuth == nil {
		return
	}
	for _, uid := range userIDs {
		if ctx.Err() != nil {
			return
		}
		perms, err := s.spaceAuth.EffectiveChannelPermissions(ctx, uid, *room.SpaceID, room.ID)
		if err != nil || !permissions.Has(perms, permissions.PermViewRoom) {
			continue
		}
		// Total before baseline: an ack that lands in between leaves the baseline at or
		// above the total read, which comes out as 0 rather than a stale badge.
		total, err := s.rooms.GetMentionCount(ctx, uid, room.ID)
		if err != nil {
			continue
		}
		row, err := s.rooms.GetRoomRow(ctx, uid, room.ID)
		if err != nil {
			continue
		}
		emit(uid, map[string]interface{}{
			"room_id":       id.Format(room.ID),
			"mention_count": int64(row.DisplayMentionCount(total)),
			"message_id":    id.Format(msgID),
		})
	}
}
