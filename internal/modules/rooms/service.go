package rooms

import (
	"context"
	"errors"
	"sort"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	ErrRoomNotFound   = errors.New("room not found")
	ErrNotParticipant = errors.New("not a participant of this room")
)

type Service struct {
	repo  Repository
	user  auth.UserRepository
	redis *redis.Client
	cfg   *config.Config
}

func NewService(repo Repository, user auth.UserRepository, redis *redis.Client, cfg *config.Config) *Service {
	return &Service{repo: repo, user: user, redis: redis, cfg: cfg}
}

// CreatePM finds or creates a 1:1 PM room between actor and target.
// Returns (room, true) when created, (room, false) when existing.
func (s *Service) CreatePM(ctx context.Context, actorID, targetID int64) (*RoomWithParticipants, bool, error) {
	if actorID == targetID {
		return nil, false, errors.New("cannot create PM with yourself")
	}
	existing, err := s.repo.GetPMRoom(ctx, actorID, targetID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		ids, _ := s.repo.GetParticipants(ctx, existing.ID)
		return &RoomWithParticipants{Room: *existing, ParticipantIDs: ids}, false, nil
	}
	roomID := id.Next()
	room := &Room{
		ID:    roomID,
		Type:  TypePM,
		SpaceID: nil,
		ParentID: nil,
		Name:  "",
		Topic: "",
	}
	if err := s.repo.Create(ctx, room, []int64{actorID, targetID}); err != nil {
		return nil, false, err
	}
	// Real-time: notify target user of new PM (Redis Pub/Sub -> WebSocket)
	if s.redis != nil && s.cfg != nil {
		region := s.cfg.Stargate.Region
		if region == "" {
			region = "default"
		}
		payload := map[string]interface{}{
			"id":         id.Format(room.ID),
			"type":       TypePM,
			"recipients": []string{id.Format(actorID), id.Format(targetID)},
			"created_at": room.CreatedAt,
		}
		stargate.PublishToUser(ctx, s.redis, targetID, "ROOM_CREATE", payload, region)
	}
	return &RoomWithParticipants{
		Room:           *room,
		ParticipantIDs: []int64{actorID, targetID},
	}, true, nil
}

// ListRooms returns rooms the user participates in (PMs first, ordered by recency).
func (s *Service) ListRooms(ctx context.Context, userID int64) ([]RoomWithParticipants, error) {
	rows, err := s.repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []RoomWithParticipants
	for _, row := range rows {
		room, err := s.repo.GetByID(ctx, row.RoomID)
		if err != nil || room == nil {
			continue
		}
		ids, _ := s.repo.GetParticipants(ctx, row.RoomID)
		rwp := RoomWithParticipants{Room: *room, ParticipantIDs: ids}
		if row.LastMessageID != nil {
			rwp.LastMessageID = row.LastMessageID
		}
		out = append(out, rwp)
	}
	sort.Slice(out, func(i, j int) bool {
		ida, idb := int64(0), int64(0)
		if out[i].LastMessageID != nil {
			ida = *out[i].LastMessageID
		}
		if out[j].LastMessageID != nil {
			idb = *out[j].LastMessageID
		}
		return ida > idb
	})
	return out, nil
}

// GetRoom returns a room by ID if the user is a participant.
func (s *Service) GetRoom(ctx context.Context, userID, roomID int64) (*RoomWithParticipants, error) {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return nil, ErrRoomNotFound
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, pid := range ids {
		if pid == userID {
			ok = true
			break
		}
	}
	if !ok {
		return nil, ErrNotParticipant
	}
	return &RoomWithParticipants{Room: *room, ParticipantIDs: ids}, nil
}
