package push

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/safego"
)

// The delivery side: a Redis Streams consumer group shared by every API process. Each
// process is one consumer with PUSH_WORKERS goroutines reading new jobs; a job a crashed
// process left unacknowledged is claimed by another after claimIdle (XAUTOCLAIM). A job is
// acknowledged and deleted once processed whatever became of each delivery - retries
// happen inside the delivery (sender.go), and a non-E2EE message's text must not linger in
// Redis after its push went out.

const (
	// claimIdle is how long a job may sit unacknowledged before another consumer takes it
	// over; well above deliveryBudget. A job can take longer than that (a message to a big
	// space, a push service timing out on many recipients), so the consumer working on one
	// keeps renewing its claim (keepClaimed) - otherwise a second process would deliver it
	// again, and every recipient would get the notification twice.
	claimIdle  = 60 * time.Second
	claimEvery = 30 * time.Second
	readBlock  = 5 * time.Second
	readCount  = 8
	// jobParallel bounds the concurrent deliveries of one job (a message in a big space
	// can have many recipients; one slow push service must not hold up the rest).
	jobParallel = 16
	// retryPause is the wait after a failed Redis read before trying again.
	retryPause = 2 * time.Second
)

// claimRefresh is how often a job being processed has its idle time reset; a variable so
// the Redis test can watch it happen without waiting a minute.
var claimRefresh = claimIdle / 3

// StartWorkers runs the delivery workers until ctx ends.
func (s *Service) StartWorkers(ctx context.Context) {
	if s.redis == nil {
		return
	}
	host, _ := os.Hostname()
	consumer := fmt.Sprintf("%s-%d", host, os.Getpid())
	safego.Go("push", func() {
		for ctx.Err() == nil {
			err := s.RebuildIndex(ctx)
			if err == nil {
				return
			}
			logger.Err("push", err, map[string]any{"step": "rebuild subscriber index"})
			_ = sleepCtx(ctx, 30*time.Second)
		}
	})
	for i := 0; i < s.cfg.Workers; i++ {
		safego.Go("push", func() { s.readLoop(ctx, consumer) })
	}
	safego.Go("push", func() { s.claimLoop(ctx, consumer) })
	logger.Info("push", "delivery workers started (%d, consumer %s)", s.cfg.Workers, consumer)
}

func (s *Service) ensureGroup(ctx context.Context) error {
	// "$": a brand-new group starts at the end - jobs queued before any worker existed
	// are old news.
	err := s.redis.XGroupCreateMkStream(ctx, streamKey, groupName, "$").Err()
	if err != nil && strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

func (s *Service) readLoop(ctx context.Context, consumer string) {
	grouped := false
	for ctx.Err() == nil {
		if !grouped {
			if err := s.ensureGroup(ctx); err != nil {
				noteRedis(err)
				_ = sleepCtx(ctx, retryPause)
				continue
			}
			grouped = true
		}
		streams, err := s.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: groupName, Consumer: consumer, Streams: []string{streamKey, ">"},
			Count: readCount, Block: readBlock,
		}).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			noteRedis(err)
			if strings.HasPrefix(err.Error(), "NOGROUP") {
				grouped = false // the stream was deleted (a Redis flush): recreate it
			}
			_ = sleepCtx(ctx, retryPause)
			continue
		}
		noteRedis(nil)
		for _, st := range streams {
			for _, msg := range st.Messages {
				s.handle(ctx, consumer, msg)
			}
		}
	}
}

// claimLoop takes over jobs another consumer took and never acknowledged (it crashed or
// was restarted mid-job).
func (s *Service) claimLoop(ctx context.Context, consumer string) {
	for sleepCtx(ctx, claimEvery) == nil {
		start := "0-0"
		for {
			msgs, next, err := s.redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
				Stream: streamKey, Group: groupName, Consumer: consumer,
				MinIdle: claimIdle, Start: start, Count: 50,
			}).Result()
			if err != nil {
				break
			}
			for _, msg := range msgs {
				s.handle(ctx, consumer, msg)
			}
			if next == "0-0" || next == "" || len(msgs) == 0 {
				break
			}
			start = next
		}
	}
}

// handle processes one stream entry and acknowledges it, also when it could not be
// parsed or processing panicked: a job that cannot be done must not come back forever.
func (s *Service) handle(ctx context.Context, consumer string, msg redis.XMessage) {
	stop := s.keepClaimed(consumer, msg.ID)
	defer func() {
		stop()
		// A fresh context: an entry processed during shutdown must still be removed.
		actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.redis.XAck(actx, streamKey, groupName, msg.ID).Err()
		_ = s.redis.XDel(actx, streamKey, msg.ID).Err()
	}()
	raw, _ := msg.Values["j"].(string)
	var j job
	if raw == "" || json.Unmarshal([]byte(raw), &j) != nil {
		return
	}
	safego.Run("push", func() { s.process(ctx, &j) })
}

// keepClaimed renews this consumer's claim on a job every claimRefresh until the returned
// stop is called: XCLAIM to ourselves with JUSTID resets the entry's idle time without
// counting another delivery, so claimLoop elsewhere never sees it as abandoned while it is
// still being worked on. A process that dies stops renewing, and the job is taken over
// claimIdle later, as before.
func (s *Service) keepClaimed(consumer, id string) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	safego.Go("push", func() {
		defer close(finished)
		t := time.NewTicker(claimRefresh)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.redis.XClaimJustID(cctx, &redis.XClaimArgs{
					Stream: streamKey, Group: groupName, Consumer: consumer, MinIdle: 0, Messages: []string{id},
				}).Err()
				cancel()
			}
		}
	})
	return func() {
		close(done)
		<-finished
	}
}

