package stargate

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/safego"
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

// ChannelScoper is optionally implemented by a ChannelAuthorizer: CanAccessChannel is
// CanAccessRoom that also says what the channel is - a room and its space (none for a PM or
// group PM), or a space's own channel - from the same reads. A subscription records it, so a
// client's op 2 sends, which name only a channel, carry the right space_id and room_id. An
// authorizer without it leaves those sends unscoped.
type ChannelScoper interface {
	CanAccessChannel(ctx context.Context, userID int64, channelID string) (bool, Scope, error)
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
		region:            cfg.Region,
		redis:             cfg.Redis,
		presenceNotify:    cfg.PresenceNotifier,
		authorizer:        cfg.Authorizer,
		clients:           make(map[*Client]struct{}),
		subs:              make(map[string]map[*Client]struct{}),
		pendingOffline:    make(map[int64]int64),
		grace:             presenceGrace,
		activityRefill:    activityRefillInterval,
		activityRetry:     activityRetryDelay,
		activityKeepalive: activityKeepaliveInterval,
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

	// grace is presenceGrace: how long a user's last socket is gone before they go offline,
	// and how long a closed connection's activity outlives it, so a reconnect flickers
	// neither. activityRefill is op 9's token interval, activityRetry the wait before a
	// failed activity write is tried again, activityKeepalive how often a held activity is
	// renewed. All are fields only so tests can shorten them.
	grace             time.Duration
	activityRefill    time.Duration
	activityRetry     time.Duration
	activityKeepalive time.Duration

	// reauthing holds the users with an authorization pass running, and what arrived during
	// it for one more pass to check (see queueAuth). Guarded by reauthMu.
	reauthMu  sync.Mutex
	reauthing map[int64]*authWork
	// grantSeq numbers the server's grants (grant.go); a pass notes it before it reads.
	grantSeq atomic.Uint64
}

// authorizeSubscribe reports whether userID may subscribe to (and send into) typ:id.
// "user" channels carry private per-user events (relationship requests, new PMs,
// read receipts, true presence) so a client may only ever subscribe to its own.
// "space" channels carry room/PM events, gated on room participation or space
// membership via the injected ChannelAuthorizer, which may also say what the channel is
// (the scope; nil when it cannot - see ChannelScoper).
func (h *Hub) authorizeSubscribe(ctx context.Context, userID int64, typ, id string) (bool, *Scope) {
	ok, sc, err := h.checkSubscribeScope(ctx, userID, typ, id)
	if err != nil {
		logger.Err("stargate", err, map[string]any{"user_id": userID, "room_id": id})
		return false, nil
	}
	return ok, sc
}

// checkSubscribe is authorizeSubscribe with a failed check told apart from a refusal: a new
// subscription fails closed on either, but one already held is dropped only on a refusal.
func (h *Hub) checkSubscribe(ctx context.Context, userID int64, typ, id string) (bool, error) {
	ok, _, err := h.checkSubscribeScope(ctx, userID, typ, id)
	return ok, err
}

