package stargate

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// The activity store: what each user is doing right now ("Playing X"), in Redis only.
//
//   - <prefix>presence:activity:<user_id> is a HASH with one field per gateway connection
//     that reported an activity (the Client's connID, never the session id: every bot
//     connection has session 0 and browser tabs share one). Written only by the gateway,
//     EXPIREd 12 h after every write as a crash net, and emptied at gateway start through
//     the index below.
//   - <prefix>presence:activity:users is the index of user ids with such a hash.
//   - <prefix>presence:activity:fed:<user_id> is a STRING: the activity a remote user's home
//     instance relayed (POST /federation/v1/users/presence), written by the API process,
//     SET with a 12 h expiry and deleted on a clear. It is not in the index, so a gateway
//     restart does not wipe remote users' activities.
//
// Each value is JSON {"at", "type", "name", "started_at"} with "at" (the server's receive
// time, unix ms) first. A user's effective activity - the one every surface shows - is the
// entry with the greatest "at" (ties: the greater field name, the federated entry being
// field "fed"). Writes go through one Lua script that also reads the effective activity
// before and after, so two connections writing at once cannot both miss a change.

const (
	// activityTTL is how long an entry outlives its last write if nothing removes it.
	activityTTL = 12 * time.Hour
	// activityFedField is the federated entry's name in the effective-activity tie-break.
	activityFedField = "fed"
)

// ActivityStore keeps the activities connections and peers report. Every method is safe
// for concurrent use; changed reports whether the user's effective activity (type, name,
// started_at) differs after the write.
type ActivityStore interface {
	// SetConn stores the activity one gateway connection reports.
	SetConn(ctx context.Context, userID, connID int64, a auth.Activity, at int64) (changed bool, err error)
	// ClearConn removes a connection's entry (op 9 clear, or the socket gone for good).
	ClearConn(ctx context.Context, userID, connID int64) (changed bool, err error)
	// SetFederated stores (a != nil) or clears (a == nil) a remote user's relayed activity.
	SetFederated(ctx context.Context, userID int64, a *auth.Activity, at int64) (changed bool, err error)
	// Effective reads the effective activities of many users in one round trip. Users
	// with none are absent from the map.
	Effective(ctx context.Context, userIDs []int64) (map[int64][]auth.Activity, error)
}

