package messages

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/nebula"
	"github.com/StrafeChat/equinox/internal/safego"
)

// Uploads that never get attached to a message - the composer was abandoned, the send failed
// after the upload - would otherwise sit in Nebula and message_attachments forever, and
// anyone could fill the disk by uploading without ever sending. Every upload is recorded in a
// Redis sorted set scored by its time; claiming it for a message removes it, and a periodic
// sweep deletes whatever is still unclaimed after pendingUploadTTL (object and row).
const (
	pendingUploadTTL = 24 * time.Hour
	orphanSweepEvery = 15 * time.Minute
	orphanSweepBatch = 500
)

func (s *Service) pendingUploadsKey() string {
	prefix := ""
	if s.cfg != nil {
		prefix = s.cfg.Database.Redis.CachePrefix
	}
	return prefix + "uploads:pending"
}

func pendingMember(roomID, attID int64) string {
	return id.Format(roomID) + ":" + id.Format(attID)
}

func (s *Service) trackPendingUpload(ctx context.Context, roomID, attID int64) {
	if s.redis == nil {
		return
	}
	z := redis.Z{Score: float64(time.Now().Unix()), Member: pendingMember(roomID, attID)}
	if err := s.redis.ZAdd(ctx, s.pendingUploadsKey(), z).Err(); err != nil {
		logger.Err("messages", err, map[string]any{"stage": "track_upload", "room_id": roomID})
	}
}

func (s *Service) untrackPendingUpload(ctx context.Context, roomID, attID int64) {
	if s.redis == nil {
		return
	}
	_ = s.redis.ZRem(ctx, s.pendingUploadsKey(), pendingMember(roomID, attID)).Err()
}

// StartOrphanSweeper runs the periodic orphan-upload sweep for the process lifetime. No-op
// without Redis (nothing is tracked) or Nebula (nothing is uploaded).
func (s *Service) StartOrphanSweeper(ctx context.Context) {
	if s.redis == nil || !s.UploadsConfigured() {
		return
	}
	safego.Go("messages", func() {
		timer := time.NewTimer(2 * time.Minute)
		defer timer.Stop()
		ticker := time.NewTicker(orphanSweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.sweepOrphans(ctx)
			case <-ticker.C:
				s.sweepOrphans(ctx)
			}
		}
	})
}

// sweepOrphans deletes uploads still unclaimed after pendingUploadTTL. An upload that was
// claimed in the meantime (row has a message_id) or already deleted is just dropped from the
// set. A failed Nebula delete leaves the entry for the next sweep.
func (s *Service) sweepOrphans(ctx context.Context) {
	cutoff := strconv.FormatInt(time.Now().Add(-pendingUploadTTL).Unix(), 10)
	members, err := s.redis.ZRangeByScore(ctx, s.pendingUploadsKey(), &redis.ZRangeBy{
		Min: "-inf", Max: cutoff, Count: orphanSweepBatch,
	}).Result()
	if err != nil {
		logger.Err("messages", err, map[string]any{"stage": "orphan_sweep"})
		return
	}
	for _, m := range members {
		roomStr, attStr, ok := strings.Cut(m, ":")
		roomID, e1 := id.Parse(roomStr)
		attID, e2 := id.Parse(attStr)
		if !ok || e1 != nil || e2 != nil {
			_ = s.redis.ZRem(ctx, s.pendingUploadsKey(), m).Err()
			continue
		}
		row, err := s.repo.GetAttachment(ctx, roomID, attID)
		if err != nil {
			continue // transient; retried next sweep
		}
		if row != nil && row.MessageID == nil {
			if s.ownsAttachment(row.URL) {
				if key := nebula.KeyFromURL(row.URL); key != "" {
					if err := nebula.Delete(ctx, s.cfg.Nebula.BaseURL, s.cfg.Nebula.UploadSecret, key); err != nil {
						logger.Err("messages", err, map[string]any{"stage": "orphan_delete", "key": key})
						continue
					}
				}
			}
			_ = s.repo.DeleteAttachment(ctx, roomID, attID)
		}
		_ = s.redis.ZRem(ctx, s.pendingUploadsKey(), m).Err()
	}
}
