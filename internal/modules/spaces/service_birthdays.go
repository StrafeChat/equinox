package spaces

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/safego"
)

// birthdayMarkerTTL keeps the once-a-day and per-member dedup keys around well past the day
// they guard, so a late or restarted run never double-posts.
const birthdayMarkerTTL = 48 * time.Hour

func (s *Service) redisKeyPrefix() string {
	if s.cfg == nil {
		return ""
	}
	p := s.cfg.Database.Redis.CachePrefix
	if p != "" && !strings.HasSuffix(p, ":") {
		p += ":"
	}
	return p
}

// StartBirthdayWorker runs a daily pass that wishes opted-in members a happy birthday in the
// birthday channel of each space that configured one. The pass runs at most once per UTC day,
// guarded by a Redis key so neither a restart nor a second API replica double-announces. It
// needs Redis (for the guard) and a system messenger (to post); without either it is a no-op.
func (s *Service) StartBirthdayWorker(ctx context.Context) {
	if s.redis == nil || s.systemMessenger == nil {
		return
	}
	safego.Go("spaces", func() {
		// A short initial delay lets the process finish starting before the first pass.
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.runBirthdaysForToday(ctx)
			case <-ticker.C:
				s.runBirthdaysForToday(ctx)
			}
		}
	})
}

func (s *Service) runBirthdaysForToday(ctx context.Context) {
	now := time.Now().UTC()
	dateKey := now.Format("2006-01-02")
	// One winner per UTC day does the work.
	runKey := s.redisKeyPrefix() + "birthday:run:" + dateKey
	ok, err := s.redis.SetNX(ctx, runKey, "1", birthdayMarkerTTL).Result()
	if err != nil || !ok {
		return
	}
	month, day := int(now.Month()), now.Day()
	userIDs, err := s.userRepo.ListBirthdaysOn(ctx, month, day)
	if err != nil {
		logger.Err("spaces", err, map[string]any{"birthday_month": month, "birthday_day": day})
		return
	}
	for _, uid := range userIDs {
		u, err := s.userRepo.GetByID(ctx, uid)
		if err != nil || u == nil || !u.BirthdayOptIn || u.DateOfBirth.IsZero() {
			continue
		}
		// Trust but verify the index: skip anyone whose stored date doesn't actually match.
		if int(u.DateOfBirth.Month()) != month || u.DateOfBirth.Day() != day {
			continue
		}
		for _, spaceID := range u.Spaces {
			s.announceBirthday(ctx, spaceID, u, dateKey)
		}
	}
}

func (s *Service) announceBirthday(ctx context.Context, spaceID int64, u *auth.User, dateKey string) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil || sp.BirthdayChannelID == nil {
		return
	}
	room, err := s.roomRepo.GetByID(ctx, *sp.BirthdayChannelID)
	if err != nil || room == nil || room.SpaceID == nil || *room.SpaceID != spaceID || room.Type != rooms.TypeSpaceText {
		return
	}
	// One greeting per member per space per day, even across replicas and restarts.
	dedup := s.redisKeyPrefix() + "birthday:done:" + id.Format(spaceID) + ":" + id.Format(u.ID) + ":" + dateKey
	ok, err := s.redis.SetNX(ctx, dedup, "1", birthdayMarkerTTL).Result()
	if err != nil || !ok {
		return
	}
	memberIDs, err := s.ListSpaceMemberUserIDs(ctx, spaceID)
	if err != nil {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"user_id": id.Format(u.ID),
		"message": sp.BirthdayMessage,
	})
	if err != nil {
		return
	}
	if _, err := s.systemMessenger(ctx, room.ID, memberIDs, SystemBirthday, string(payload)); err != nil {
		logger.Err("spaces", err, map[string]any{"space_id": spaceID, "user_id": u.ID, "system_type": SystemBirthday})
	}
}