// storedActivity is one entry's value. Field order matters: the Lua script finds "at" by
// the value's prefix.
type storedActivity struct {
	At        int64  `json:"at"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	StartedAt int64  `json:"started_at"`
}

func (v storedActivity) activity() auth.Activity {
	return auth.Activity{Type: v.Type, Name: v.Name, StartedAt: v.StartedAt}
}

func encodeActivity(a auth.Activity, at int64) string {
	raw, _ := json.Marshal(storedActivity{At: at, Type: a.Type, Name: a.Name, StartedAt: a.StartedAt})
	return string(raw)
}

func decodeActivity(raw string) (storedActivity, bool) {
	var v storedActivity
	if raw == "" || json.Unmarshal([]byte(raw), &v) != nil || v.At < 0 || v.Type == "" {
		return storedActivity{}, false
	}
	return v, true
}

// pickEffective is the effective-activity rule, shared by the batch read and the memory
// store; the Lua script implements the same one.
func pickEffective(fields map[string]string, fed string) *auth.Activity {
	var best *storedActivity
	bestField := ""
	consider := func(field, raw string) {
		v, ok := decodeActivity(raw)
		if !ok {
			return
		}
		if best == nil || v.At > best.At || (v.At == best.At && field > bestField) {
			best, bestField = &v, field
		}
	}
	for f, raw := range fields {
		consider(f, raw)
	}
	consider(activityFedField, fed)
	if best == nil {
		return nil
	}
	a := best.activity()
	return &a
}

func activityList(a *auth.Activity) []auth.Activity {
	if a == nil {
		return nil
	}
	return []auth.Activity{*a}
}

// ---- Redis -----------------------------------------------------------------------------

// RedisActivityStore is the ActivityStore both binaries use.
type RedisActivityStore struct {
	rdb    *redis.Client
	prefix string
}

// NewActivityStore keys the store with the deployment's REDIS_CACHE_PREFIX, so two
// instances sharing one Redis database do not see each other's activities.
func NewActivityStore(rdb *redis.Client, prefix string) *RedisActivityStore {
	return &RedisActivityStore{rdb: rdb, prefix: prefix}
}

var errNoActivityRedis = errors.New("activity store has no redis client")

func (s *RedisActivityStore) connKey(userID int64) string {
	return s.prefix + "presence:activity:" + id.Format(userID)
}

func (s *RedisActivityStore) fedKey(userID int64) string {
	return s.prefix + "presence:activity:fed:" + id.Format(userID)
}

func (s *RedisActivityStore) indexKey() string {
	return s.prefix + "presence:activity:users"
}

// activityWriteScript applies one write and returns the effective entry (its raw value,
// "" for none) before and after it.
//
// KEYS: 1 the user's connection hash, 2 the user's federated string, 3 the index set.
// ARGV: 1 op (hset | hdel | fset | fdel), 2 field, 3 value, 4 ttl seconds, 5 user id.
var activityWriteScript = redis.NewScript(activityWriteLua)

const activityWriteLua = `
local function at_of(v)
  if type(v) ~= 'string' then return nil end
  local n = string.match(v, '^{"at":(%d+),')
  if n then return tonumber(n) end
  return nil
end
local function effective()
  local best, best_at, best_field = false, -1, ''
  local h = redis.call('HGETALL', KEYS[1])
  for i = 1, #h, 2 do
    local a = at_of(h[i + 1])
    if a and (a > best_at or (a == best_at and h[i] > best_field)) then
      best, best_at, best_field = h[i + 1], a, h[i]
    end
  end
  local f = redis.call('GET', KEYS[2])
  local a = at_of(f)
  if a and (a > best_at or (a == best_at and 'fed' > best_field)) then
    best = f
  end
  return best
end
local before = effective()
local op = ARGV[1]
if op == 'hset' then
  redis.call('HSET', KEYS[1], ARGV[2], ARGV[3])
  redis.call('EXPIRE', KEYS[1], ARGV[4])
  redis.call('SADD', KEYS[3], ARGV[5])
elseif op == 'hdel' then
  redis.call('HDEL', KEYS[1], ARGV[2])
  if redis.call('EXISTS', KEYS[1]) == 0 then
    redis.call('SREM', KEYS[3], ARGV[5])
  end
elseif op == 'fset' then
  redis.call('SET', KEYS[2], ARGV[3], 'EX', ARGV[4])
elseif op == 'fdel' then
  redis.call('DEL', KEYS[2])
end
local after = effective()
return {before or '', after or ''}
`

func (s *RedisActivityStore) write(ctx context.Context, userID int64, op, field, value string) (bool, error) {
	if s == nil || s.rdb == nil {
		return false, errNoActivityRedis
	}
	keys := []string{s.connKey(userID), s.fedKey(userID), s.indexKey()}
	res, err := activityWriteScript.Run(ctx, s.rdb, keys, op, field, value, int64(activityTTL/time.Second), id.Format(userID)).StringSlice()
	if err != nil {
		return false, err
	}
	if len(res) != 2 {
		return false, errors.New("activity script: unexpected reply")
	}
	return !auth.SameActivity(effectiveOf(res[0]), effectiveOf(res[1])), nil
}

func effectiveOf(raw string) *auth.Activity {
	v, ok := decodeActivity(raw)
	if !ok {
		return nil
	}
	a := v.activity()
	return &a
}

func (s *RedisActivityStore) SetConn(ctx context.Context, userID, connID int64, a auth.Activity, at int64) (bool, error) {
	return s.write(ctx, userID, "hset", id.Format(connID), encodeActivity(a, at))
}

func (s *RedisActivityStore) ClearConn(ctx context.Context, userID, connID int64) (bool, error) {
	return s.write(ctx, userID, "hdel", id.Format(connID), "")
}

func (s *RedisActivityStore) SetFederated(ctx context.Context, userID int64, a *auth.Activity, at int64) (bool, error) {
	if a == nil {
		return s.write(ctx, userID, "fdel", "", "")
	}
	return s.write(ctx, userID, "fset", "", encodeActivity(*a, at))
}

func (s *RedisActivityStore) Effective(ctx context.Context, userIDs []int64) (map[int64][]auth.Activity, error) {
	out := map[int64][]auth.Activity{}
	if len(userIDs) == 0 {
		return out, nil
	}
	if s == nil || s.rdb == nil {
		return out, errNoActivityRedis
	}
	type reads struct {
		conns *redis.MapStringStringCmd
		fed   *redis.StringCmd
	}
	seen := make(map[int64]struct{}, len(userIDs))
	ids := make([]int64, 0, len(userIDs))
	for _, uid := range userIDs {
		if _, dup := seen[uid]; !dup {
			seen[uid] = struct{}{}
			ids = append(ids, uid)
		}
	}
	cmds := make([]reads, len(ids))
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, uid := range ids {
			cmds[i] = reads{conns: p.HGetAll(ctx, s.connKey(uid)), fed: p.Get(ctx, s.fedKey(uid))}
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return out, err
	}
	for i, uid := range ids {
		fields, _ := cmds[i].conns.Result()
		fed, _ := cmds[i].fed.Result()
		if a := pickEffective(fields, fed); a != nil {
			out[uid] = []auth.Activity{*a}
		}
	}
	return out, nil
}

// PurgeConnections empties every connection hash, for a gateway that is starting with no
// clients (the same self-heal as the presence:online reset; single-gateway, as that is).
// Federated entries are left alone: the API owns them.
func (s *RedisActivityStore) PurgeConnections(ctx context.Context) error {
	if s == nil || s.rdb == nil {
		return errNoActivityRedis
	}
	members, err := s.rdb.SMembers(ctx, s.indexKey()).Result()
	if err != nil {
		return err
	}
	for start := 0; start < len(members); start += 500 {
		end := min(start+500, len(members))
		keys := make([]string, 0, end-start)
		for _, m := range members[start:end] {
			if uid, err := id.Parse(m); err == nil {
				keys = append(keys, s.connKey(uid))
			}
		}
		if len(keys) > 0 {
			if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
	}
	return s.rdb.Del(ctx, s.indexKey()).Err()
}

// ---- in memory ---------------------------------------------------------------------------

// MemoryActivityStore is an ActivityStore in process memory with the Redis store's rules,
// for tests (in this package and the modules that attach activities to lists) and for a
// process that runs without Redis. It counts Effective calls, so a test can check that a
// list costs one read.
type MemoryActivityStore struct {
	mu    sync.Mutex
	conns map[int64]map[string]string
	fed   map[int64]string
	reads int
}

func NewMemoryActivityStore() *MemoryActivityStore {
	return &MemoryActivityStore{conns: map[int64]map[string]string{}, fed: map[int64]string{}}
}

func (m *MemoryActivityStore) effectiveLocked(userID int64) *auth.Activity {
	return pickEffective(m.conns[userID], m.fed[userID])
}

func (m *MemoryActivityStore) apply(userID int64, write func()) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.effectiveLocked(userID)
	write()
	return !auth.SameActivity(before, m.effectiveLocked(userID))
}

func (m *MemoryActivityStore) SetConn(_ context.Context, userID, connID int64, a auth.Activity, at int64) (bool, error) {
	return m.apply(userID, func() {
		if m.conns[userID] == nil {
			m.conns[userID] = map[string]string{}
		}
		m.conns[userID][id.Format(connID)] = encodeActivity(a, at)
	}), nil
}

func (m *MemoryActivityStore) ClearConn(_ context.Context, userID, connID int64) (bool, error) {
	return m.apply(userID, func() {
		delete(m.conns[userID], id.Format(connID))
		if len(m.conns[userID]) == 0 {
			delete(m.conns, userID)
		}
	}), nil
}

func (m *MemoryActivityStore) SetFederated(_ context.Context, userID int64, a *auth.Activity, at int64) (bool, error) {
	return m.apply(userID, func() {
		if a == nil {
			delete(m.fed, userID)
			return
		}
		m.fed[userID] = encodeActivity(*a, at)
	}), nil
}

func (m *MemoryActivityStore) Effective(_ context.Context, userIDs []int64) (map[int64][]auth.Activity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	out := map[int64][]auth.Activity{}
	for _, uid := range userIDs {
		if a := m.effectiveLocked(uid); a != nil {
			out[uid] = []auth.Activity{*a}
		}
	}
	return out, nil
}

// Reads is how many times Effective was called.
func (m *MemoryActivityStore) Reads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads
}

// Conns is how many connection entries the store holds for a user.
func (m *MemoryActivityStore) Conns(userID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns[userID])
}

// HasFederated reports whether a federated entry is stored for the user.
func (m *MemoryActivityStore) HasFederated(userID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.fed[userID]
	return ok
}

// ---- the process-wide store ----------------------------------------------------------------

var (
	activityMu    sync.RWMutex
	activityStore ActivityStore
)

// SetActivityStore installs the store both binaries read and write through: the gateway
// (op 9, disconnects, READY, PRESENCE_UPDATE) and the API (lists, PATCH status, federation).
// Both must use the same Redis and prefix, or a status change in one would publish without
// the activity the other wrote. nil turns activities off.
func SetActivityStore(s ActivityStore) {
	activityMu.Lock()
	activityStore = s
	activityMu.Unlock()
}

// CurrentActivityStore is the installed store, nil when there is none.
func CurrentActivityStore() ActivityStore {
	activityMu.RLock()
	defer activityMu.RUnlock()
	return activityStore
}

// ActivitiesFor reads many users' effective activities in one round trip. A missing store
// or a Redis failure reads as "nobody is doing anything": an activity is never worth
// failing a list for.
func ActivitiesFor(ctx context.Context, userIDs []int64) map[int64][]auth.Activity {
	st := CurrentActivityStore()
	if st == nil || len(userIDs) == 0 {
		return nil
	}
	out, err := st.Effective(ctx, userIDs)
	if err != nil {
		logger.Warn("stargate", "activity read failed: %v", err)
		return nil
	}
	return out
}

// ActivitiesOf is ActivitiesFor for one user.
func ActivitiesOf(ctx context.Context, userID int64) []auth.Activity {
	return ActivitiesFor(ctx, []int64{userID})[userID]
}

// visibleToOthers reports whether others see the user as anything but offline, the only
// case in which an activity is shown to them.
func visibleToOthers(u *auth.User) bool {
	return u != nil && auth.ToPublicPresence(u.Presence, true).Status != "offline"
}

// VisibleActivities reads, in one round trip, the activities of the users in a list that
// others would see them with - those who do not look offline; everyone else is skipped
// without a read, since WithActivities would drop theirs anyway.
func VisibleActivities(ctx context.Context, users []*auth.User) map[int64][]auth.Activity {
	ids := make([]int64, 0, len(users))
	for _, u := range users {
		if visibleToOthers(u) {
			ids = append(ids, u.ID)
		}
	}
	return ActivitiesFor(ctx, ids)
}

// VisibleActivitiesOf is VisibleActivities for one user (a single member or friend payload).
func VisibleActivitiesOf(ctx context.Context, u *auth.User) []auth.Activity {
	if !visibleToOthers(u) {
		return nil
	}
	return ActivitiesOf(ctx, u.ID)
}
