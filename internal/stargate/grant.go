package stargate

import "strconv"

// Granting: an event that gives its recipients a channel (SPACE_CREATE, ROOM_CREATE,
// SPACE_ROOM_CREATE, THREAD_CREATE, being added to a thread) names it in the envelope's
// Subscribe, and the gateway subscribes every socket the event reached as it delivers it.
// Without this a socket only ever gains a channel through op 1, after its client has heard of
// the channel and asked - and everything published in between (the join notice, the new
// member's SPACE_MEMBER_ADD, a bot's role update) never reaches it, while a bot that does not
// subscribe by hand hears nothing from a space it was installed into until it reconnects.
//
// The subscription is made on the receive loop, before the next message is read, so no event
// published after the grant is missed; it does no I/O there. A grant on a user's own channel
// is checked against the authorizer afterwards, off the loop, through the same per-user worker
// as reauthorization (queueAuth), so a grant and a loss are checked in the order they came.

// grant subscribes the sockets that received env to the channels it names. onUserChannel says
// env came on one user's own channel: those sockets are that user's, and each new channel is
// checked for them afterwards. On a space's channel the publisher sent the event space-wide
// only because everyone in the space may view the room, so its members are not re-checked one
// by one - unless a check of theirs is already running, whose read may predate the grant.
func (h *Hub) grant(env *RedisEnvelope, recipients []*Client, onUserChannel bool) {
	seq := h.grantSeq.Add(1)
	byUser := map[int64][]string{}
	busy := map[int64]bool{}
	for _, c := range recipients {
		b, seen := busy[c.userID]
		if !seen {
			b = h.authBusy(c.userID)
			busy[c.userID] = b
		}
		for _, ch := range env.Subscribe {
			if ch == "" {
				continue
			}
			added, held := h.grantTo(c, ch, grantScope(env, ch), seq)
			// What needs a check: a channel just added to a user's own sockets, and anything a
			// running check might drop on a read older than this grant.
			if (added && onUserChannel) || (held && b) {
				byUser[c.userID] = append(byUser[c.userID], ch)
			}
		}
	}
	for uid, ids := range byUser {
		h.queueAuth(uid, false, ids)
	}
}

// grantTo subscribes c to a "space" channel unless it holds it already, is at
// maxSubsPerClient, or is going away, and stamps it with the grant's seq either way (in the
// same critical section, so a running pass can never drop it unstamped). It spends none of
// the client's op 1 tokens. added says it subscribed, held that c holds the channel now.
func (h *Hub) grantTo(c *Client, ch string, sc *Scope, seq uint64) (added, held bool) {
	k := c.subKey("space", ch)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return false, false
	}
	if _, ok := c.subs[k]; !ok {
		if len(c.subs) >= maxSubsPerClient {
			return false, false
		}
		added = true
	}
	if c.granted == nil {
		c.granted = make(map[string]uint64)
	}
	c.granted[ch] = seq
	if !added {
		return false, true
	}
	c.subs[k] = struct{}{}
	if sc != nil {
		if c.scopes == nil {
			c.scopes = make(map[string]Scope)
		}
		c.scopes[ch] = *sc
	}
	// Under c.mu, like revoke, so a grant and a drop of the same channel never leave the two
	// maps disagreeing.
	h.subscribe(c, "space", ch)
	return true, true
}

// revoke drops c's subscription to a "space" channel that a pass starting at grant sequence
// since found refused, unless it was granted after since.
func (h *Hub) revoke(c *Client, ch string, since uint64) bool {
	k := c.subKey("space", ch)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.subs[k]; !ok {
		return false
	}
	if seq, ok := c.granted[ch]; ok && seq > since {
		return false
	}
	delete(c.subs, k)
	delete(c.scopes, ch)
	delete(c.granted, ch)
	h.unsubscribe(c, "space", ch)
	return true
}

// settleGrant records what an allowed channel is, and clears its grant stamp when the pass
// that allowed it (starting at since) read after the grant.
func (c *Client) settleGrant(ch string, since uint64, sc *Scope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.subs[c.subKey("space", ch)]; !ok {
		return
	}
	if seq, ok := c.granted[ch]; ok && seq <= since {
		delete(c.granted, ch)
	}
	if sc != nil {
		if c.scopes == nil {
			c.scopes = make(map[string]Scope)
		}
		c.scopes[ch] = Scope{SpaceID: sc.SpaceID, RoomID: sc.RoomID}
	}
}

// grantScope is what a granted channel is, from the event that granted it: the event's own
// space, and the channel as its room unless it is that space's own channel. Nil for an
// unscoped envelope, which says nothing about it.
func grantScope(env *RedisEnvelope, ch string) *Scope {
	if !env.Scoped {
		return nil
	}
	var sc Scope
	if env.ScopeSpaceID != "" {
		sid, err := strconv.ParseInt(env.ScopeSpaceID, 10, 64)
		if err != nil {
			return nil
		}
		sc.SpaceID = sid
	}
	if ch != env.ScopeSpaceID {
		rid, err := strconv.ParseInt(ch, 10, 64)
		if err != nil {
			return nil
		}
		sc.RoomID = rid
	}
	return &sc
}
