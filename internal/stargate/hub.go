package stargate

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/logger"
)

const redisPrefix = "stargate"

func redisChannel(typ, id string) string {
	return redisPrefix + ":" + typ + ":" + id
}

// PresenceNotifier is called when a client connects or disconnects.
// Implementations should update the user's presence in the DB and publish to friends.
type PresenceNotifier interface {
	OnConnect(ctx context.Context, userID int64)
	OnDisconnect(ctx context.Context, userID int64)
}

// ChannelAuthorizer decides whether a user may subscribe to (and therefore send
// into) a "space" channel - a room ID for PMs/group PMs, or a room ID for a space
// text/voice room. "user" channels never reach this: a client may only ever
// subscribe to its own user ID, which the Hub checks directly.
type ChannelAuthorizer interface {
	CanAccessRoom(ctx context.Context, userID int64, roomID string) (bool, error)
}

const authzTimeout = 5 * time.Second

// presenceGrace delays marking a user offline after their last socket drops, so a brief
// disconnect (a backgrounded mobile browser, a network blip) that reconnects right away
// doesn't flicker them offline for everyone who can see them.
const presenceGrace = 10 * time.Second

type HubConfig struct {
	Redis            *redis.Client
	Region           string
	PresenceNotifier PresenceNotifier  // optional; if set, called on connect/disconnect
	Authorizer       ChannelAuthorizer // required to allow "space" subscriptions; nil denies all of them
}

func NewHub(redis *redis.Client, region string) *Hub {
	return NewHubWithConfig(HubConfig{Redis: redis, Region: region})
}

func NewHubWithConfig(cfg HubConfig) *Hub {
	if cfg.Region == "" {
		cfg.Region = "default"
	}
	h := &Hub{
		region:         cfg.Region,
		redis:          cfg.Redis,
		presenceNotify: cfg.PresenceNotifier,
		authorizer:     cfg.Authorizer,
		clients:        make(map[*Client]struct{}),
		subs:           make(map[string]map[*Client]struct{}),
		pendingOffline: make(map[int64]int64),
	}
	return h
}

type Hub struct {
	region         string
	redis          *redis.Client
	presenceNotify PresenceNotifier
	authorizer     ChannelAuthorizer
	clients        map[*Client]struct{}
	subs           map[string]map[*Client]struct{} // channel -> clients
	subMu          sync.RWMutex
	regMu          sync.RWMutex
	// pendingOffline holds the generation of the latest scheduled offline per user (grace
	// period); a reconnect deletes the entry to cancel it. Guarded by regMu.
	pendingOffline map[int64]int64
	offlineGen     int64
}

// authorizeSubscribe reports whether userID may subscribe to (and send into) typ:id.
// "user" channels carry private per-user events (relationship requests, new PMs,
// read receipts, true presence) so a client may only ever subscribe to its own.
// "space" channels carry room/PM events, gated on room participation or space
// membership via the injected ChannelAuthorizer.
func (h *Hub) authorizeSubscribe(ctx context.Context, userID int64, typ, id string) bool {
	switch typ {
	case "user":
		return id == strconv.FormatInt(userID, 10)
	case "space":
		if h.authorizer == nil {
			logger.Warn("stargate", "no ChannelAuthorizer configured; denying space subscription for user_id=%d", userID)
			return false
		}
		ctx, cancel := context.WithTimeout(ctx, authzTimeout)
		defer cancel()
		ok, err := h.authorizer.CanAccessRoom(ctx, userID, id)
		if err != nil {
			logger.Err("stargate", err, map[string]any{"user_id": userID, "room_id": id})
			return false
		}
		return ok
	default:
		return false
	}
}

func (h *Hub) Run(ctx context.Context) {
	// Start Redis subscriber goroutine - we need to subscribe to channels dynamically
	go h.redisReceiver(ctx)
}

func (h *Hub) redisReceiver(ctx context.Context) {
	pubsub := h.redis.PSubscribe(ctx, redisPrefix+":*")
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			h.handleRedisMessage(msg)
		}
	}
}

// handleRedisMessage processes a message from Redis and fans out to local subscribers
func (h *Hub) handleRedisMessage(msg *redis.Message) {
	var env RedisEnvelope
	if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
		logger.Warn("stargate", "invalid redis message: %v", err)
		return
	}

	// Don't echo back to sender if from same region

	var channel string
	if env.SpaceID != "" {
		channel = redisChannel("space", env.SpaceID)
	} else if env.UserID != "" {
		channel = redisChannel("user", env.UserID)
	} else {
		return
	}

	event := EventPayload{
		Type:    env.Type,
		SpaceID: env.SpaceID,
		RoomID:  env.SpaceID, // room_id alias for room/PM events
		UserID:  env.UserID,
		Data:    env.Data,
		Origin:  env.Origin,
		Region:  env.Region,
	}

	h.subMu.RLock()
	clients, ok := h.subs[channel]
	h.subMu.RUnlock()

	if !ok || len(clients) == 0 {
		return
	}

	raw, _ := json.Marshal(map[string]interface{}{
		"op": OpEvent,
		"t":  event.Type,
		"d":  event,
	})

	h.subMu.RLock()
	for c := range clients {
		select {
		case c.send <- raw:
		default:
			// drop if buffer full
		}
	}
	h.subMu.RUnlock()

	// A revoked session (the account was banned, or signed out everywhere) must not keep
	// a live socket: the API already refuses its token, but this connection was
	// authenticated once at upgrade and would otherwise stay subscribed until it dropped
	// on its own. Deliver the event first so the client can say why, then close.
	if env.Type == "SESSION_REVOKED" && env.UserID != "" {
		if uid, err := strconv.ParseInt(env.UserID, 10, 64); err == nil {
			time.AfterFunc(500*time.Millisecond, func() { h.disconnectUser(uid) })
		}
	}
}

