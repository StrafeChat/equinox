package routes

import (
	"context"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// newSystemMessenger builds the closure that stores a server-generated message ("X added
// Y", "Y joined the space") in a room, bumps the room's last-message pointer for every
// recipient, fans it out over the gateway and, when federated, to peer instances. The
// rooms and spaces services share it so both kinds of notice look identical.
func newSystemMessenger(d Deps, roomRepo rooms.Repository, msgRepo messages.Repository) rooms.OnRoomSystemEvent {
	region := "default"
	if d.Config != nil && d.Config.Stargate.Region != "" {
		region = d.Config.Stargate.Region
	}
	return func(ctx context.Context, roomID int64, participantIDs []int64, eventType, payload string) (int64, error) {
		msg := &messages.Message{
			RoomID:        roomID,
			ID:            id.Next(),
			SenderID:      0,
			SystemType:    eventType,
			SystemPayload: payload,
		}
		if err := msgRepo.Create(ctx, msg); err != nil {
			return 0, err
		}
		if err := roomRepo.UpdateLastMessageID(ctx, roomID, participantIDs, msg.ID); err != nil {
			// non-fatal
		}
		if d.Redis != nil {
			pl := map[string]interface{}{
				"room_id":        id.Format(roomID),
				"id":             id.Format(msg.ID),
				"sender_id":      "0",
				"system_type":    msg.SystemType,
				"system_payload": msg.SystemPayload,
				"created_at":     msg.CreatedAt,
				"updated_at":     msg.UpdatedAt,
			}
			stargate.PublishToSpace(ctx, d.Redis, roomID, "MESSAGE_CREATE", pl, region)
		}
		if d.Federation != nil {
			// "X added Y" and friends need to show up on every instance in the room.
			d.Federation.AfterMessageCreated(ctx, roomID, participantIDs, msg)
		}
		return msg.ID, nil
	}
}
