package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/nebula"
	"github.com/StrafeChat/equinox/internal/safego"
)

// What the proxy keeps. In Redis, under the instance's cache prefix and keyed by the source's
// keyed hash (never the URL itself):
//   - media:img:<h>  the image's record (type, size, dimensions, whether Nebula holds it), for
//     MEDIA_PROXY_CACHE_TTL;
//   - media:fail:<h> the status a failed fetch ended in, for failureTTL;
//   - media:cache    a sorted set of the cached objects ("<h>.<ext>.<bytes>") by expiry, which
//     the sweeper deletes from Nebula once they are due (the messages/orphans.go pattern);
//   - media:cache:bytes the sum of the <bytes> of every entry in media:cache, which keeps
//     the whole cache within MEDIA_PROXY_CACHE_MAX_GB.
//
// In Nebula, media-cache/<h>.<ext>, the bytes - only when Nebula is configured and the cache
// has room. Otherwise the record still remembers an image's size, and the bytes are fetched
// again per view.
const (
	failureTTL   = 15 * time.Minute
	storeTimeout = 30 * time.Second
	sweepEvery   = 15 * time.Minute
	sweepBatch   = 500
	objectPrefix = "media-cache/"
)

type record struct {
	ContentType string `json:"ct"`
	Ext         string `json:"ext"`
	Size        int64  `json:"size"`
	Width       int    `json:"w,omitempty"`
	Height      int    `json:"h,omitempty"`
	// Stored: the bytes are in Nebula at objectPrefix + <h>.<ext>.
	Stored bool `json:"stored,omitempty"`
}

func (s *Service) recordKey(h string) string  { return s.prefix + "media:img:" + h }
func (s *Service) failureKey(h string) string { return s.prefix + "media:fail:" + h }
func (s *Service) indexKey() string           { return s.prefix + "media:cache" }
func (s *Service) bytesKey() string           { return s.prefix + "media:cache:bytes" }

func objectKey(h, ext string) string { return objectPrefix + h + "." + ext }

// entry is an object's member in the index: its name and the bytes it counts for.
func entry(h, ext string, size int64) string {
	return h + "." + ext + "." + strconv.FormatInt(size, 10)
}

func parseEntry(m string) (h, ext string, size int64, ok bool) {
	h, rest, _ := strings.Cut(m, ".")
	ext, n, _ := strings.Cut(rest, ".")
	size, err := strconv.ParseInt(n, 10, 64)
	return h, ext, size, err == nil && size >= 0 && len(h) == 32 && validExt(ext)
}

func (s *Service) record(ctx context.Context, h string) *record {
	if s.redis == nil {
		return nil
	}
	raw, err := s.redis.Get(ctx, s.recordKey(h)).Result()
	if err != nil {
		return nil
	}
	var rec record
	if json.Unmarshal([]byte(raw), &rec) != nil || imageTypes[rec.ContentType] != rec.Ext {
		return nil
	}
	return &rec
}

// failure is the status a recent fetch of the source failed with, 0 for none.
func (s *Service) failure(ctx context.Context, h string) int {
	if s.redis == nil {
		return 0
	}
	raw, err := s.redis.Get(ctx, s.failureKey(h)).Result()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(raw)
	return n
}

// remember keeps a failed fetch's status for failureTTL. A bad address is not a fetch and
// is not remembered.
func (s *Service) remember(ctx context.Context, h string, status int) {
	if s.redis == nil || status == http.StatusBadRequest || status == http.StatusTooManyRequests {
		return
	}
	if err := s.redis.Set(ctx, s.failureKey(h), strconv.Itoa(status), failureTTL).Err(); err != nil {
		logger.Err("media", err, map[string]any{"stage": "remember_failure"})
	}
}

func (s *Service) forget(ctx context.Context, h string) {
	if s.redis != nil {
		_ = s.redis.Del(ctx, s.recordKey(h)).Err()
	}
}

// keep records a fetched image, and puts its bytes in Nebula when there is one and the cache
// has room for them. A failed upload only means the next view fetches again.
func (s *Service) keep(ctx context.Context, h string, f *fetched) {
	if s.redis == nil {
		return
	}
	rec := record{ContentType: f.contentType, Ext: f.ext, Size: int64(len(f.data)), Width: f.width, Height: f.height}
	if s.store {
		if m, ok := s.track(ctx, h, f.ext, rec.Size); ok {
			err := nebula.Put(ctx, s.nebula.BaseURL, s.nebula.UploadSecret, objectKey(h, f.ext), bytes.NewReader(f.data), f.contentType, rec.Size)
			rec.Stored = err == nil
			if err != nil {
				logger.Err("media", err, map[string]any{"stage": "cache_put"})
				// Due now: the next sweep deletes whatever the upload left and gives its bytes
				// back, rather than the cache counting them for MEDIA_PROXY_CACHE_TTL.
				if added, err := s.redis.ZAdd(ctx, s.indexKey(), redis.Z{Score: float64(s.now().Unix()), Member: m}).Result(); err == nil && added > 0 {
					_ = s.redis.IncrBy(ctx, s.bytesKey(), rec.Size).Err()
				}
			}
		}
	}
	b, err := json.Marshal(rec)
	if err == nil {
		err = s.redis.Set(ctx, s.recordKey(h), string(b), s.cfg.CacheTTL).Err()
	}
	if err != nil {
		logger.Err("media", err, map[string]any{"stage": "cache_record"})
	}
}

