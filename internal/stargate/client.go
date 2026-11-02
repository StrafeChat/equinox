package stargate

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
)

// reservedEventTypes cannot be spoofed via client Send (op 2): these names are
// only ever authoritative when published by messages.Service after the message
// is persisted via REST. Relaying a client-supplied one live would show other
// participants a "message" that doesn't actually exist - it vanishes on reload.
var reservedEventTypes = map[string]struct{}{
	"message":        {},
	"message_create": {},
	"message_update": {},
	"message_delete": {},
}

const (
	readWait   = 60 * time.Second
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10

	// maxSubsPerClient bounds the hub's per-connection bookkeeping; a client that needs
	// more than this is subscribing to things it cannot possibly be showing.
	maxSubsPerClient = 5000
	// Client-originated publishes (op 2) are relayed to everyone in the room without
	// touching the database, so they are the cheapest way to spam a room; allow a burst
	// then a steady trickle.
	sendBurst          = 20
	sendRefillInterval = 250 * time.Millisecond
)

type Client struct {
	hub       *Hub
	conn      Conn
	userID    int64
	sessionID int64
	send      chan []byte
	subs      map[string]struct{} // "space:123" or "user:456"
	// scopes says what a subscribed "space" channel is (by channel id), for op 2 sends;
	// a channel subscribed without one is missing. Guarded by mu, like subs.
	scopes map[string]Scope
	// granted stamps a "space" channel the server granted (grant.go) with the grant's
	// sequence number until a check that read after it has settled it, so an older pass
	// cannot drop it. gone is set once unregister starts, after which nothing is granted.
	// Both guarded by mu.
	granted map[string]uint64
	gone    bool
	mu      sync.RWMutex

	// connID names this one socket (several share a session: browser tabs, and every bot
	// connection has session 0). It keys the connection's activity entry.
	connID int64

	// token bucket for op 2 sends; only touched from readPump.
	sendTokens   int
	sendRefilled time.Time
	// token bucket for op 1 subscribes that need authorizing; only touched from readPump.
	subTokens   int
	subRefilled time.Time

	// act is op 9's state (activity.go), shared by readPump and the connection's worker.
	act clientActivity
}

// Each new subscription costs the authorizer several reads, so a client gets a burst and
// a steady trickle after it; re-subscribing to channels it already holds is free.
const (
	subBurst          = 60
	subRefillInterval = 100 * time.Millisecond
)

func newClient(hub *Hub, conn Conn, userID, sessionID int64) *Client {
	return &Client{
		hub:          hub,
		conn:         conn,
		userID:       userID,
		sessionID:    sessionID,
		connID:       id.Next(),
		send:         make(chan []byte, 512),
		subs:         make(map[string]struct{}),
		sendTokens:   sendBurst,
		sendRefilled: time.Now(),
	}
}

func (c *Client) subKey(typ, id string) string { return typ + ":" + id }

func (c *Client) subscribe(typ, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.subKey(typ, id)
	if _, ok := c.subs[k]; ok {
		return false
	}
	if len(c.subs) >= maxSubsPerClient {
		return false
	}
	c.subs[k] = struct{}{}
	return true
}

// allowSubscribe is allowSend's bucket for subscriptions that reach the authorizer.
func (c *Client) allowSubscribe() bool {
	now := time.Now()
	if c.subRefilled.IsZero() {
		c.subTokens, c.subRefilled = subBurst, now
	}
	if refill := int(now.Sub(c.subRefilled) / subRefillInterval); refill > 0 {
		c.subTokens += refill
		if c.subTokens > subBurst {
			c.subTokens = subBurst
		}
		c.subRefilled = now
	}
	if c.subTokens <= 0 {
		return false
	}
	c.subTokens--
	return true
}

// allowSend refills the op 2 token bucket and takes one token if available.
func (c *Client) allowSend() bool {
	now := time.Now()
	if refill := int(now.Sub(c.sendRefilled) / sendRefillInterval); refill > 0 {
		c.sendTokens += refill
		if c.sendTokens > sendBurst {
			c.sendTokens = sendBurst
		}
		c.sendRefilled = now
	}
	if c.sendTokens <= 0 {
		return false
	}
	c.sendTokens--
	return true
}

func (c *Client) unsubscribe(typ, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.subKey(typ, id)
	if _, ok := c.subs[k]; !ok {
		return false
	}
	delete(c.subs, k)
	if typ == "space" {
		delete(c.scopes, id)
		delete(c.granted, id)
	}
	return true
}

// join subscribes the client and adds it to the hub's fan-out in one critical section, as
// grantTo and revoke do: done in two steps, a drop of the same channel landing between them
// left the socket in the hub's map but not the client's, so unregister missed it and the next
// event on that channel was sent on the closed send channel. Nothing is added once gone.
func (c *Client) join(typ, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.subKey(typ, id)
	if c.gone || len(c.subs) >= maxSubsPerClient {
		return false
	}
	if _, ok := c.subs[k]; ok {
		return false
	}
	c.subs[k] = struct{}{}
	c.hub.subscribe(c, typ, id)
	return true
}

