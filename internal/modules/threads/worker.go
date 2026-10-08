package threads

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/safego"
)

const archiveSweepEvery = time.Minute

// StartArchiveWorker archives threads whose auto-archive timer ran out. Every (re)scheduling
// writes a queue row for the moment the thread would archive; the worker reads the rows that
// are due in the current and the previous hour bucket, and archives a thread only if that
// row still matches its last activity plus its duration - a thread that was active again
// since has a newer row somewhere ahead, and the stale one is just dropped.
func (s *Service) StartArchiveWorker(ctx context.Context) {
	safego.Go("threads", func() {
		t := time.NewTicker(archiveSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepArchiveQueue(ctx)
			}
		}
	})
}

func (s *Service) sweepArchiveQueue(ctx context.Context) {
	now := time.Now().UTC()
	buckets := []string{rooms.ThreadArchiveBucket(now), rooms.ThreadArchiveBucket(now.Add(-time.Hour))}
	seen := map[string]struct{}{}
	for _, b := range buckets {
		if _, dup := seen[b]; dup {
			continue
		}
		seen[b] = struct{}{}
		due, err := s.rooms.ListThreadArchiveDue(ctx, b, now)
		if err != nil {
			logger.Err("threads", err, map[string]any{"bucket": b, "step": "list archive queue"})
			continue
		}
		for _, d := range due {
			s.handleDue(ctx, d, now)
		}
	}
}

func (s *Service) handleDue(ctx context.Context, d rooms.ThreadArchiveDue, now time.Time) {
	defer func() { _ = s.rooms.DeleteThreadArchiveDue(ctx, d) }()
	room, err := s.rooms.GetByID(ctx, d.RoomID)
	if err != nil || room == nil || room.Type != rooms.TypeThread || room.ThreadIsArchived() || room.SpaceID == nil {
		return
	}
	auto := room.ThreadAutoArchiveMin
	if auto <= 0 {
		auto = DefaultAutoArchiveMin
	}
	expected := lastActive(room).Add(time.Duration(auto) * time.Minute)
	// A newer activity moved the deadline past this row: nothing to do here.
	if expected.After(now) {
		return
	}
	t := true
	if err := s.rooms.UpdateThread(ctx, room.ID, rooms.ThreadPatch{Archived: &t}); err != nil {
		logger.Err("threads", err, map[string]any{"thread_id": room.ID, "step": "auto-archive"})
		return
	}
	if updated, err := s.loadThread(ctx, room.ID); err == nil {
		if th, err := s.thread(ctx, 0, updated); err == nil {
			s.publish(ctx, updated, "THREAD_UPDATE", s.threadJSON(th))
		}
		s.spaces.FedRoomChanged(ctx, *updated.SpaceID, updated.ID, false)
	}
}
