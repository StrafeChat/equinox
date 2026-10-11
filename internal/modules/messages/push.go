package messages

import (
	"context"

	"github.com/StrafeChat/equinox/internal/modules/push"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// pushMessage queues push notifications for a new message (push.EnqueueMessage decides
// who has a device to notify; the delivery workers decide who wants it). mentioned is
// everyone the message mentions directly or through a role, everyone an @everyone/@here
// that was allowed to notify.
func (s *Service) pushMessage(ctx context.Context, room *rooms.Room, m *Message, participants, mentioned []int64, everyone bool, attachments int) {
	if s.redis == nil || m.SystemType != "" {
		return
	}
	e2ee := !roomE2EEOff(room)
	ev := push.Message{
		RoomID: room.ID, RoomType: room.Type, E2EE: e2ee,
		ID: m.ID, SenderID: m.SenderID, Attachments: attachments,
		Mentioned: mentioned, Everyone: everyone, CreatedAt: m.CreatedAt,
	}
	if room.SpaceID != nil {
		ev.SpaceID = *room.SpaceID
	}
	if !e2ee {
		ev.Text = m.Plaintext
	}
	push.EnqueueMessage(ctx, s.redis, ev, participants)
}

// pushFederated is pushMessage for a message relayed from another instance: the room is
// only loaded once it is known that someone here has a device to notify, a thread narrows
// the space's member list to its members (as a local send does), and role mentions are
// expanded here, since the relay carries the role ids, not their holders.
func (s *Service) pushFederated(ctx context.Context, roomID int64, participants []int64, m *Message) {
	if s.redis == nil || m.SenderID == 0 || m.SystemType != "" {
		return
	}
	// A local member's own message in a mirrored channel comes back through here (the
	// origin applied it): it reads the room for them, as a local send does.
	if push.NotificationShowing(ctx, s.redis, m.SenderID, roomID) {
		if room, err := s.rooms.GetByID(ctx, roomID); err == nil && room != nil {
			s.pushRead(ctx, m.SenderID, room, m.ID)
		}
	}
	if len(push.Subscribed(ctx, s.redis, participants, m.SenderID)) == 0 {
		return
	}
	room, err := s.rooms.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return
	}
	if room.Type == rooms.TypeThread {
		members, err := s.rooms.ListThreadMemberIDs(ctx, roomID)
		if err != nil {
			return
		}
		participants = members
	}
	mentioned := m.Mentions
	if len(m.MentionRoles) > 0 && room.SpaceID != nil && s.spaceAuth != nil {
		// Only mentionable roles: whether the sender could also ping the others was the
		// origin's call, and the relay does not say.
		if holders, err := s.spaceAuth.ListSpaceMemberUserIDsByRoles(ctx, *room.SpaceID, m.MentionRoles, false); err == nil {
			mentioned = append(append([]int64{}, mentioned...), holders...)
		}
	}
	s.pushMessage(ctx, room, m, participants, mentioned, m.MentionEveryone, len(m.Attachments()))
}

// pushRead clears the room's notifications on the sender's other devices: writing in a
// room reads it (see the read-cursor advance in create).
func (s *Service) pushRead(ctx context.Context, userID int64, room *rooms.Room, msgID int64) {
	if s.redis == nil {
		return
	}
	var spaceID int64
	if room.SpaceID != nil {
		spaceID = *room.SpaceID
	}
	push.EnqueueRead(ctx, s.redis, userID, room.ID, room.Type, spaceID, msgID)
}