// process delivers one job to each of its candidate users.
func (s *Service) process(ctx context.Context, j *job) {
	jc := newJobCtx(s, j)
	switch j.T {
	case TypeMessage:
		if j.Message == nil {
			return
		}
		// Load the shared context once, before fanning out.
		if !jc.loadRoom(ctx) {
			return
		}
	case TypeCall:
		if !jc.loadRoom(ctx) {
			return
		}
	case TypeRead, TypeCallEnd:
	default:
		return
	}
	sem := make(chan struct{}, jobParallel)
	var wg sync.WaitGroup
	for _, uid := range j.Users {
		sem <- struct{}{}
		wg.Add(1)
		safego.Go("push", func() {
			defer func() { <-sem; wg.Done() }()
			s.deliverTo(ctx, jc, uid)
		})
	}
	wg.Wait()
}

// liveSubscriptions returns the user's subscriptions whose sessions are still signed in,
// deleting the rest (a session that expired, or was revoked while the hook could not
// reach the database).
func (s *Service) liveSubscriptions(ctx context.Context, userID int64) []Subscription {
	subs, err := s.repo.ListByUser(ctx, userID)
	if err != nil {
		logger.Err("push", err, map[string]any{"user_id": userID, "step": "list subscriptions"})
		return nil
	}
	if len(subs) == 0 {
		// The index was stale (it is only a filter); put it right.
		markUnsubscribed(ctx, s.redis, userID)
		return nil
	}
	live := subs[:0]
	for _, sub := range subs {
		ok := usable(&sub)
		if ok {
			active, err := s.dir.SessionActive(ctx, userID, sub.SessionID)
			if err != nil {
				logger.Err("push", err, map[string]any{"user_id": userID, "step": "check session"})
				continue
			}
			ok = active
		}
		if !ok {
			_ = s.Delete(ctx, userID, sub.SessionID)
			continue
		}
		live = append(live, sub)
	}
	return live
}

// deliverTo makes the per-user decision and sends to each of the user's devices that
// wants this push.
func (s *Service) deliverTo(ctx context.Context, jc *jobCtx, userID int64) {
	subs := s.liveSubscriptions(ctx, userID)
	if len(subs) == 0 {
		return
	}
	j := jc.job
	d := deliveryFor(j.T, j.Room)
	switch j.T {
	case TypeMessage:
		if jc.room.SpaceID != 0 {
			ok, err := s.dir.CanView(ctx, userID, jc.room)
			if err != nil {
				logger.Err("push", err, map[string]any{"user_id": userID, "room_id": j.Room, "step": "view permission"})
			}
			if !ok {
				return
			}
		}
		recipient := jc.user(ctx, userID)
		settings, err := s.dir.RoomSettings(ctx, userID, j.Room)
		if err != nil {
			logger.Err("push", err, map[string]any{"user_id": userID, "room_id": j.Room, "step": "room settings"})
			return
		}
		dec := messageDecision{
			recipient: recipient, senderID: j.Message.Sender, roomType: jc.room.Type, settings: settings,
			spaceDefaultMentionsOnly: jc.space != nil && jc.space.DefaultMentionsOnly,
			mentioned:                jc.mentions(userID), now: s.now(),
		}
		if !dec.recipientWants() {
			return
		}
		sent := false
		for i := range subs {
			sub := &subs[i]
			dec.prefs = sub.Prefs()
			if !dec.notify() {
				continue
			}
			payload, err := jc.messagePayload(ctx, userID, dec.prefs.Preview, dec.mentioned)
			if err != nil {
				continue
			}
			if s.send(ctx, sub, payload, d) {
				sent = true
			}
		}
		if sent && s.redis != nil {
			// Read-sync pushes only when a notification may be showing (EnqueueRead).
			_ = s.redis.Set(ctx, sentKey(userID, j.Room), "1", sentTTL).Err()
		}
	case TypeCall:
		recipient := jc.user(ctx, userID)
		payload, err := jc.callPayload(ctx, userID)
		if err != nil {
			return
		}
		for i := range subs {
			if notifyCall(recipient, j.Caller, subs[i].Prefs()) {
				s.send(ctx, &subs[i], payload, d)
			}
		}
	case TypeCallEnd:
		payload, err := jc.basePayload(userID, TypeCallEnd, false)
		if err != nil {
			return
		}
		for i := range subs {
			// A device that never rings has no ringing to stop.
			if subs[i].Prefs().Calls {
				s.send(ctx, &subs[i], payload, d)
			}
		}
	case TypeRead:
		payload, err := jc.basePayload(userID, TypeRead, true)
		if err != nil {
			return
		}
		for i := range subs {
			s.send(ctx, &subs[i], payload, d)
		}
	}
}

// send delivers one push with retries and records the outcome on the subscription.
func (s *Service) send(ctx context.Context, sub *Subscription, payload []byte, d delivery) bool {
	out := s.sender.Send(ctx, sub, payload, d, true)
	s.record(ctx, sub, out)
	return out.kind == delivered
}

func formatID(v int64) string { return strconv.FormatInt(v, 10) }
