package messages

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	ErrRoomNotFound    = errors.New("room not found")
	ErrNotParticipant  = errors.New("not a participant")
	ErrMessageNotFound = errors.New("message not found")
	ErrForbidden       = errors.New("forbidden")
	ErrInvalidInput    = errors.New("invalid message input")
)

// SpaceChannelAuth resolves membership and effective channel permission bits for space text channels.
type SpaceChannelAuth interface {
	IsMember(ctx context.Context, spaceID, userID int64) (bool, error)
	EffectiveChannelPermissions(ctx context.Context, userID, spaceID, roomID int64) (int64, error)
	// ListSpaceMemberUserIDs lists all members for fan-out of rooms_by_user.last_message_id on new messages.
	ListSpaceMemberUserIDs(ctx context.Context, spaceID int64) ([]int64, error)
}

type Service struct {
	repo         Repository
	rooms        rooms.Repository
	redis        *redis.Client
	cfg          *config.Config
	spaceAuth    SpaceChannelAuth
}

func NewService(repo Repository, roomsRepo rooms.Repository, redis *redis.Client, cfg *config.Config, spaceAuth SpaceChannelAuth) *Service {
	return &Service{repo: repo, rooms: roomsRepo, redis: redis, cfg: cfg, spaceAuth: spaceAuth}
}

