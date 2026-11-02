package stargate

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The gateway's side of a grant: an event that gives its recipients a channel subscribes the
// sockets it reaches as it is delivered, and a grant on a user's own channel is then checked.

// grantHub is a hub over f with one socket of user 7 holding its own channel.
func grantHub(t *testing.T, f *reauthFake) (*Hub, *Client, *redis.Client, *rawPubs) {
	t.Helper()
	rdb, pubs := rawRedis(t)
	h, c := scopeHub(t, rdb, f)
	return h, c, rdb, pubs
}

// addSocket gives the hub another socket of a user, holding its own channel and the given
// "space" channels.
func addSocket(h *Hub, userID int64, channels ...string) *Client {
	c := newClient(h, nil, userID, 1)
	h.regMu.Lock()
	h.clients[c] = struct{}{}
	h.regMu.Unlock()
	uid := strconv.FormatInt(userID, 10)
	c.subscribe("user", uid)
	h.subscribe(c, "user", uid)
	for _, ch := range channels {
		c.subscribe("space", ch)
		h.subscribe(c, "space", ch)
	}
	return c
}

// handleAll hands everything published since the last take to the hub, in order, and drains
// the frames the given sockets got from it.
func handleAll(h *Hub, pubs *rawPubs, sockets ...*Client) {
	for _, m := range pubs.take() {
		h.handleRedisMessage(m)
	}
	for _, c := range sockets {
		drain(c)
	}
}

func drain(c *Client) {
	for {
		select {
		case <-c.send:
		default:
			return
		}
	}
}

