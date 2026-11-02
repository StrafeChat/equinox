// Package redistest is go-redis with an in-memory map behind it, for tests of code that keeps
// state in Redis. It works as a client hook, so nothing dials, and it holds the commands the
// voice store and the gateway publisher use: a test that reaches for another one is told so.
package redistest

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

// Event is one gateway event as it was published. UserID / SpaceID are the channel it went
// out on (a user's, or a room's or space's); the Scope fields what it is about.
type Event struct {
	Type    string
	UserID  string
	SpaceID string
	Data    map[string]any

	Scoped       bool
	ScopeSpaceID string
	ScopeRoomID  string
	Subscribe    []string
}

// Redis is the memory behind a client from New.
type Redis struct {
	mu     sync.Mutex
	strs   map[string]string
	hashes map[string]map[string]string
	sets   map[string]map[string]bool
	stream map[string][]string
	events []Event
}

// New returns a client backed by nothing but memory.
func New() (*redis.Client, *Redis) {
	r := &Redis{strs: map[string]string{}, hashes: map[string]map[string]string{}, sets: map[string]map[string]bool{}, stream: map[string][]string{}}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rdb.AddHook(r)
	return rdb, r
}

// Published is every event published so far, in order.
func (r *Redis) Published() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// AddToSet adds members to a set.
func (r *Redis) AddToSet(key string, members ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sets[key] == nil {
		r.sets[key] = map[string]bool{}
	}
	for _, m := range members {
		r.sets[key][m] = true
	}
}

// Stream is the last field value of each entry added to a stream, in order.
func (r *Redis) Stream(key string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stream[key]...)
}

// HasString reports whether a string key is set.
func (r *Redis) HasString(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.strs[key]
	return ok
}

// SetString sets a string key.
func (r *Redis) SetString(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.strs[key] = value
}

// DeleteString deletes string keys, as if they had expired.
func (r *Redis) DeleteString(keys ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		delete(r.strs, k)
	}
}

func (r *Redis) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *Redis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.run(cmd)
		return cmd.Err()
	}
}

func (r *Redis) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, cmds []redis.Cmder) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, cmd := range cmds {
			r.run(cmd)
		}
		return nil
	}
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func (r *Redis) run(cmd redis.Cmder) {
	args := cmd.Args()
	key := ""
	if len(args) > 1 {
		key = text(args[1])
	}
	switch strings.ToLower(cmd.Name()) {
	case "multi", "exec":
	case "hget":
		v, ok := r.hashes[key][text(args[2])]
		if !ok {
			cmd.SetErr(redis.Nil)
			return
		}
		cmd.(*redis.StringCmd).SetVal(v)
	case "hset":
		if r.hashes[key] == nil {
			r.hashes[key] = map[string]string{}
		}
		for i := 2; i+1 < len(args); i += 2 {
			r.hashes[key][text(args[i])] = text(args[i+1])
		}
		cmd.(*redis.IntCmd).SetVal(1)
	case "hdel":
		for _, f := range args[2:] {
			delete(r.hashes[key], text(f))
		}
		if len(r.hashes[key]) == 0 {
			delete(r.hashes, key)
		}
		cmd.(*redis.IntCmd).SetVal(1)
	case "hgetall":
		out := map[string]string{}
		for k, v := range r.hashes[key] {
			out[k] = v
		}
		cmd.(*redis.MapStringStringCmd).SetVal(out)
	case "hlen":
		cmd.(*redis.IntCmd).SetVal(int64(len(r.hashes[key])))
	case "get":
		v, ok := r.strs[key]
		if !ok {
			cmd.SetErr(redis.Nil)
			return
		}
		cmd.(*redis.StringCmd).SetVal(v)
	case "set", "setnx":
		_, taken := r.strs[key]
		if b, ok := cmd.(*redis.BoolCmd); ok {
			if !taken {
				r.strs[key] = text(args[2])
			}
			b.SetVal(!taken)
			return
		}
		r.strs[key] = text(args[2])
		cmd.(*redis.StatusCmd).SetVal("OK")
	case "del":
		var n int64
		for _, k := range args[1:] {
			_, isStr := r.strs[text(k)]
			_, isHash := r.hashes[text(k)]
			if isStr || isHash {
				n++
			}
			delete(r.strs, text(k))
			delete(r.hashes, text(k))
		}
		cmd.(*redis.IntCmd).SetVal(n)
	case "exists":
		var n int64
		for _, k := range args[1:] {
			if _, ok := r.strs[text(k)]; ok {
				n++
			}
		}
		cmd.(*redis.IntCmd).SetVal(n)
	case "expire":
		cmd.(*redis.BoolCmd).SetVal(true)
	case "sadd":
		if r.sets[key] == nil {
			r.sets[key] = map[string]bool{}
		}
		for _, v := range args[2:] {
			r.sets[key][text(v)] = true
		}
		cmd.(*redis.IntCmd).SetVal(1)
	case "srem":
		for _, v := range args[2:] {
			delete(r.sets[key], text(v))
		}
		cmd.(*redis.IntCmd).SetVal(1)
	case "smembers":
		out := make([]string, 0, len(r.sets[key]))
		for v := range r.sets[key] {
			out = append(out, v)
		}
		sort.Strings(out)
		cmd.(*redis.StringSliceCmd).SetVal(out)
	case "smismember":
		out := make([]bool, 0, len(args)-2)
		for _, v := range args[2:] {
			out = append(out, r.sets[key][text(v)])
		}
		cmd.(*redis.BoolSliceCmd).SetVal(out)
	case "xadd":
		r.stream[key] = append(r.stream[key], text(args[len(args)-1]))
		cmd.(*redis.StringCmd).SetVal("0-0")
	case "publish":
		var env struct {
			Type         string         `json:"t"`
			Data         map[string]any `json:"d"`
			UserID       string         `json:"user_id"`
			SpaceID      string         `json:"space_id"`
			Scoped       bool           `json:"scoped"`
			ScopeSpaceID string         `json:"scope_space_id"`
			ScopeRoomID  string         `json:"scope_room_id"`
			Subscribe    []string       `json:"subscribe"`
		}
		if err := json.Unmarshal([]byte(text(args[2])), &env); err != nil {
			cmd.SetErr(err)
			return
		}
		r.events = append(r.events, Event{
			Type: env.Type, UserID: env.UserID, SpaceID: env.SpaceID, Data: env.Data,
			Scoped: env.Scoped, ScopeSpaceID: env.ScopeSpaceID, ScopeRoomID: env.ScopeRoomID, Subscribe: env.Subscribe,
		})
		cmd.(*redis.IntCmd).SetVal(1)
	default:
		cmd.SetErr(errors.New("redistest: " + cmd.Name() + " is not implemented"))
	}
}
