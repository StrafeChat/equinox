package messages

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	ErrRoomNotFound   = errors.New("room not found")
	ErrNotParticipant = errors.New("not a participant")
	ErrMessageNotFound = errors.New("message not found")
	ErrForbidden      = errors.New("forbidden")
)

type Service struct {
	repo  Repository
	rooms rooms.Repository
	redis *redis.Client
	cfg   *config.Config
}

func NewService(repo Repository, roomsRepo rooms.Repository, redis *redis.Client, cfg *config.Config) *Service {
	return &Service{repo: repo, rooms: roomsRepo, redis: redis, cfg: cfg}
}

func (s *Service) ensureParticipant(ctx context.Context, userID, roomID int64) ([]int64, error) {
	participants, err := s.rooms.GetParticipants(ctx, roomID)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, p := range participants {
		if p == userID {
			ok = true
			break
		}
	}
	if !ok {
		return nil, ErrNotParticipant
	}
	return participants, nil
}

func (s *Service) Create(ctx context.Context, userID, roomID int64, in *CreateMessageInput) (*Message, error) {
	participants, err := s.ensureParticipant(ctx, userID, roomID)
	if err != nil {
		return nil, err
	}
	msgID := id.Next()
	m := &Message{
		RoomID:         roomID,
		ID:             msgID,
		SenderID:       userID,
		SenderDeviceID: in.SenderDeviceID,
		Ciphertext:     in.Ciphertext,
		ReplyToID:      in.ReplyToID,
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return nil, err
	}
	if err := s.rooms.UpdateLastMessageID(ctx, roomID, participants, msgID); err != nil {
		// non-fatal: message is stored
	}
	if s.redis != nil && s.cfg != nil {
		region := s.cfg.Stargate.Region
		if region == "" {
			region = "default"
		}
		payload := messageEventPayload(m)
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_CREATE", payload, region)
	}
	return m, nil
}

func (s *Service) Get(ctx context.Context, userID, roomID, msgID int64) (*Message, error) {
	if _, err := s.ensureParticipant(ctx, userID, roomID); err != nil {
		return nil, err
	}
	return s.repo.GetByID(ctx, roomID, msgID)
}

func (s *Service) List(ctx context.Context, userID, roomID int64, beforeID *int64, limit int) ([]Message, error) {
	if _, err := s.ensureParticipant(ctx, userID, roomID); err != nil {
		return nil, err
	}
	return s.repo.List(ctx, roomID, beforeID, limit)
}

func (s *Service) Edit(ctx context.Context, userID, roomID, msgID int64, in *EditMessageInput) (*Message, error) {
	if _, err := s.ensureParticipant(ctx, userID, roomID); err != nil {
		return nil, err
	}
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil || msg == nil {
		return nil, ErrMessageNotFound
	}
	if msg.SenderID != userID {
		return nil, ErrForbidden
	}
	if msg.DeletedAt != nil && !msg.DeletedAt.IsZero() {
		return nil, ErrMessageNotFound
	}
	updated, err := s.repo.Update(ctx, roomID, msgID, in.Ciphertext)
	if err != nil {
		return nil, err
	}
	if s.redis != nil && s.cfg != nil && updated != nil {
		region := s.cfg.Stargate.Region
		if region == "" {
			region = "default"
		}
		payload := messageEventPayload(updated)
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_UPDATE", payload, region)
	}
	return updated, nil
}

func (s *Service) Delete(ctx context.Context, userID, roomID, msgID int64) error {
	if _, err := s.ensureParticipant(ctx, userID, roomID); err != nil {
		return err
	}
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil || msg == nil {
		return ErrMessageNotFound
	}
	if msg.SenderID != userID {
		return ErrForbidden
	}
	if err := s.repo.SoftDelete(ctx, roomID, msgID); err != nil {
		return err
	}
	if s.redis != nil && s.cfg != nil {
		region := s.cfg.Stargate.Region
		if region == "" {
			region = "default"
		}
		payload := map[string]interface{}{
			"room_id":   id.Format(roomID),
			"message_id": id.Format(msgID),
		}
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_DELETE", payload, region)
	}
	return nil
}

func messageEventPayload(m *Message) map[string]interface{} {
	out := map[string]interface{}{
		"room_id":           id.Format(m.RoomID),
		"id":                id.Format(m.ID),
		"sender_id":         id.Format(m.SenderID),
		"sender_device_id":  id.Format(m.SenderDeviceID),
		"ciphertext":        m.Ciphertext,
		"created_at":        m.CreatedAt,
		"updated_at":        m.UpdatedAt,
	}
	if m.ReplyToID != nil {
		out["reply_to_id"] = id.Format(*m.ReplyToID)
	}
	if m.DeletedAt != nil && !m.DeletedAt.IsZero() {
		out["deleted_at"] = m.DeletedAt
	}
	return out
}
