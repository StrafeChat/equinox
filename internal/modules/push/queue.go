package push

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/logger"
)

// The producers. Each is called from the request (or federation) path that caused the
// event, so each costs at most a couple of Redis round trips and never waits for a
// delivery: they work out who could be notified at all (accounts with a subscription,
// from the push:users set) and append one job to the push:jobs stream for the delivery
// workers (worker.go). With Redis unavailable the push is dropped - a missed notification
// is better than a failed send.

const (
	streamKey     = "push:jobs"
	groupName     = "push"
	usersKey      = "push:users"
	usersReadyKey = "push:users:ready"
	streamMaxLen  = 100000
	// sentTTL is how long "this room has a notification showing" is remembered, so
	// read-sync only pushes when there is something to clear.
	sentTTL = 7 * 24 * time.Hour
	// ringTTL bounds the remembered ringing set of a call whose end was never seen.
	ringTTL = 24 * time.Hour
	// smismemberChunk bounds one SMISMEMBER (a space can have many members).
	smismemberChunk = 1000
)

func sentKey(userID, roomID int64) string {
	return "push:sent:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(roomID, 10)
}

func ringKey(roomID int64) string { return "push:ringing:" + strconv.FormatInt(roomID, 10) }

// producing gates every producer. It is off until the API's route setup turns it on with
// push enabled, so a process that never wires push (the gateway binary, tests) never
// queues anything.
var producing atomic.Bool

// SetProducing switches the producers on or off.
func SetProducing(on bool) { producing.Store(on) }

// redisDown remembers that the last queue write failed, so an outage is logged once rather
// than once per message.
var redisDown atomic.Bool

func noteRedis(err error) {
	if err == nil {
		redisDown.Store(false)
		return
	}
	if redisDown.CompareAndSwap(false, true) {
		logger.Error("push", "queue unavailable, dropping pushes until it recovers: %v", err)
	}
}

// job is one stream entry. Users are the candidate recipients (already narrowed to
// accounts with a subscription); the worker makes the per-user decision.
type job struct {
	T        string      `json:"t"`
	Users    []int64     `json:"u"`
	Room     int64       `json:"r,omitempty"`
	RoomType int         `json:"rt,omitempty"`
	Space    int64       `json:"s,omitempty"`
	TS       int64       `json:"ts"`
	Message  *messageJob `json:"m,omitempty"`
	ReadTo   int64       `json:"rd,omitempty"`
	Caller   int64       `json:"c,omitempty"`
}

type messageJob struct {
	ID     int64 `json:"id"`
	Sender int64 `json:"a"`
	E2EE   bool  `json:"e,omitempty"`
	// Text is the plaintext of a non-E2EE message, for the preview; never set for an
	// end-to-end encrypted room (the server has no plaintext there).
	Text        string  `json:"x,omitempty"`
	Attachments int     `json:"att,omitempty"`
	Mentioned   []int64 `json:"mu,omitempty"`
	Everyone    bool    `json:"me,omitempty"`
}

func enqueue(ctx context.Context, rdb *redis.Client, j *job) {
	raw, err := json.Marshal(j)
	if err != nil {
		return
	}
	err = rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: streamMaxLen,
		Approx: true,
		Values: map[string]interface{}{"j": raw},
	}).Err()
	noteRedis(err)
}

// Subscribed narrows userIDs to the accounts with at least one push subscription, leaving
// out exclude (the sender). Cheap enough to call before doing any other work for a push.
func Subscribed(ctx context.Context, rdb *redis.Client, userIDs []int64, exclude int64) []int64 {
	if rdb == nil || !producing.Load() || len(userIDs) == 0 {
		return nil
	}
	var out []int64
	for start := 0; start < len(userIDs); start += smismemberChunk {
		chunk := userIDs[start:min(start+smismemberChunk, len(userIDs))]
		members := make([]interface{}, len(chunk))
		for i, uid := range chunk {
			members[i] = strconv.FormatInt(uid, 10)
		}
		flags, err := rdb.SMIsMember(ctx, usersKey, members...).Result()
		noteRedis(err)
		if err != nil {
			return nil
		}
		for i, ok := range flags {
			if ok && chunk[i] != exclude {
				out = append(out, chunk[i])
			}
		}
	}
	return dedupe(out)
}

