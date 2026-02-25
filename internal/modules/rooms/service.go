package rooms

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

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
		rwp := &RoomWithParticipants{Room: *existing, ParticipantIDs: ids}
		enrichParticipants(ctx, s.user, rwp)
		return rwp, false, nil
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
	rwp := &RoomWithParticipants{
		Room:           *room,
		ParticipantIDs: []int64{actorID, targetID},
	}
	enrichParticipants(ctx, s.user, rwp)
	return rwp, true, nil
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

	// Enrich with participant details for display (PM names, etc.)
	allIDs := make(map[int64]struct{})
	for _, r := range out {
		for _, pid := range r.ParticipantIDs {
			allIDs[pid] = struct{}{}
		}
	}
		ids := make([]int64, 0, len(allIDs))
	for pid := range allIDs {
		ids = append(ids, pid)
	}
	if len(ids) > 0 {
		users, err := s.user.GetByIDs(ctx, ids)
		if err == nil {
			byID := make(map[int64]*auth.User)
			for i := range ids {
				if i < len(users) && users[i] != nil {
					byID[ids[i]] = users[i]
				}
			}
			for i := range out {
				for _, pid := range out[i].ParticipantIDs {
					if u := byID[pid]; u != nil {
						out[i].Participants = append(out[i].Participants, Participant{
							ID:          id.Format(u.ID),
							Username:    u.Username,
							DisplayName: u.DisplayName,
							Avatar:      u.Avatar,
						})
					}
				}
			}
		}
	}
	return out, nil
}

func enrichParticipants(ctx context.Context, user auth.UserRepository, rwp *RoomWithParticipants) {
	if len(rwp.ParticipantIDs) == 0 {
		return
	}
	users, err := user.GetByIDs(ctx, rwp.ParticipantIDs)
	if err != nil {
		return
	}
	for i := range rwp.ParticipantIDs {
		if i < len(users) && users[i] != nil {
			rwp.Participants = append(rwp.Participants, Participant{
				ID:          id.Format(users[i].ID),
				Username:    users[i].Username,
				DisplayName: users[i].DisplayName,
				Avatar:      users[i].Avatar,
			})
		}
	}
}

const typingRateLimitSec = 5

// Typing publishes TYPING_START to the room. No persistence. Rate-limited per user/room.
func (s *Service) Typing(ctx context.Context, userID, roomID int64) error {
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	ok := false
	for _, pid := range ids {
		if pid == userID {
			ok = true
			break
		}
	}
	if !ok {
		return ErrNotParticipant
	}
	if s.redis == nil || s.cfg == nil {
		return nil
	}
	key := "typing:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(roomID, 10)
	// Rate limit: 1 event per 5s per user per room. Silent drop if throttled.
	if set, _ := s.redis.SetNX(ctx, key, "1", typingRateLimitSec*time.Second).Result(); !set {
		return nil
	}
	payload := map[string]interface{}{
		"room_id":   id.Format(roomID),
		"user_id":   id.Format(userID),
		"timestamp": time.Now().Unix(),
	}
	stargate.PublishToSpace(ctx, s.redis, roomID, "TYPING_START", payload, s.cfg.Stargate.Region)
	return nil
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
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: ids}
	users, _ := s.user.GetByIDs(ctx, ids)
	for i := range ids {
		if i < len(users) && users[i] != nil {
			rwp.Participants = append(rwp.Participants, Participant{
				ID:          id.Format(users[i].ID),
				Username:    users[i].Username,
				DisplayName: users[i].DisplayName,
				Avatar:      users[i].Avatar,
			})
		}
	}
	return rwp, nil
}