// leave is join's opposite, under the same lock.
func (c *Client) leave(typ, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.subKey(typ, id)
	if _, ok := c.subs[k]; !ok {
		return false
	}
	delete(c.subs, k)
	if typ == "space" {
		delete(c.scopes, id)
		delete(c.granted, id)
	}
	c.hub.unsubscribe(c, typ, id)
	return true
}

// recordScope notes what a "space" channel the client holds is; nothing for one it does not
// hold (an unsubscribe that won the race).
func (c *Client) recordScope(id string, sc Scope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.subs[c.subKey("space", id)]; !ok {
		return
	}
	if c.scopes == nil {
		c.scopes = make(map[string]Scope)
	}
	c.scopes[id] = Scope{SpaceID: sc.SpaceID, RoomID: sc.RoomID}
}

// scopeOf is what recordScope noted for a "space" channel.
func (c *Client) scopeOf(id string) (Scope, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sc, ok := c.scopes[id]
	return sc, ok
}

func (c *Client) hasSub(typ, id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.subs[c.subKey(typ, id)]
	return ok
}

func (c *Client) listSubs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.subs))
	for k := range c.subs {
		out = append(out, k)
	}
	return out
}

func (c *Client) readPump(ctx context.Context) {
	defer func() {
		c.hub.unregister(c)
		_ = c.conn.Close()
	}()

	c.conn.SetReadLimit(64 * 1024) // 64KB
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		c.conn.SetReadDeadline(time.Now().Add(readWait))

		var msg ClientMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.sendError(4000, "invalid json")
			continue
		}

		switch msg.Op {
		case OpHeartbeat, OpPing:
			c.sendOp(OpPong, nil)
		case OpSubscribe:
			c.handleSubscribe(msg.D)
		case OpUnsubscribe:
			c.handleUnsubscribe(msg.D)
		case OpSend:
			c.handleSend(msg.D)
		case OpActivity:
			c.handleActivity(raw)
		default:
			c.sendError(4001, "unknown op")
		}
	}
}

func (c *Client) handleSubscribe(d interface{}) {
	m, ok := d.(map[string]interface{})
	if !ok {
		c.sendError(4002, "invalid subscribe payload")
		return
	}
	// Check hasSub before hitting the (DB-backed) authorizer: clients resubscribe to
	// their whole room/space list on every local list update, and a channel already
	// granted this session doesn't need re-authorizing on every duplicate request.
	if sid, ok := m["space_id"].(string); ok && sid != "" && !c.hasSub("space", sid) {
		if !c.allowSubscribe() {
			c.sendError(4029, "rate limited")
		} else if allowed, sc := c.hub.authorizeSubscribe(context.Background(), c.userID, "space", sid); !allowed {
			c.sendError(4403, "not authorized to subscribe to this channel")
		} else if c.join("space", sid) {
			if sc != nil {
				c.recordScope(sid, *sc)
			}
		}
	}
	if uid, ok := m["user_id"].(string); ok && uid != "" && !c.hasSub("user", uid) {
		if allowed, _ := c.hub.authorizeSubscribe(context.Background(), c.userID, "user", uid); !allowed {
			c.sendError(4403, "not authorized to subscribe to this channel")
		} else {
			c.join("user", uid)
		}
	}
}

func (c *Client) handleUnsubscribe(d interface{}) {
	m, ok := d.(map[string]interface{})
	if !ok {
		return
	}
	if sid, ok := m["space_id"].(string); ok && sid != "" {
		c.leave("space", sid)
	}
	if uid, ok := m["user_id"].(string); ok && uid != "" {
		c.leave("user", uid)
	}
}

func (c *Client) handleSend(d interface{}) {
	payload, ok := d.(map[string]interface{})
	if !ok {
		c.sendError(4003, "invalid send payload")
		return
	}
	spaceID, _ := payload["space_id"].(string)
	if spaceID == "" {
		c.sendError(4004, "space_id required")
		return
	}
	if !c.hasSub("space", spaceID) {
		c.sendError(4005, "not subscribed to space")
		return
	}
	if typ, _ := payload["type"].(string); typ != "" {
		if _, reserved := reservedEventTypes[strings.ToLower(typ)]; reserved {
			c.sendError(4006, "event type is server-reserved")
			return
		}
	}
	if !c.allowSend() {
		c.sendError(4029, "rate limited")
		return
	}

	c.hub.publish(c, "space", spaceID, payload)
}

func (c *Client) sendOp(op int, d interface{}) {
	msg := map[string]interface{}{"op": op}
	if d != nil {
		msg["d"] = d
	}
	raw, _ := json.Marshal(msg)
	select {
	case c.send <- raw:
	default:
		logger.Warn("stargate", "client send buffer full, dropping: user_id=%d", c.userID)
	}
}

func (c *Client) sendError(code int, message string) {
	c.sendOp(OpError, ErrorPayload{Code: code, Message: message})
}

func (c *Client) writePump(ctx context.Context) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				_ = c.conn.WriteMessage(CloseMessage, []byte{})
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}
