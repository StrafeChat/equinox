package voice

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store keeps voice states and calls in Redis. They are live, in-memory facts about
// running connections - nothing here needs to outlive the media it describes, and
// every process that shares the Redis (API replicas, the gateway building READY)
// sees the same picture.
//
// Keys (under the configured cache prefix):
//
//	voice:room:<room>   hash  user id -> State JSON
//	voice:user:<user>   the room the user is in
//	voice:rooms         set of rooms that have any state, or a call
//	voice:call:<room>   Call JSON for a PM / group PM call
//	voice:modflags:<space>  hash  user id -> server mute/deafen bits a space keeps on a member
//	voice:sync:lock:<origin>    one API replica is asking that origin (see mirror.go)
//	voice:sync:retry:<origin>   it failed less than a minute ago
//	voice:sync:down:<origin>    when it stopped answering
//	voice:sync:legacy:<origin>  it predates the states endpoint
type Store struct {
	rdb    *redis.Client
	prefix string
}

func NewStore(rdb *redis.Client, prefix string) *Store {
	return &Store{rdb: rdb, prefix: prefix}
}

func (s *Store) roomKey(roomID int64) string {
	return s.prefix + "voice:room:" + strconv.FormatInt(roomID, 10)
}
func (s *Store) userKey(userID int64) string {
	return s.prefix + "voice:user:" + strconv.FormatInt(userID, 10)
}
func (s *Store) callKey(roomID int64) string {
	return s.prefix + "voice:call:" + strconv.FormatInt(roomID, 10)
}
func (s *Store) roomsKey() string { return s.prefix + "voice:rooms" }
func (s *Store) syncKey(what, origin string) string {
	return s.prefix + "voice:sync:" + what + ":" + origin
}
func (s *Store) modFlagsKey(spaceID int64) string {
	return s.prefix + "voice:modflags:" + strconv.FormatInt(spaceID, 10)
}

// ModFlags returns the server mute and deafen a space keeps on a member between voice
// connections. They live on the member, not the connection, as on Discord: a moderator
// lifts them, leaving and rejoining does not.
func (s *Store) ModFlags(ctx context.Context, spaceID, userID int64) (mute, deaf bool, err error) {
	v, err := s.rdb.HGet(ctx, s.modFlagsKey(spaceID), strconv.FormatInt(userID, 10)).Result()
	if errors.Is(err, redis.Nil) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	n, _ := strconv.Atoi(v)
	return n&1 != 0, n&2 != 0, nil
}

// SetModFlags records (or, with both false, forgets) a member's server mute and deafen.
func (s *Store) SetModFlags(ctx context.Context, spaceID, userID int64, mute, deaf bool) error {
	field := strconv.FormatInt(userID, 10)
	if !mute && !deaf {
		return s.rdb.HDel(ctx, s.modFlagsKey(spaceID), field).Err()
	}
	n := 0
	if mute {
		n |= 1
	}
	if deaf {
		n |= 2
	}
	return s.rdb.HSet(ctx, s.modFlagsKey(spaceID), field, strconv.Itoa(n)).Err()
}

