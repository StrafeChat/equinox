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
	ErrRoomNotFound    = errors.New("room not found")
	ErrNotParticipant  = errors.New("not a participant of this room")
	ErrNotGroupRoom    = errors.New("not a group room")
	ErrAlreadyInGroup  = errors.New("user already in group")
	ErrMinParticipants = errors.New("group must have at least 2 participants")
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

func (s *Service) stargateRegion() string {
	if s.cfg == nil || s.cfg.Stargate.Region == "" {
		return "default"
	}
	return s.cfg.Stargate.Region
}

// CreatePM finds or creates a 1:1 PM room between actor and target.
// When actorID == targetID, creates a "notes" room (one-person PM for personal notes).
// Returns (room, true) when created, (room, false) when existing.
func (s *Service) CreatePM(ctx context.Context, actorID, targetID int64) (*RoomWithParticipants, bool, error) {
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
		ID:       roomID,
		Type:     TypePM,
		SpaceID:  nil,
		ParentID: nil,
		Name:     "",
		Topic:    "",
	}
	participantIDs := []int64{actorID, targetID}
	if actorID == targetID {
		participantIDs = []int64{actorID}
	}
	if err := s.repo.Create(ctx, room, participantIDs); err != nil {
		return nil, false, err
	}
	rwp := &RoomWithParticipants{
		Room:           *room,
		ParticipantIDs: participantIDs,
	}
	enrichParticipants(ctx, s.user, rwp)
	// Real-time: notify target user of new PM with full room (skip for notes room)
	if actorID != targetID && s.redis != nil && s.cfg != nil {
		recipients := make([]string, len(participantIDs))
		for i, pid := range participantIDs {
			recipients[i] = id.Format(pid)
		}
		payload := map[string]interface{}{
			"id":          id.Format(room.ID),
			"type":        TypePM,
			"recipients":  recipients,
			"created_at":  room.CreatedAt,
			"participants": rwp.Participants,
		}
		stargate.PublishToUser(ctx, s.redis, targetID, "ROOM_CREATE", payload, s.stargateRegion())
	}
	return rwp, true, nil
}

// CreateGroupPM creates a group PM with the given name and participants (actor is always included).
// All participant IDs must be friends of the actor (not enforced here; can be added).
func (s *Service) CreateGroupPM(ctx context.Context, actorID int64, name string, participantIDs []int64) (*RoomWithParticipants, error) {
	seen := map[int64]struct{}{actorID: {}}
	allIDs := []int64{actorID}
	for _, pid := range participantIDs {
		if pid == actorID {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		allIDs = append(allIDs, pid)
	}
	if len(allIDs) < 2 {
		return nil, ErrMinParticipants
	}
	roomID := id.Next()
	room := &Room{
		ID:       roomID,
		Type:     TypeGroupPM,
		SpaceID:  nil,
		ParentID: nil,
		Name:     name,
		Topic:    "",
	}
	if err := s.repo.Create(ctx, room, allIDs); err != nil {
		return nil, err
	}
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: allIDs}
	enrichParticipants(ctx, s.user, rwp)
	if s.redis != nil && s.cfg != nil {
		recipients := make([]string, len(allIDs))
		for i, pid := range allIDs {
			recipients[i] = id.Format(pid)
		}
		payload := map[string]interface{}{
			"id":           id.Format(room.ID),
			"type":         TypeGroupPM,
			"recipients":   recipients,
			"name":         room.Name,
			"created_at":   room.CreatedAt,
			"participants": rwp.Participants,
		}
		region := s.stargateRegion()
		for _, uid := range allIDs {
			stargate.PublishToUser(ctx, s.redis, uid, "ROOM_CREATE", payload, region)
		}
	}
	return rwp, nil
}

// AddParticipant adds a user to a group PM. Caller must be a participant; room must be TypeGroupPM.
func (s *Service) AddParticipant(ctx context.Context, actorID, roomID, targetID int64) error {
	room, err := s.repo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return ErrRoomNotFound
	}
	if room.Type != TypeGroupPM {
		return ErrNotGroupRoom
	}
	ids, err := s.repo.GetParticipants(ctx, roomID)
	if err != nil {
		return err
	}
	actorIn := false
	for _, pid := range ids {
		if pid == actorID {
			actorIn = true
			break
		}
	}
	if !actorIn {
		return ErrNotParticipant
	}
	for _, pid := range ids {
		if pid == targetID {
			return ErrAlreadyInGroup
		}
	}
	if err := s.repo.AddParticipant(ctx, roomID, targetID); err != nil {
		return err
	}
	newIDs, _ := s.repo.GetParticipants(ctx, roomID)
	rwp := &RoomWithParticipants{Room: *room, ParticipantIDs: newIDs}
	enrichParticipants(ctx, s.user, rwp)
	if s.redis != nil && s.cfg != nil {
		recipients := make([]string, len(newIDs))
		for i, pid := range newIDs {
			recipients[i] = id.Format(pid)
		}
		payload := map[string]interface{}{
			"id":           id.Format(room.ID),
			"type":         TypeGroupPM,
			"recipients":   recipients,
			"name":         room.Name,
			"created_at":   room.CreatedAt,
			"participants": rwp.Participants,
		}
		region := s.stargateRegion()
		stargate.PublishToUser(ctx, s.redis, targetID, "ROOM_CREATE", payload, region)
		for _, uid := range ids {
			stargate.PublishToUser(ctx, s.redis, uid, "ROOM_UPDATE", payload, region)
		}
	}
	return nil
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
		rwp.LastReadMessageID = row.LastReadMessageID
		rwp.MentionCount = row.MentionCount
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
							Presence:    auth.ToPublicPresence(u.Presence, true),
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
				Presence:    auth.ToPublicPresence(users[i].Presence, true),
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
	if row, _ := s.repo.GetRoomRow(ctx, userID, roomID); row != nil {
		rwp.LastReadMessageID = row.LastReadMessageID
		rwp.MentionCount = row.MentionCount
	}
	users, _ := s.user.GetByIDs(ctx, ids)
	for i := range ids {
		if i < len(users) && users[i] != nil {
			rwp.Participants = append(rwp.Participants, Participant{
				ID:          id.Format(users[i].ID),
				Username:    users[i].Username,
				DisplayName: users[i].DisplayName,
				Avatar:      users[i].Avatar,
				Presence:    auth.ToPublicPresence(users[i].Presence, true),
			})
		}
	}
	return rwp, nil
}

// Ack marks messages up to messageID as read for the user in the room.
// Publishes MESSAGE_ACK to the user's channel (for multi-session sync).
func (s *Service) Ack(ctx context.Context, userID, roomID, messageID int64) error {
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
	if err := s.repo.UpdateReadState(ctx, userID, roomID, messageID); err != nil {
		return err
	}
	if s.redis != nil && s.cfg != nil {
		region := s.cfg.Stargate.Region
		if region == "" {
			region = "default"
		}
		payload := map[string]interface{}{
			"room_id":              id.Format(roomID),
			"message_id":           id.Format(messageID),
			"last_read_message_id": id.Format(messageID),
		}
		stargate.PublishToUser(ctx, s.redis, userID, "MESSAGE_ACK", payload, region)
	}
	return nil
}
