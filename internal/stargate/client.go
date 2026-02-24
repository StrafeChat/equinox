package stargate

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
)

const (
	readWait   = 60 * time.Second
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

type Client struct {
	hub       *Hub
	conn      Conn
	userID    int64
	sessionID int64
	send      chan []byte
	subs      map[string]struct{} // "space:123" or "user:456"
	mu        sync.RWMutex
}

func newClient(hub *Hub, conn Conn, userID, sessionID int64) *Client {
	return &Client{
		hub:       hub,
		conn:      conn,
		userID:    userID,
		sessionID: sessionID,
		send:      make(chan []byte, 512),
		subs:      make(map[string]struct{}),
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
	c.subs[k] = struct{}{}
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
	return true
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
	if sid, ok := m["space_id"].(string); ok && sid != "" {
		if c.subscribe("space", sid) {
			c.hub.subscribe(c, "space", sid)
		}
	}
	if uid, ok := m["user_id"].(string); ok && uid != "" {
		if c.subscribe("user", uid) {
			c.hub.subscribe(c, "user", uid)
		}
	}
}

func (c *Client) handleUnsubscribe(d interface{}) {
	m, ok := d.(map[string]interface{})
	if !ok {
		return
	}
	if sid, ok := m["space_id"].(string); ok && sid != "" {
		if c.unsubscribe("space", sid) {
			c.hub.unsubscribe(c, "space", sid)
		}
	}
	if uid, ok := m["user_id"].(string); ok && uid != "" {
		if c.unsubscribe("user", uid) {
			c.hub.unsubscribe(c, "user", uid)
		}
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