// waitChecks waits until no check of the user's is running or queued.
func waitChecks(t *testing.T, h *Hub, userID int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.authBusy(userID) {
		if time.Now().After(deadline) {
			t.Fatal("the checks never finished")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// heldBy reports whether c holds a "space" channel, in both the client's and the hub's books.
func heldBy(h *Hub, c *Client, ch string) bool {
	h.subMu.RLock()
	_, inHub := h.subs[redisChannel("space", ch)][c]
	h.subMu.RUnlock()
	inClient := c.hasSub("space", ch)
	if inHub != inClient {
		panic("client and hub disagree about space:" + ch)
	}
	return inClient
}

func newFake(answers map[string]error) *reauthFake {
	if answers == nil {
		answers = map[string]error{}
	}
	return &reauthFake{answers: answers, calls: map[string]int{}}
}

// SPACE_CREATE on the user's own channel subscribes every socket of theirs to the space and
// its rooms before the next message is read, so a message published right after it arrives -
// while the check of the grant is still running. Nobody else's sockets are touched, none of
// the client's op 1 tokens are spent, and an op 2 send on the room carries its scope.
func TestGrantOnOwnChannel(t *testing.T) {
	f := newFake(nil)
	f.hold = make(chan struct{})
	h, c, rdb, pubs := grantHub(t, f)
	other := addSocket(h, 7)
	stranger := addSocket(h, 8)
	ctx := context.Background()
	space := int64(100)

	PublishToUserIn(ctx, rdb, 7, InSpace(100).Subscribing(100, 300), "SPACE_CREATE", map[string]interface{}{"id": "100"}, "")
	PublishToRoom(ctx, rdb, InRoom(&space, 300), "MESSAGE_CREATE", map[string]interface{}{"id": "1"}, "")
	msgs := pubs.take()
	if len(msgs) != 2 {
		t.Fatalf("published %v", msgs)
	}
	deliver(t, h, c, msgs[0])
	drain(other)
	for _, sock := range []*Client{c, other} {
		for _, ch := range []string{"100", "300"} {
			if !heldBy(h, sock, ch) {
				t.Errorf("a socket of the user does not hold space:%s", ch)
			}
		}
	}
	if heldBy(h, stranger, "300") || heldBy(h, stranger, "100") {
		t.Error("another user's socket was subscribed")
	}
	if f := deliver(t, h, c, msgs[1]); f.d["id"] != "1" {
		t.Errorf("message frame %v", f.d)
	}
	if !c.subRefilled.IsZero() {
		t.Error("the grant spent op 1 tokens")
	}
	if sc, ok := c.scopeOf("300"); !ok || sc.SpaceID != 100 || sc.RoomID != 300 {
		t.Errorf("room scope %+v %v", sc, ok)
	}
	if sc, ok := c.scopeOf("100"); !ok || sc.SpaceID != 100 || sc.RoomID != 0 {
		t.Errorf("space scope %+v %v", sc, ok)
	}

	close(f.hold)
	waitChecks(t, h, 7)
	if !heldBy(h, c, "300") || !heldBy(h, other, "300") {
		t.Error("an allowed grant was dropped")
	}
	if f.count("300") != 1 || f.count("100") != 1 {
		t.Errorf("checks: room %d, space %d; want one each for the user", f.count("300"), f.count("100"))
	}
}

// A room announced on a space's channel subscribes the space's subscribers, and only them. The
// publisher sends a room space-wide only when everyone in the space may view it, so nobody is
// checked one by one.
func TestGrantOnSpaceChannel(t *testing.T) {
	f := newFake(nil)
	h, c, rdb, pubs := grantHub(t, f)
	c.subscribe("space", "100")
	h.subscribe(c, "space", "100")
	member := addSocket(h, 8, "100")
	outsider := addSocket(h, 9)

	PublishToSpaceChannel(context.Background(), rdb, InSpace(100).About(300).Subscribing(300), "SPACE_ROOM_CREATE", map[string]interface{}{"id": "300"}, "")
	handleAll(h, pubs, c, member, outsider)
	if !heldBy(h, c, "300") || !heldBy(h, member, "300") {
		t.Error("a member of the space was not subscribed")
	}
	if heldBy(h, outsider, "300") {
		t.Error("a socket not on the space's channel was subscribed")
	}
	if h.authBusy(7) || h.authBusy(8) || f.count("300") != 0 {
		t.Errorf("a space-wide grant was checked (%d checks)", f.count("300"))
	}
}

// A client sending into a channel (op 2) can never grant one.
func TestClientRelayGrantsNothing(t *testing.T) {
	h, c, _, _ := grantHub(t, newFake(nil))
	raw := `{"t":"MESSAGE","space_id":"","user_id":"7","from":7,"d":{},"subscribe":["300"]}`
	h.handleRedisMessage(&redis.Message{Channel: "stargate:user:7", Payload: raw})
	drain(c)
	if heldBy(h, c, "300") {
		t.Fatal("a client-originated envelope granted a channel")
	}
}

// A socket at maxSubsPerClient is not subscribed past it.
func TestGrantRespectsTheCap(t *testing.T) {
	h, c, rdb, pubs := grantHub(t, newFake(nil))
	for i := len(c.listSubs()); i < maxSubsPerClient; i++ {
		ch := strconv.Itoa(1_000_000 + i)
		c.subscribe("space", ch)
		h.subscribe(c, "space", ch)
	}
	PublishToUserIn(context.Background(), rdb, 7, InRoom(nil, 300).Subscribing(300), "ROOM_CREATE", map[string]interface{}{"id": "300"}, "")
	handleAll(h, pubs, c)
	waitChecks(t, h, 7)
	if heldBy(h, c, "300") || len(c.listSubs()) != maxSubsPerClient {
		t.Fatalf("subscribed past the cap: %d subscriptions", len(c.listSubs()))
	}
}

// A granted channel is an ordinary subscription afterwards: op 5 ends it.
func TestGrantedChannelUnsubscribes(t *testing.T) {
	h, c, rdb, pubs := grantHub(t, newFake(nil))
	PublishToUserIn(context.Background(), rdb, 7, InRoom(nil, 300).Subscribing(300), "ROOM_CREATE", map[string]interface{}{"id": "300"}, "")
	handleAll(h, pubs, c)
	waitChecks(t, h, 7)
	c.handleUnsubscribe(map[string]interface{}{"space_id": "300"})
	if heldBy(h, c, "300") {
		t.Fatal("op 5 left the granted channel subscribed")
	}
	if _, ok := c.scopeOf("300"); ok {
		t.Error("op 5 left the channel's scope")
	}
}

// The check after a grant drops a channel the authorizer refuses, and keeps one whose check
// failed: a timeout is not a lost permission.
func TestGrantCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer error
		kept   bool
	}{
		{"refused", errRefused, false},
		{"failed read", errors.New("timeout"), true},
		{"allowed", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(map[string]error{"300": tc.answer})
			h, c, rdb, pubs := grantHub(t, f)
			PublishToUserIn(context.Background(), rdb, 7, InRoom(nil, 300).Subscribing(300), "ROOM_CREATE", map[string]interface{}{"id": "300"}, "")
			handleAll(h, pubs, c)
			waitChecks(t, h, 7)
			if got := heldBy(h, c, "300"); got != tc.kept {
				t.Fatalf("held %v, want %v", got, tc.kept)
			}
			if f.count("300") != 1 {
				t.Errorf("%d checks, want 1", f.count("300"))
			}
		})
	}
}

