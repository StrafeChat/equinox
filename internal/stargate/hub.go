package stargate

import (
	"context"
	"encoding/json"
	"sync"

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

type HubConfig struct {
	Redis             *redis.Client
	Region            string
	PresenceNotifier  PresenceNotifier // optional; if set, called on connect/disconnect
}

func NewHub(redis *redis.Client, region string) *Hub {
	return NewHubWithConfig(HubConfig{Redis: redis, Region: region})
}

func NewHubWithConfig(cfg HubConfig) *Hub {
	if cfg.Region == "" {
		cfg.Region = "default"
	}
	h := &Hub{
		region:          cfg.Region,
		redis:           cfg.Redis,
		presenceNotify:  cfg.PresenceNotifier,
		clients:         make(map[*Client]struct{}),
		subs:            make(map[string]map[*Client]struct{}),
	}
	return h
}

type Hub struct {
	region         string
	redis          *redis.Client
	presenceNotify PresenceNotifier
	clients        map[*Client]struct{}
	subs           map[string]map[*Client]struct{} // channel -> clients
	subMu          sync.RWMutex
	regMu          sync.RWMutex
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

func (h *Hub) register(c *Client) {
	h.regMu.Lock()
	h.clients[c] = struct{}{}
	h.regMu.Unlock()
	logger.Info("stargate", "client connected: user_id=%d", c.userID)
	if h.presenceNotify != nil {
		go h.presenceNotify.OnConnect(context.Background(), c.userID)
	}
}

func (h *Hub) unregister(c *Client) {
	userID := c.userID
	h.regMu.Lock()
	delete(h.clients, c)
	// Only notify offline when no other clients for this user remain
	hasOther := h.hasOtherClientLocked(userID, c)
	h.regMu.Unlock()

	if h.presenceNotify != nil && !hasOther {
		go h.presenceNotify.OnDisconnect(context.Background(), userID)
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