// checkSubscribeScope is checkSubscribe that also returns what an allowed "space" channel is,
// when the authorizer is a ChannelScoper (nil otherwise).
func (h *Hub) checkSubscribeScope(ctx context.Context, userID int64, typ, id string) (bool, *Scope, error) {
	switch typ {
	case "user":
		return id == strconv.FormatInt(userID, 10), nil, nil
	case "space":
		if h.authorizer == nil {
			logger.Warn("stargate", "no ChannelAuthorizer configured; denying space subscription for user_id=%d", userID)
			return false, nil, nil
		}
		ctx, cancel := context.WithTimeout(ctx, authzTimeout)
		defer cancel()
		if scoper, ok := h.authorizer.(ChannelScoper); ok {
			allowed, sc, err := scoper.CanAccessChannel(ctx, userID, id)
			if err != nil || !allowed {
				return false, nil, err
			}
			return true, &sc, nil
		}
		ok, err := h.authorizer.CanAccessRoom(ctx, userID, id)
		return ok, nil, err
	default:
		return false, nil, nil
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

	// env.SpaceID / env.UserID are the routing channel (see RedisEnvelope), never the ids the
	// frame carries.
	var channel string
	if env.SpaceID != "" {
		channel = redisChannel("space", env.SpaceID)
	} else if env.UserID != "" {
		channel = redisChannel("user", env.UserID)
	} else {
		return
	}

	event := EventPayload{
		Type:   env.Type,
		UserID: env.UserID,
		Data:   env.Data,
		Origin: env.Origin,
		Region: env.Region,
	}
	if env.Scoped {
		event.SpaceID, event.RoomID = env.ScopeSpaceID, env.ScopeRoomID
	} else {
		// A publisher that predates scopes (an API binary one release behind, mid-upgrade)
		// names only its channel; both ids carry that, as they always did.
		event.SpaceID, event.RoomID = env.SpaceID, env.SpaceID
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

	// Only the server grants channels; a client's op 2 relay never carries a list.
	granting := env.From == 0 && len(env.Subscribe) > 0
	var recipients []*Client
	h.subMu.RLock()
	for c := range clients {
		select {
		case c.send <- raw:
		default:
			// drop if buffer full
		}
		if granting {
			recipients = append(recipients, c)
		}
	}
	h.subMu.RUnlock()
	// Before the next message is read, so nothing published after the grant can miss them.
	if granting {
		h.grant(&env, recipients, env.SpaceID == "")
	}

	// A revoked session (the account was banned, or signed out everywhere) must not keep
	// a live socket: the API already refuses its token, but this connection was
	// authenticated once at upgrade and would otherwise stay subscribed until it dropped
	// on its own. Deliver the event first so the client can say why, then close.
	if env.Type == "SESSION_REVOKED" && env.UserID != "" {
		if uid, err := strconv.ParseInt(env.UserID, 10, 64); err == nil {
			time.AfterFunc(500*time.Millisecond, func() { h.disconnectUser(uid) })
		}
	}
	// A subscription is authorized once, when it is made; membership is what authorized a
	// space or room, so losing it must end the subscription too, or a kicked or banned
	// member keeps streaming the room. The API tells the user themselves about every such
	// loss on their own channel: SPACE_LEAVE (kick, ban, leave), ROOM_LEAVE (removed from a
	// group PM) and SPACE_ROOM_DELETE (an override or sync change took their View away).
	if env.UserID != "" && (env.Type == "SPACE_LEAVE" || env.Type == "ROOM_LEAVE" || env.Type == "SPACE_ROOM_DELETE") {
		if uid, err := strconv.ParseInt(env.UserID, 10, 64); err == nil {
			h.queueReauthorize(uid)
		}
	}
}

// authWork is what arrived for a user while a pass of theirs was running: all asks for every
// subscription to be re-checked (a loss), grants for just those channels (the server granted
// them; grant.go).
type authWork struct {
	all    bool
	grants map[string]struct{}
}

// queueReauthorize re-checks every subscription a user's sockets hold (reauthorizeUser).
func (h *Hub) queueReauthorize(userID int64) {
	h.queueAuth(userID, true, nil)
}

// queueAuth runs a user's checks at most one pass at a time: a role change can send a member
// a SPACE_ROOM_DELETE for several rooms at once, and each pass re-checks every subscription
// they hold, so events that arrive during a pass share one more pass. Grants and losses go
// through the same worker, in order, so a check of a grant never races a check of a loss.
func (h *Hub) queueAuth(userID int64, all bool, grants []string) {
	h.reauthMu.Lock()
	if w, running := h.reauthing[userID]; running {
		w.add(all, grants)
		h.reauthMu.Unlock()
		return
	}
	if h.reauthing == nil {
		h.reauthing = map[int64]*authWork{}
	}
	pending := &authWork{} // what arrives while this worker runs
	h.reauthing[userID] = pending
	next := &authWork{}
	next.add(all, grants)
	h.reauthMu.Unlock()
	safego.Go("stargate", func() {
		defer func() {
			// After a panic too, or the user's checks would never run again; never a newer
			// worker's entry.
			h.reauthMu.Lock()
			if h.reauthing[userID] == pending {
				delete(h.reauthing, userID)
			}
			h.reauthMu.Unlock()
		}()
		for {
			h.authorizeHeld(userID, next.all, next.grants)
			h.reauthMu.Lock()
			if !pending.all && len(pending.grants) == 0 {
				delete(h.reauthing, userID)
				h.reauthMu.Unlock()
				return
			}
			next = &authWork{all: pending.all, grants: pending.grants}
			pending.all, pending.grants = false, nil
			h.reauthMu.Unlock()
		}
	})
}

func (w *authWork) add(all bool, grants []string) {
	w.all = w.all || all
	for _, id := range grants {
		if w.grants == nil {
			w.grants = make(map[string]struct{}, len(grants))
		}
		w.grants[id] = struct{}{}
	}
}

// authBusy reports whether a pass for the user is running or queued.
func (h *Hub) authBusy(userID int64) bool {
	h.reauthMu.Lock()
	defer h.reauthMu.Unlock()
	_, busy := h.reauthing[userID]
	return busy
}

// reauthorizeUser re-runs the authorizer over every "space:" subscription the user's local
// sockets hold and drops the ones it now refuses.
func (h *Hub) reauthorizeUser(userID int64) {
	h.authorizeHeld(userID, true, nil)
}

// authorizeHeld checks the "space:" subscriptions the user's local sockets hold - every one
// when all is set, otherwise just the granted ones - and drops those the authorizer now
// refuses. Each distinct id is checked once, and one whose check fails rather than refuses is
// kept: a timeout is not a lost permission. A channel granted again after this pass began is
// never dropped by it (its read may predate the grant); the grant's own check follows.
func (h *Hub) authorizeHeld(userID int64, all bool, grants map[string]struct{}) {
	since := h.grantSeq.Load()
	h.regMu.RLock()
	var targets []*Client
	for c := range h.clients {
		if c.userID == userID {
			targets = append(targets, c)
		}
	}
	h.regMu.RUnlock()
	if len(targets) == 0 {
		return
	}
	type verdict struct {
		allowed bool
		scope   *Scope
	}
	ctx := context.Background()
	verdicts := map[string]verdict{}
	dropped := 0
	for _, c := range targets {
		for _, k := range c.listSubs() {
			typ, id, ok := strings.Cut(k, ":")
			if !ok || typ != "space" {
				continue
			}
			if _, granted := grants[id]; !all && !granted {
				continue
			}
			v, seen := verdicts[id]
			if !seen {
				ok, sc, err := h.checkSubscribeScope(ctx, userID, "space", id)
				if err != nil {
					logger.Warn("stargate", "kept subscription %s for user_id=%d: check failed: %v", id, userID, err)
				}
				v = verdict{allowed: ok || err != nil, scope: sc}
				verdicts[id] = v
			}
			if v.allowed {
				c.settleGrant(id, since, v.scope)
				continue
			}
			if h.revoke(c, id, since) {
				dropped++
			}
		}
	}
	if dropped > 0 {
		logger.Info("stargate", "dropped %d subscription(s) user_id=%d no longer holds", dropped, userID)
	}
}

// connectionsFor counts a user's live sockets on this gateway.
func (h *Hub) connectionsFor(userID int64) int {
	h.regMu.RLock()
	defer h.regMu.RUnlock()
	n := 0
	for c := range h.clients {
		if c.userID == userID {
			n++
		}
	}
	return n
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
	userID, connID := c.userID, c.connID
	// From here on nothing is written for the connection's activity. dirty: the store may
	// still hold an entry for it, which goes after the same grace a status gets, so a
	// socket that reconnects right away (and re-reports) never shows the game ending.
	dirty := c.closeActivity()
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

	// The user keeps another socket (or nothing here tracks presence): the entry is cleared
	// on its own timer, and the change published to the user's other sockets' audience.
	if dirty && gen < 0 {
		h.scheduleActivityCleanup(userID, connID)
	}

	// Grace period before going offline (see presenceGrace). A reconnect within the window
	// deletes the map entry in register(); a newer disconnect bumps the generation - either
	// way this timer then no-ops, so only the last disconnect that is still pending fires.
	if gen >= 0 {
		time.AfterFunc(h.grace, func() {
			h.regMu.Lock()
			cur, ok := h.pendingOffline[userID]
			act := ok && cur == gen && !h.hasAnyClientLocked(userID)
			if act {
				delete(h.pendingOffline, userID)
			}
			h.regMu.Unlock()
			if dirty {
				if act {
					// Going offline: the entry goes first, unannounced. The offline below
					// carries no activity, and an activity publish built from the user
					// record - still online until OnDisconnect writes it - could land after
					// that offline, here or on another instance, and show them online again.
					safego.Run("stargate", func() { h.clearConnActivity(userID, connID, 1) })
				} else {
					// Back (or gone again, with a newer timer pending): an ordinary cleanup,
					// which publishes only while the user has a socket.
					h.activityCleanupAfter(0, userID, connID, 1)
				}
			}
			if act {
				h.presenceNotify.OnDisconnect(context.Background(), userID)
			}
		})
	}

	// Unsubscribe from all Redis channels this client was in. Marked gone first, so a grant
	// on the receive loop either lands in this list or is not made at all.
	c.mu.Lock()
	c.gone = true
	c.mu.Unlock()
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
	} else if sc, ok := c.scopeOf(id); ok {
		// The client names a channel; what it is was recorded when the subscription was
		// authorized. A channel subscribed without one goes out unscoped.
		env.Scoped = true
		if sc.SpaceID != 0 {
			env.ScopeSpaceID = strconv.FormatInt(sc.SpaceID, 10)
		}
		if sc.RoomID != 0 {
			env.ScopeRoomID = strconv.FormatInt(sc.RoomID, 10)
		}
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
