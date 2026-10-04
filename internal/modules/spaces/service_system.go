package spaces

import (
	"context"
	"encoding/json"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
)

// SystemMessenger posts a server-generated message into a room and returns its id. The
// spaces service doesn't own message storage; routes wire in the same closure the rooms
// service uses for "X added Y" notices.
type SystemMessenger func(ctx context.Context, roomID int64, recipientIDs []int64, eventType, payload string) (int64, error)

// System message types posted to a space's system room (Space.SystemRoomID).
const (
	SystemMemberJoin  = "space_member_join"
	SystemMemberLeave = "space_member_leave"
	// SystemBirthday is posted in a space's birthday channel for an opted-in member on their
	// birthday. Payload: {user_id, message} (message is the space's custom template, or "").
	SystemBirthday = "space_birthday"
)

// SetSystemMessenger enables join/leave notices in system rooms.
func (s *Service) SetSystemMessenger(fn SystemMessenger) {
	s.systemMessenger = fn
}

// postSystemMessage writes a notice into the space's system room unless the space has no
// system room, the notice type is suppressed by SystemRoomFlags, or the room no longer
// belongs to the space (deleted rooms are not scrubbed from the setting).
func (s *Service) postSystemMessage(ctx context.Context, sp *Space, suppressFlag int, eventType string, payload map[string]interface{}) {
	if s.systemMessenger == nil || sp == nil || sp.SystemRoomID == nil || sp.SystemRoomFlags&suppressFlag != 0 {
		return
	}
	room, err := s.roomRepo.GetByID(ctx, *sp.SystemRoomID)
	if err != nil || room == nil || room.SpaceID == nil || *room.SpaceID != sp.ID {
		return
	}
	memberIDs, err := s.ListSpaceMemberUserIDs(ctx, sp.ID)
	if err != nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := s.systemMessenger(ctx, room.ID, memberIDs, eventType, string(raw)); err != nil {
		logger.Err("spaces", err, map[string]any{"space_id": sp.ID, "system_type": eventType})
	}
}

// announceLeave posts the "left / was kicked / was banned" notice for a former member.
func (s *Service) announceLeave(ctx context.Context, spaceID, userID int64, how string) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return
	}
	s.postSystemMessage(ctx, sp, SystemRoomFlagSuppressLeave, SystemMemberLeave, map[string]interface{}{
		"user_id": id.Format(userID),
		"how":     how,
	})
}