// GetState returns the user's state in the room, or nil.
func (s *Store) GetState(ctx context.Context, roomID, userID int64) (*State, error) {
	raw, err := s.rdb.HGet(ctx, s.roomKey(roomID), strconv.FormatInt(userID, 10)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// PutState writes the state and points the user at its room.
func (s *Store) PutState(ctx context.Context, st *State) error {
	return s.putState(ctx, st, true)
}

// PutCopy writes a mirrored state without moving the user's pointer, for a user whose pointer
// names a room another instance's relays may not end (see mirrorState).
func (s *Store) PutCopy(ctx context.Context, st *State) error {
	return s.putState(ctx, st, false)
}

func (s *Store) putState(ctx context.Context, st *State, point bool) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.HSet(ctx, s.roomKey(st.RoomID), strconv.FormatInt(st.UserID, 10), raw)
	if point {
		pipe.Set(ctx, s.userKey(st.UserID), strconv.FormatInt(st.RoomID, 10), 0)
	}
	pipe.SAdd(ctx, s.roomsKey(), strconv.FormatInt(st.RoomID, 10))
	_, err = pipe.Exec(ctx)
	return err
}

// DeleteState removes the user's state from the room. The user pointer is only cleared
// when it still points at this room, so a leave that races a join elsewhere does not
// erase the new pointer.
func (s *Store) DeleteState(ctx context.Context, roomID, userID int64) error {
	pipe := s.rdb.TxPipeline()
	pipe.HDel(ctx, s.roomKey(roomID), strconv.FormatInt(userID, 10))
	if cur, ok, _ := s.UserRoom(ctx, userID); ok && cur == roomID {
		pipe.Del(ctx, s.userKey(userID))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	n, err := s.rdb.HLen(ctx, s.roomKey(roomID)).Result()
	if err == nil && n == 0 {
		// A call with nobody in it stays listed: it ends a moment later here, and on a mirror
		// this is how a call whose CALL_DELETE was lost keeps being asked about.
		if calls, err := s.rdb.Exists(ctx, s.callKey(roomID)).Result(); err == nil && calls == 0 {
			_ = s.UntrackRoom(ctx, roomID)
		}
	}
	return nil
}

// TrackRoom lists the room as active whether or not it holds a state.
func (s *Store) TrackRoom(ctx context.Context, roomID int64) error {
	return s.rdb.SAdd(ctx, s.roomsKey(), strconv.FormatInt(roomID, 10)).Err()
}

// UntrackRoom stops listing the room as active.
func (s *Store) UntrackRoom(ctx context.Context, roomID int64) error {
	return s.rdb.SRem(ctx, s.roomsKey(), strconv.FormatInt(roomID, 10)).Err()
}

// UserRoom returns the room the user is connected to, if any.
func (s *Store) UserRoom(ctx context.Context, userID int64) (int64, bool, error) {
	raw, err := s.rdb.Get(ctx, s.userKey(userID)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	rid, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, nil
	}
	return rid, true, nil
}

// RoomStates lists everyone in the room.
func (s *Store) RoomStates(ctx context.Context, roomID int64) ([]State, error) {
	m, err := s.rdb.HGetAll(ctx, s.roomKey(roomID)).Result()
	if err != nil {
		return nil, err
	}
	return decodeStates(m), nil
}

func decodeStates(m map[string]string) []State {
	out := make([]State, 0, len(m))
	for _, raw := range m {
		var st State
		if json.Unmarshal([]byte(raw), &st) == nil {
			out = append(out, st)
		}
	}
	return out
}

// ClearRoom drops every state in the room and returns what was there.
func (s *Store) ClearRoom(ctx context.Context, roomID int64) ([]State, error) {
	states, err := s.RoomStates(ctx, roomID)
	if err != nil {
		return nil, err
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, s.roomKey(roomID))
	pipe.SRem(ctx, s.roomsKey(), strconv.FormatInt(roomID, 10))
	for _, st := range states {
		if cur, ok, _ := s.UserRoom(ctx, st.UserID); ok && cur == roomID {
			pipe.Del(ctx, s.userKey(st.UserID))
		}
	}
	_, err = pipe.Exec(ctx)
	return states, err
}

// ActiveRooms lists rooms with at least one state.
func (s *Store) ActiveRooms(ctx context.Context) ([]int64, error) {
	members, err := s.rdb.SMembers(ctx, s.roomsKey()).Result()
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(members))
	for _, m := range members {
		if rid, err := strconv.ParseInt(m, 10, 64); err == nil {
			out = append(out, rid)
		}
	}
	return out, nil
}

// StatesForRooms returns every state in any of the given rooms, plus the calls running
// in them. Only rooms in the active set are read, so a READY for a user in many spaces
// costs one SMEMBERS and one pipelined read of the few rooms that are actually live.
func (s *Store) StatesForRooms(ctx context.Context, roomIDs []int64) ([]State, []Call, error) {
	active, err := s.ActiveRooms(ctx)
	if err != nil {
		return nil, nil, err
	}
	activeSet := make(map[int64]struct{}, len(active))
	for _, rid := range active {
		activeSet[rid] = struct{}{}
	}
	wanted := make([]int64, 0)
	for _, rid := range roomIDs {
		if _, ok := activeSet[rid]; ok {
			wanted = append(wanted, rid)
		}
	}
	if len(wanted) == 0 {
		return nil, nil, nil
	}
	pipe := s.rdb.Pipeline()
	stateCmds := make([]*redis.MapStringStringCmd, len(wanted))
	callCmds := make([]*redis.StringCmd, len(wanted))
	for i, rid := range wanted {
		stateCmds[i] = pipe.HGetAll(ctx, s.roomKey(rid))
		callCmds[i] = pipe.Get(ctx, s.callKey(rid))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, nil, err
	}
	var states []State
	var calls []Call
	for i := range wanted {
		if m, err := stateCmds[i].Result(); err == nil {
			states = append(states, decodeStates(m)...)
		}
		if raw, err := callCmds[i].Bytes(); err == nil {
			var c Call
			if json.Unmarshal(raw, &c) == nil {
				calls = append(calls, c)
			}
		}
	}
	return states, calls, nil
}

// claimSync takes a short lock: every API replica sweeps, and one of them asks each origin.
func (s *Store) claimSync(ctx context.Context, origin string, ttl time.Duration) bool {
	ok, err := s.rdb.SetNX(ctx, s.syncKey("lock", origin), "1", ttl).Result()
	return err == nil && ok
}

// syncHeldOff reports whether an origin is not to be asked for now: it failed moments ago,
// or it is too old to answer.
func (s *Store) syncHeldOff(ctx context.Context, origin string) bool {
	n, err := s.rdb.Exists(ctx, s.syncKey("retry", origin), s.syncKey("legacy", origin)).Result()
	return err != nil || n > 0
}

// markOriginLegacy remembers an origin with no states endpoint for ttl.
func (s *Store) markOriginLegacy(ctx context.Context, origin string, ttl time.Duration) error {
	return s.rdb.Set(ctx, s.syncKey("legacy", origin), "1", ttl).Err()
}

// markOriginDown records one more failure and returns when the origin first stopped
// answering. The key outlives a failure by downFor, so it is gone once nothing asks any more
// and an old outage is never taken for the start of the next one.
func (s *Store) markOriginDown(ctx context.Context, origin string, now time.Time, retryAfter, downFor time.Duration) (time.Time, error) {
	if err := s.rdb.Set(ctx, s.syncKey("retry", origin), "1", retryAfter).Err(); err != nil {
		return now, err
	}
	key := s.syncKey("down", origin)
	if err := s.rdb.SetNX(ctx, key, now.UTC().Format(time.RFC3339Nano), downFor).Err(); err != nil {
		return now, err
	}
	_ = s.rdb.Expire(ctx, key, downFor).Err()
	raw, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		return now, err
	}
	since, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return now, nil
	}
	return since, nil
}

// markOriginUp forgets an outage.
func (s *Store) markOriginUp(ctx context.Context, origin string) {
	_ = s.rdb.Del(ctx, s.syncKey("down", origin)).Err()
}

func (s *Store) GetCall(ctx context.Context, roomID int64) (*Call, error) {
	raw, err := s.rdb.Get(ctx, s.callKey(roomID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Call
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) PutCall(ctx context.Context, c *Call) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, s.callKey(c.RoomID), raw, 0).Err()
}

func (s *Store) DeleteCall(ctx context.Context, roomID int64) error {
	return s.rdb.Del(ctx, s.callKey(roomID)).Err()
}