func (s *Service) ensureParticipant(ctx context.Context, userID, roomID int64) ([]int64, error) {
	room, _ := s.rooms.GetByID(ctx, roomID)
	if room != nil && room.SpaceID != nil && s.spaceAuth != nil {
		ok, err := s.spaceAuth.IsMember(ctx, *room.SpaceID, userID)
		if err == nil && ok {
			return nil, nil
		}
	}
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

func (s *Service) requireSpaceTextPerms(ctx context.Context, userID, roomID int64, need int64) error {
	room, _ := s.rooms.GetByID(ctx, roomID)
	if room == nil || room.SpaceID == nil || room.Type != rooms.TypeSpaceText {
		return nil
	}
	if s.spaceAuth == nil {
		return nil
	}
	perms, err := s.spaceAuth.EffectiveChannelPermissions(ctx, userID, *room.SpaceID, roomID)
	if err != nil {
		return err
	}
	// Implicit rule: if user cannot view the channel, all other room permissions are irrelevant.
	if need != permissions.PermViewRoom && !permissions.Has(perms, permissions.PermViewRoom) {
		return ErrForbidden
	}
	if !permissions.Has(perms, need) {
		return ErrForbidden
	}
	return nil
}

func (s *Service) Create(ctx context.Context, userID, roomID int64, in *CreateMessageInput) (*Message, error) {
	participants, err := s.ensureParticipant(ctx, userID, roomID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceTextPerms(ctx, userID, roomID, permissions.PermSendMessages); err != nil {
		return nil, err
	}
	room, _ := s.rooms.GetByID(ctx, roomID)
	// Space channels have no room_participants; fan out last_message_id to every space member.
	if room != nil && room.SpaceID != nil && len(participants) == 0 && s.spaceAuth != nil {
		if room.Type == rooms.TypeSpaceText || room.Type == rooms.TypeSpaceVoice {
			ids, err := s.spaceAuth.ListSpaceMemberUserIDs(ctx, *room.SpaceID)
			if err == nil && len(ids) > 0 {
				participants = ids
			}
		}
	}
	e2eeOff := room != nil && (
		(room.Type == rooms.TypeGroupPM && room.E2EEEnabled != nil && !*room.E2EEEnabled) ||
		((room.Type == rooms.TypeSpaceText || room.Type == rooms.TypeSpaceVoice) && (room.E2EEEnabled == nil || !*room.E2EEEnabled)))
	var ciphertext, plaintext string
	if e2eeOff {
		plaintext = in.Plaintext
		if plaintext == "" {
			return nil, ErrInvalidInput
		}
	} else {
		ciphertext = in.Ciphertext
		if ciphertext == "" {
			return nil, ErrInvalidInput
		}
	}
	msgID := id.Next()
	m := &Message{
		RoomID:         roomID,
		ID:             msgID,
		SenderID:       userID,
		SenderDeviceID: in.SenderDeviceID,
		Ciphertext:     ciphertext,
		Plaintext:      plaintext,
		ReplyToID:      in.ReplyToID,
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return nil, err
	}
	if err := s.rooms.UpdateLastMessageID(ctx, roomID, participants, msgID); err != nil {
		// non-fatal: message is stored
	}
	if s.redis != nil && s.cfg != nil {
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_CREATE", messageEventPayload(m), s.cfg.Stargate.Region)
	}
	return m, nil
}

func (s *Service) Get(ctx context.Context, userID, roomID, msgID int64) (*Message, error) {
	if _, err := s.ensureParticipant(ctx, userID, roomID); err != nil {
		return nil, err
	}
	if err := s.requireSpaceTextPerms(ctx, userID, roomID, permissions.PermViewRoom); err != nil {
		return nil, err
	}
	if err := s.requireSpaceTextPerms(ctx, userID, roomID, permissions.PermReadMessageHistory); err != nil {
		return nil, err
	}
	return s.repo.GetByID(ctx, roomID, msgID)
}

func (s *Service) List(ctx context.Context, userID, roomID int64, beforeID *int64, limit int) ([]Message, error) {
	if _, err := s.ensureParticipant(ctx, userID, roomID); err != nil {
		return nil, err
	}
	if err := s.requireSpaceTextPerms(ctx, userID, roomID, permissions.PermViewRoom); err != nil {
		return nil, err
	}
	if err := s.requireSpaceTextPerms(ctx, userID, roomID, permissions.PermReadMessageHistory); err != nil {
		return nil, err
	}
	return s.repo.List(ctx, roomID, beforeID, limit)
}

func (s *Service) Edit(ctx context.Context, userID, roomID, msgID int64, in *EditMessageInput) (*Message, error) {
	if _, err := s.ensureParticipant(ctx, userID, roomID); err != nil {
		return nil, err
	}
	if err := s.requireSpaceTextPerms(ctx, userID, roomID, permissions.PermViewRoom); err != nil {
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
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_UPDATE", messageEventPayload(updated), s.cfg.Stargate.Region)
	}
	return updated, nil
}

func (s *Service) Delete(ctx context.Context, userID, roomID, msgID int64) error {
	if _, err := s.ensureParticipant(ctx, userID, roomID); err != nil {
		return err
	}
	if err := s.requireSpaceTextPerms(ctx, userID, roomID, permissions.PermViewRoom); err != nil {
		return err
	}
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil || msg == nil {
		return ErrMessageNotFound
	}
	if msg.SenderID != userID {
		room, _ := s.rooms.GetByID(ctx, roomID)
		if room != nil && room.SpaceID != nil && room.Type == rooms.TypeSpaceText {
			if err := s.requireSpaceTextPerms(ctx, userID, roomID, permissions.PermManageMessages); err != nil {
				return err
			}
		} else {
			return ErrForbidden
		}
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
			"room_id":     id.Format(roomID),
			"message_id": id.Format(msgID),
		}
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_DELETE", payload, s.cfg.Stargate.Region)
	}
	return nil
}

func messageEventPayload(m *Message) map[string]interface{} {
	out := map[string]interface{}{
		"room_id":          id.Format(m.RoomID),
		"id":               id.Format(m.ID),
		"sender_id":        id.Format(m.SenderID),
		"sender_device_id": id.Format(m.SenderDeviceID),
		"ciphertext":       m.Ciphertext,
		"created_at":       m.CreatedAt,
		"updated_at":       m.UpdatedAt,
	}
	if m.Plaintext != "" {
		out["plaintext"] = m.Plaintext
	}
	if m.ReplyToID != nil {
		out["reply_to_id"] = id.Format(*m.ReplyToID)
	}
	if m.DeletedAt != nil && !m.DeletedAt.IsZero() {
		out["deleted_at"] = m.DeletedAt
	}
	if m.SystemType != "" {
		out["system_type"] = m.SystemType
		out["system_payload"] = m.SystemPayload
	}
	return out
}
