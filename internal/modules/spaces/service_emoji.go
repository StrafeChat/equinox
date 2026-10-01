package spaces

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

var (
	ErrEmojiNotFound    = errors.New("emoji not found")
	ErrInvalidEmojiName = errors.New("emoji names are 2-32 letters, numbers, or underscores")
	ErrEmojiNameTaken   = errors.New("an emoji with that name already exists in this space")
	ErrEmojiLimit       = errors.New("this space has reached its custom emoji limit")
)

var emojiNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{2,32}$`)

// CanManageEmojis: Manage Emojis, Manage Space, or Administrator (owner implicitly).
func (s *Service) CanManageEmojis(ctx context.Context, actorID, spaceID int64) error {
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if permissions.Has(base, permissions.PermManageEmojis) ||
		permissions.Has(base, permissions.PermManageSpace) ||
		permissions.Has(base, permissions.PermAdministrator) {
		return nil
	}
	return ErrMissingPerm
}

// ListEmojis returns a space's custom emoji. Members only.
func (s *Service) ListEmojis(ctx context.Context, userID, spaceID int64) ([]SpaceEmoji, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	return s.repo.ListEmojis(ctx, spaceID)
}

// ListEmojisForUser returns the custom emoji of every space the user is in - what the
// picker offers, since a member may use any of them anywhere in the app.
func (s *Service) ListEmojisForUser(ctx context.Context, userID int64) ([]SpaceEmoji, error) {
	rows, err := s.ListSpacesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []SpaceEmoji
	for _, row := range rows {
		list, err := s.repo.ListEmojis(ctx, row.SpaceID)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// LookupEmoji resolves an emoji id seen in a message, for viewers who aren't members of
// its home space. Nothing here is secret - the image URL is already in every message
// that uses it.
func (s *Service) LookupEmoji(ctx context.Context, emojiID int64) (*SpaceEmojiRef, error) {
	ref, err := s.repo.GetEmojiByID(ctx, emojiID)
	if err != nil {
		return nil, err
	}
	if ref == nil {
		return nil, ErrEmojiNotFound
	}
	return ref, nil
}

// ValidateEmojiName normalises and checks a proposed name.
func ValidateEmojiName(name string) (string, error) {
	name = strings.TrimSpace(strings.Trim(strings.TrimSpace(name), ":"))
	if !emojiNameRe.MatchString(name) {
		return "", ErrInvalidEmojiName
	}
	return name, nil
}

func (s *Service) emojiNameTaken(ctx context.Context, spaceID int64, name string, exceptID int64) (bool, error) {
	list, err := s.repo.ListEmojis(ctx, spaceID)
	if err != nil {
		return false, err
	}
	for _, e := range list {
		if e.ID != exceptID && strings.EqualFold(e.Name, name) {
			return true, nil
		}
	}
	return false, nil
}

// CreateEmoji records an already-uploaded image as a custom emoji. The handler uploads
// to Nebula first (after CanManageEmojis) and passes the resulting URL here.
func (s *Service) CreateEmoji(ctx context.Context, actorID, spaceID, emojiID int64, name, url string, animated bool) (*SpaceEmoji, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteCreateEmoji(ctx, origin, spaceID, actor, emojiID, name, url, animated)
	}
	if err := s.CanManageEmojis(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	name, err := ValidateEmojiName(name)
	if err != nil {
		return nil, err
	}
	existing, err := s.repo.ListEmojis(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if len(existing) >= MaxEmojisPerSpace {
		return nil, ErrEmojiLimit
	}
	for _, e := range existing {
		if strings.EqualFold(e.Name, name) {
			return nil, ErrEmojiNameTaken
		}
	}
	now := time.Now().UTC()
	e := &SpaceEmoji{
		SpaceID:   spaceID,
		ID:        emojiID,
		Name:      name,
		URL:       url,
		Animated:  animated,
		CreatorID: actorID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.CreateEmoji(ctx, e); err != nil {
		return nil, err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_EMOJI_CREATE", spaceEmojiEventData(e))
	s.fedEmojiChanged(ctx, spaceID, e, false)
	s.audit(ctx, spaceID, actorID, AuditEmojiCreate, id.Format(e.ID), map[string]change{"name": {New: e.Name}}, "")
	return e, nil
}

func (s *Service) RenameEmoji(ctx context.Context, actorID, spaceID, emojiID int64, name string) (*SpaceEmoji, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteRenameEmoji(ctx, origin, spaceID, actor, emojiID, name)
	}
	if err := s.CanManageEmojis(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	name, err := ValidateEmojiName(name)
	if err != nil {
		return nil, err
	}
	e, err := s.repo.GetEmoji(ctx, spaceID, emojiID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, ErrEmojiNotFound
	}
	if taken, err := s.emojiNameTaken(ctx, spaceID, name, emojiID); err != nil {
		return nil, err
	} else if taken {
		return nil, ErrEmojiNameTaken
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateEmojiName(ctx, spaceID, emojiID, name, now); err != nil {
		return nil, err
	}
	oldName := e.Name
	e.Name = name
	e.UpdatedAt = now
	s.publishSpaceEvent(ctx, spaceID, "SPACE_EMOJI_UPDATE", spaceEmojiEventData(e))
	s.fedEmojiChanged(ctx, spaceID, e, false)
	s.audit(ctx, spaceID, actorID, AuditEmojiUpdate, id.Format(e.ID), map[string]change{"name": {Old: oldName, New: name}}, "")
	return e, nil
}

// DeleteEmoji removes the record and returns it so the handler can delete the image.
func (s *Service) DeleteEmoji(ctx context.Context, actorID, spaceID, emojiID int64) (*SpaceEmoji, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteDeleteEmoji(ctx, origin, spaceID, actor, emojiID)
	}
	if err := s.CanManageEmojis(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	e, err := s.repo.GetEmoji(ctx, spaceID, emojiID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, ErrEmojiNotFound
	}
	if err := s.repo.DeleteEmoji(ctx, spaceID, emojiID); err != nil {
		return nil, err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_EMOJI_DELETE", map[string]interface{}{
		"emoji_id": id.Format(emojiID),
	})
	s.fedEmojiChanged(ctx, spaceID, e, true)
	s.audit(ctx, spaceID, actorID, AuditEmojiDelete, id.Format(emojiID), map[string]change{"name": {Old: e.Name}}, "")
	return e, nil
}

func spaceEmojiEventData(e *SpaceEmoji) map[string]interface{} {
	return map[string]interface{}{
		"id":         id.Format(e.ID),
		"space_id":   id.Format(e.SpaceID),
		"name":       e.Name,
		"url":        e.URL,
		"animated":   e.Animated,
		"creator_id": id.Format(e.CreatorID),
		"created_at": e.CreatedAt,
		"updated_at": e.UpdatedAt,
	}
}