// disconnectUser closes every socket a user holds; readPump's exit does the unregister.
func (h *Hub) disconnectUser(userID int64) {
	h.regMu.RLock()
	var targets []*Client
	for c := range h.clients {
		if c.userID == userID {
			targets = append(targets, c)
		}
	}
	h.regMu.RUnlock()
	for _, c := range targets {
		_ = c.conn.Close()
	}
	if len(targets) > 0 {
		logger.Info("stargate", "closed %d connection(s) for revoked user_id=%d", len(targets), userID)
	}
}

// hasOtherClientLocked returns true if there is another connected client for the same user.
// Caller must hold h.regMu.
func (h *Hub) hasOtherClientLocked(userID int64, exclude *Client) bool {
	for cl := range h.clients {
		if cl != exclude && cl.userID == userID {
			return true
		}
	}
	return false
}

// hasAnyClientLocked reports whether the user has at least one connected client.
func (h *Hub) hasAnyClientLocked(userID int64) bool {
	for cl := range h.clients {
		if cl.userID == userID {
			return true
		}
	}
	return false
}

func (h *Hub) register(c *Client) {
	h.regMu.Lock()
	had := h.hasAnyClientLocked(c.userID)
	h.clients[c] = struct{}{}
	_, wasPending := h.pendingOffline[c.userID]
	delete(h.pendingOffline, c.userID) // a reconnect cancels a pending offline
	h.regMu.Unlock()
	logger.Info("stargate", "client connected: user_id=%d", c.userID)
	// OnConnect only on a genuine offline->online transition: not for a second device (the
	// user is already online) and not for a reconnect within the grace window (they never
	// actually went offline, so no PRESENCE_UPDATE is needed).
	if h.presenceNotify != nil && !had && !wasPending {
		go h.presenceNotify.OnConnect(context.Background(), c.userID)
	}
}

func (h *Hub) unregister(c *Client) {
	userID := c.userID
	h.regMu.Lock()
	delete(h.clients, c)
	// Only schedule offline when no other clients for this user remain.
	hasOther := h.hasOtherClientLocked(userID, c)
	var gen int64 = -1
	if h.presenceNotify != nil && !hasOther {
		h.offlineGen++
		gen = h.offlineGen
		h.pendingOffline[userID] = gen
	}
	h.regMu.Unlock()

	// Grace period before going offline (see presenceGrace). A reconnect within the window
	// deletes the map entry in register(); a newer disconnect bumps the generation - either
	// way this timer then no-ops, so only the last disconnect that is still pending fires.
	if gen >= 0 {
		time.AfterFunc(presenceGrace, func() {
			h.regMu.Lock()
			cur, ok := h.pendingOffline[userID]
			act := ok && cur == gen && !h.hasAnyClientLocked(userID)
			if act {
				delete(h.pendingOffline, userID)
			}
			h.regMu.Unlock()
			if act {
				h.presenceNotify.OnDisconnect(context.Background(), userID)
			}
		})
	}

	// Unsubscribe from all Redis channels this client was in
	for _, sub := range c.listSubs() {
		// sub is "space:123" or "user:456" -> redis channel "stargate:space:123"
		ch := redisPrefix + ":" + sub
		h.subMu.Lock()
		if m, ok := h.subs[ch]; ok {
			delete(m, c)
			if len(m) == 0 {
				delete(h.subs, ch)
			}
		}
		h.subMu.Unlock()
	}

	close(c.send)
	logger.Info("stargate", "client disconnected: user_id=%d", c.userID)
}

func (h *Hub) subscribe(c *Client, typ, id string) {
	ch := redisChannel(typ, id)
	h.subMu.Lock()
	if h.subs[ch] == nil {
		h.subs[ch] = make(map[*Client]struct{})
	}
	h.subs[ch][c] = struct{}{}
	h.subMu.Unlock()
}

func (h *Hub) unsubscribe(c *Client, typ, id string) {
	ch := redisChannel(typ, id)
	h.subMu.Lock()
	if m, ok := h.subs[ch]; ok {
		delete(m, c)
		if len(m) == 0 {
			delete(h.subs, ch)
		}
	}
	h.subMu.Unlock()
}

func (h *Hub) publish(c *Client, typ, id string, payload map[string]interface{}) {
	env := RedisEnvelope{
		Type:    "MESSAGE",
		SpaceID: id,
		From:    c.userID,
		Data:    payload,
		Origin:  "",
		Region:  h.region,
	}
	if typ == "user" {
		env.SpaceID = ""
		env.UserID = id
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return
	}

	ch := redisChannel(typ, id)
	if err := h.redis.Publish(context.Background(), ch, raw).Err(); err != nil {
		logger.Err("stargate", err, map[string]any{"channel": ch})
	}
}