// track makes room for an object in the cache and indexes it, before it is uploaded: nothing
// is ever stored that the sweeper does not know to delete (an index entry for an upload that
// failed is just swept as gone). The bytes are counted first, so however many replicas keep
// images at once the total stays within MEDIA_PROXY_CACHE_MAX_GB; past it, ok is false and
// the image is served without being kept. An entry already in the index (the same image
// fetched again) is counted once.
func (s *Service) track(ctx context.Context, h, ext string, size int64) (string, bool) {
	m := entry(h, ext, size)
	z := redis.Z{Score: float64(s.now().Add(s.cfg.CacheTTL).Unix()), Member: m}
	if _, err := s.redis.ZScore(ctx, s.indexKey(), m).Result(); err == nil {
		// Already counted: only its expiry moves - unless a sweep took it out in between, and
		// then it is indexed afresh and counts afresh.
		added, err := s.redis.ZAdd(ctx, s.indexKey(), z).Result()
		if err != nil {
			logger.Err("media", err, map[string]any{"stage": "cache_index"})
			return "", false
		}
		if added > 0 {
			_ = s.redis.IncrBy(ctx, s.bytesKey(), size).Err()
		}
		return m, true
	}
	used, err := s.redis.IncrBy(ctx, s.bytesKey(), size).Result()
	if err != nil {
		logger.Err("media", err, map[string]any{"stage": "cache_count"})
		return "", false
	}
	if used > s.cfg.CacheMaxBytes {
		_ = s.redis.DecrBy(ctx, s.bytesKey(), size).Err()
		return "", false
	}
	added, err := s.redis.ZAdd(ctx, s.indexKey(), z).Result()
	if err != nil || added == 0 {
		_ = s.redis.DecrBy(ctx, s.bytesKey(), size).Err()
	}
	if err != nil {
		logger.Err("media", err, map[string]any{"stage": "cache_index"})
		return "", false
	}
	return m, true
}

// untrack takes a deleted object's entry out of the index and its bytes out of the count -
// once, however many sweepers removed it. False when the entry could not be removed.
func (s *Service) untrack(ctx context.Context, m string, size int64) bool {
	n, err := s.redis.ZRem(ctx, s.indexKey(), m).Result()
	if err != nil {
		return false
	}
	if n > 0 && size > 0 {
		_ = s.redis.DecrBy(ctx, s.bytesKey(), size).Err()
	}
	return true
}

// openStored reads a cached image back from Nebula's internal address. The body is streamed
// to the reader after the handler returns, so the request has no context to cancel it early;
// the client's timeout bounds it.
func (s *Service) openStored(h string, rec *record) (io.ReadCloser, int64, error) {
	base := strings.TrimRight(strings.TrimSpace(s.nebula.BaseURL), "/")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/v1/"+objectKey(h, rec.Ext), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.cdn.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, errors.New("media: cached object is " + resp.Status)
	}
	size := resp.ContentLength
	if size < 0 {
		size = rec.Size
	}
	return resp.Body, size, nil
}

// StartSweeper deletes cached images from Nebula once their time is up, for the process
// lifetime. No-op without Nebula (nothing is stored) or Redis (nothing is tracked).
func (s *Service) StartSweeper(ctx context.Context) {
	if s == nil || !s.store || s.redis == nil {
		return
	}
	safego.Go("media", func() {
		timer := time.NewTimer(3 * time.Minute)
		defer timer.Stop()
		ticker := time.NewTicker(sweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.sweep(ctx)
			case <-ticker.C:
				s.sweep(ctx)
			}
		}
	})
}

// sweep deletes the objects that are due, a batch at a time until none are left. One fetched
// again since the listing has a later score and is left alone; in the moment between that
// check and the delete, a fresh copy can still go, and the next view then finds the object
// gone and fetches once more. A failed delete is stepped over and left for the next sweep.
func (s *Service) sweep(ctx context.Context) {
	now := s.now().Unix()
	// skipped is how many due entries the listings so far have left in place, which the next
	// listing starts after.
	var skipped int64
	for ctx.Err() == nil {
		members, err := s.redis.ZRangeByScore(ctx, s.indexKey(), &redis.ZRangeBy{
			Min: "-inf", Max: strconv.FormatInt(now, 10), Offset: skipped, Count: sweepBatch,
		}).Result()
		if err != nil {
			logger.Err("media", err, map[string]any{"stage": "sweep"})
			return
		}
		for _, m := range members {
			h, ext, size, ok := parseEntry(m)
			if !ok {
				if !s.untrack(ctx, m, 0) {
					skipped++
				}
				continue
			}
			if score, err := s.redis.ZScore(ctx, s.indexKey(), m).Result(); err == nil && int64(score) > now {
				continue
			}
			if err := nebula.Delete(ctx, s.nebula.BaseURL, s.nebula.UploadSecret, objectKey(h, ext)); err != nil {
				logger.Err("media", err, map[string]any{"stage": "sweep_delete"})
				skipped++
				continue
			}
			if !s.untrack(ctx, m, size) {
				skipped++
			}
		}
		if len(members) < sweepBatch {
			return
		}
	}
}

func validExt(ext string) bool {
	for _, e := range imageTypes {
		if e == ext {
			return true
		}
	}
	return false
}