func dedupe(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Message is a new message, for EnqueueMessage.
type Message struct {
	RoomID   int64
	RoomType int
	SpaceID  int64 // 0 outside a space
	// E2EE: the room is end-to-end encrypted; Text must then be empty.
	E2EE        bool
	ID          int64
	SenderID    int64
	Text        string
	Attachments int
	// Mentioned is everyone the message mentions directly or through a role; Everyone is
	// an @everyone/@here that was allowed to notify.
	Mentioned []int64
	Everyone  bool
	CreatedAt time.Time
}

// EnqueueMessage queues a message push for the room's participants that have a
// subscription, except the sender. participants is the fan-out set the caller already
// computed (thread members for a thread).
func EnqueueMessage(ctx context.Context, rdb *redis.Client, m Message, participants []int64) {
	if m.SenderID == 0 {
		return
	}
	users := Subscribed(ctx, rdb, participants, m.SenderID)
	if len(users) == 0 {
		return
	}
	// Only the mentions of people who are actually candidates travel with the job - a role
	// mention in a large space can name thousands.
	cand := make(map[int64]struct{}, len(users))
	for _, uid := range users {
		cand[uid] = struct{}{}
	}
	var mentioned []int64
	for _, uid := range m.Mentioned {
		if _, ok := cand[uid]; ok {
			mentioned = append(mentioned, uid)
			delete(cand, uid)
		}
	}
	text := m.Text
	if m.E2EE {
		text = ""
	}
	ts := m.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	enqueue(ctx, rdb, &job{
		T: TypeMessage, Users: users, Room: m.RoomID, RoomType: m.RoomType, Space: m.SpaceID, TS: ts.UnixMilli(),
		Message: &messageJob{
			ID: m.ID, Sender: m.SenderID, E2EE: m.E2EE, Text: text, Attachments: m.Attachments,
			Mentioned: mentioned, Everyone: m.Everyone,
		},
	})
}

// EnqueueRead tells userID's devices the room was read up to messageID elsewhere, but only
// if a message push for the room was delivered since the last time - otherwise there is
// nothing on the phone to clear.
func EnqueueRead(ctx context.Context, rdb *redis.Client, userID, roomID int64, roomType int, spaceID, messageID int64) {
	if rdb == nil || !producing.Load() {
		return
	}
	n, err := rdb.Del(ctx, sentKey(userID, roomID)).Result()
	noteRedis(err)
	if err != nil || n == 0 {
		return
	}
	enqueue(ctx, rdb, &job{
		T: TypeRead, Users: []int64{userID}, Room: roomID, RoomType: roomType, Space: spaceID,
		TS: time.Now().UnixMilli(), ReadTo: messageID,
	})
}

// NotificationShowing reports whether a message push for the room was delivered to the
// user since the room was last read - whether EnqueueRead would have anything to clear.
// For a caller that must do work (load the room) before it can call EnqueueRead.
func NotificationShowing(ctx context.Context, rdb *redis.Client, userID, roomID int64) bool {
	if rdb == nil || !producing.Load() {
		return false
	}
	n, err := rdb.Exists(ctx, sentKey(userID, roomID)).Result()
	noteRedis(err)
	return err == nil && n > 0
}

// Call is the ringing state of a PM or group PM call, for EnqueueCall.
type Call struct {
	RoomID   int64
	RoomType int
	// CallerID started the call; shown as who is calling.
	CallerID int64
	// Ringing is everyone the call is ringing right now.
	Ringing []int64
	// RingAll: ring everyone in Ringing again, not only those newly added - a new call, or
	// a re-ring, which restarts the ringtone on every callee.
	RingAll bool
}

// ringDiff replaces a call's remembered ringing set and returns who started ringing
// (everyone, with ringAll) and who stopped, atomically, so two events for the same call
// can never both announce the same change.
var ringDiff = redis.NewScript(`
local old = redis.call('SMEMBERS', KEYS[1])
local was = {}
for _, v in ipairs(old) do was[v] = true end
local now = {}
local started = {}
for i = 3, #ARGV do
  local v = ARGV[i]
  if not now[v] then
    now[v] = true
    if ARGV[1] == '1' or not was[v] then table.insert(started, v) end
  end
end
local stopped = {}
for _, v in ipairs(old) do
  if not now[v] then table.insert(stopped, v) end
end
redis.call('DEL', KEYS[1])
if #ARGV > 2 then
  redis.call('SADD', KEYS[1], unpack(ARGV, 3))
  redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
end
return {started, stopped}
`)

// EnqueueCall queues "incoming call" for the callees who started ringing and "stopped
// ringing" for those who no longer are (answered, declined, rung out).
func EnqueueCall(ctx context.Context, rdb *redis.Client, c Call) {
	if rdb == nil || !producing.Load() {
		return
	}
	args := make([]interface{}, 0, len(c.Ringing)+2)
	all := "0"
	if c.RingAll {
		all = "1"
	}
	args = append(args, all, int(ringTTL.Seconds()))
	for _, uid := range c.Ringing {
		if uid != c.CallerID {
			args = append(args, strconv.FormatInt(uid, 10))
		}
	}
	started, stopped, ok := runRingDiff(ctx, rdb, c.RoomID, args)
	if !ok {
		return
	}
	now := time.Now().UnixMilli()
	if users := Subscribed(ctx, rdb, started, c.CallerID); len(users) > 0 {
		enqueue(ctx, rdb, &job{T: TypeCall, Users: users, Room: c.RoomID, RoomType: c.RoomType, TS: now, Caller: c.CallerID})
	}
	if users := Subscribed(ctx, rdb, stopped, 0); len(users) > 0 {
		enqueue(ctx, rdb, &job{T: TypeCallEnd, Users: users, Room: c.RoomID, RoomType: c.RoomType, TS: now})
	}
}

// EnqueueCallEnded queues "stopped ringing" for everyone the call was still ringing when
// it ended.
func EnqueueCallEnded(ctx context.Context, rdb *redis.Client, roomID int64, roomType int) {
	EnqueueCall(ctx, rdb, Call{RoomID: roomID, RoomType: roomType})
}

func runRingDiff(ctx context.Context, rdb *redis.Client, roomID int64, args []interface{}) (started, stopped []int64, ok bool) {
	res, err := ringDiff.Run(ctx, rdb, []string{ringKey(roomID)}, args...).Slice()
	noteRedis(err)
	if err != nil || len(res) != 2 {
		return nil, nil, false
	}
	return parseIDList(res[0]), parseIDList(res[1]), true
}

func parseIDList(v interface{}) []int64 {
	list, _ := v.([]interface{})
	out := make([]int64, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// markSubscribed / markUnsubscribed maintain push:users, the "has a subscription" index
// the producers consult.
func markSubscribed(ctx context.Context, rdb *redis.Client, userID int64) {
	if rdb != nil {
		_ = rdb.SAdd(ctx, usersKey, strconv.FormatInt(userID, 10)).Err()
	}
}

func markUnsubscribed(ctx context.Context, rdb *redis.Client, userID int64) {
	if rdb != nil {
		_ = rdb.SRem(ctx, usersKey, strconv.FormatInt(userID, 10)).Err()
	}
}