// A grant is no exemption from a later loss: SPACE_LEAVE re-checks everything the user holds.
func TestLossAfterGrantStillRemovesIt(t *testing.T) {
	f := newFake(nil)
	h, c, rdb, pubs := grantHub(t, f)
	ctx := context.Background()
	PublishToUserIn(ctx, rdb, 7, InSpace(100).Subscribing(100, 300), "SPACE_CREATE", map[string]interface{}{"id": "100"}, "")
	handleAll(h, pubs, c)
	waitChecks(t, h, 7)
	if !heldBy(h, c, "300") {
		t.Fatal("the grant did not subscribe")
	}
	f.mu.Lock()
	f.answers["100"], f.answers["300"] = errRefused, errRefused
	f.mu.Unlock()
	PublishToUserIn(ctx, rdb, 7, InSpace(100), "SPACE_LEAVE", map[string]interface{}{"id": "100"}, "")
	handleAll(h, pubs, c)
	waitChecks(t, h, 7)
	if heldBy(h, c, "300") || heldBy(h, c, "100") {
		t.Fatal("the space's channels outlived leaving it")
	}
}

// A user who left and rejoined at once: the check queued by the leave may read the state
// between the two writes. It must not drop the channel the rejoin granted again; the grant's
// own check, which follows it, decides.
func TestOlderCheckKeepsANewerGrant(t *testing.T) {
	f := newFake(map[string]error{"300": errRefused})
	f.hold = make(chan struct{})
	h, c, rdb, pubs := grantHub(t, f)
	c.subscribe("space", "300")
	h.subscribe(c, "space", "300")
	ctx := context.Background()

	// The leave: its check reads "refused" and is held there.
	PublishToUserIn(ctx, rdb, 7, InSpace(100).About(300), "SPACE_ROOM_DELETE", map[string]interface{}{"room_id": "300"}, "")
	handleAll(h, pubs, c)
	deadline := time.Now().Add(2 * time.Second)
	for f.count("300") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the leave's check never started")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The regain lands while that check is out; the state now allows the room.
	f.mu.Lock()
	f.answers["300"] = nil
	f.mu.Unlock()
	PublishToUserIn(ctx, rdb, 7, InSpace(100).About(300).Subscribing(300), "SPACE_ROOM_CREATE", map[string]interface{}{"room_id": "300"}, "")
	handleAll(h, pubs, c)
	close(f.hold)
	waitChecks(t, h, 7)
	if !heldBy(h, c, "300") {
		t.Fatal("a check older than the grant dropped it")
	}
	if n := f.count("300"); n != 2 {
		t.Errorf("%d checks, want 2 (the leave's, then the grant's)", n)
	}
}

// A socket that is going away is never subscribed again, so unregister leaves nothing behind.
func TestNoGrantAfterUnregister(t *testing.T) {
	h, c, _, _ := grantHub(t, newFake(nil))
	c.mu.Lock()
	c.gone = true
	c.mu.Unlock()
	h.grant(&RedisEnvelope{Scoped: true, Subscribe: []string{"300"}}, []*Client{c}, true)
	h.subMu.RLock()
	_, inHub := h.subs[redisChannel("space", "300")]
	h.subMu.RUnlock()
	if c.hasSub("space", "300") || inHub {
		t.Fatal("a closing socket was subscribed")
	}
}
