package stargate

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
)

// rawPubs is a go-redis hook that answers every command itself and keeps each PUBLISH as it
// went out, so a test can hand it to the hub exactly as Redis would.
type rawPubs struct {
	mu   sync.Mutex
	msgs []*redis.Message
}

func (r *rawPubs) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *rawPubs) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() != "publish" || len(args) != 3 {
			return nil
		}
		ch, _ := args[1].(string)
		var payload string
		switch v := args[2].(type) {
		case []byte:
			payload = string(v)
		case string:
			payload = v
		}
		r.mu.Lock()
		r.msgs = append(r.msgs, &redis.Message{Channel: ch, Payload: payload})
		r.mu.Unlock()
		return nil
	}
}

func (r *rawPubs) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return nil }
}

// take returns what was published since the last take.
func (r *rawPubs) take() []*redis.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.msgs
	r.msgs = nil
	return out
}

func rawRedis(t *testing.T) (*redis.Client, *rawPubs) {
	t.Helper()
	rec := &rawPubs{}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rdb.AddHook(rec)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, rec
}

// envelope decodes a published message.
func envelope(t *testing.T, m *redis.Message) RedisEnvelope {
	t.Helper()
	var env RedisEnvelope
	if err := json.Unmarshal([]byte(m.Payload), &env); err != nil {
		t.Fatalf("envelope %s: %v", m.Payload, err)
	}
	return env
}

// receivedFrame is an op 3 frame as a client reads it.
type receivedFrame struct {
	raw map[string]any // d, undecoded further, to see which keys are there at all
	d   map[string]any // d.d, the payload
}

func (f receivedFrame) id(key string) (string, bool) {
	v, ok := f.raw[key]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// deliver hands one published message to the hub and returns the frame c got from it.
func deliver(t *testing.T, h *Hub, c *Client, m *redis.Message) receivedFrame {
	t.Helper()
	h.handleRedisMessage(m)
	select {
	case raw := <-c.send:
		var f struct {
			Op int            `json:"op"`
			D  map[string]any `json:"d"`
		}
		if err := json.Unmarshal(raw, &f); err != nil || f.Op != OpEvent {
			t.Fatalf("frame %s: %v", raw, err)
		}
		d, _ := f.D["d"].(map[string]any)
		return receivedFrame{raw: f.D, d: d}
	default:
		t.Fatalf("no frame for %s on %s", m.Payload, m.Channel)
		return receivedFrame{}
	}
}

// scopeHub is a hub with one client of user 7 holding its own channel and the given "space"
// channels.
func scopeHub(t *testing.T, rdb *redis.Client, auth ChannelAuthorizer, channels ...string) (*Hub, *Client) {
	t.Helper()
	h := NewHubWithConfig(HubConfig{Redis: rdb, Authorizer: auth})
	c := newClient(h, nil, 7, 1)
	h.regMu.Lock()
	h.clients[c] = struct{}{}
	h.regMu.Unlock()
	c.subscribe("user", "7")
	h.subscribe(c, "user", "7")
	for _, ch := range channels {
		c.subscribe("space", ch)
		h.subscribe(c, "space", ch)
	}
	return h, c
}

// The frame says what an event is about: space_id the space (none for a PM), room_id the room
// (none for a space-wide event), user_id only on the account's own channel. Routing still
// follows the channel, so a room's events reach the room's subscribers alone.
func TestFrameCarriesTheEventsScope(t *testing.T) {
	ctx := context.Background()
	rdb, pubs := rawRedis(t)
	h, c := scopeHub(t, rdb, nil, "100", "300", "500")
	space := int64(100)

	cases := []struct {
		name                  string
		publish               func()
		channel               string
		space, room, user     string // "" = absent
		payloadSpace, payload string // payload's space_id / room_id; "" = absent
	}{
		{"space room", func() {
			PublishToRoom(ctx, rdb, InRoom(&space, 300), "MESSAGE_CREATE", map[string]interface{}{"id": "1"}, "")
		}, "stargate:space:300", "100", "300", "", "100", "300"},
		{"pm", func() {
			PublishToRoom(ctx, rdb, InRoom(nil, 500), "MESSAGE_CREATE", map[string]interface{}{"id": "2"}, "")
		}, "stargate:space:500", "", "500", "", "", "500"},
		{"space-wide", func() {
			PublishToSpaceChannel(ctx, rdb, InSpace(100), "SPACE_UPDATE", map[string]interface{}{"id": "100"}, "")
		}, "stargate:space:100", "100", "", "", "100", ""},
		{"space channel, about a room", func() {
			PublishToSpaceChannel(ctx, rdb, InSpace(100).About(300), "SPACE_ROOM_CREATE", map[string]interface{}{"id": "300"}, "")
		}, "stargate:space:100", "100", "300", "", "100", "300"},
		{"own channel, about a space", func() {
			PublishToUserIn(ctx, rdb, 7, InSpace(100), "SPACE_CREATE", map[string]interface{}{"id": "100"}, "")
		}, "stargate:user:7", "100", "", "7", "100", ""},
		{"own channel, about a pm", func() {
			PublishToUsersIn(ctx, rdb, []int64{7}, InRoom(nil, 500), "ROOM_CREATE", map[string]interface{}{"id": "500"}, "")
		}, "stargate:user:7", "", "500", "7", "", "500"},
		{"own channel, about nothing", func() {
			PublishToUser(ctx, rdb, 7, "RELATIONSHIP_ADD", map[string]interface{}{"id": "9"}, "")
		}, "stargate:user:7", "", "", "7", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.publish()
			msgs := pubs.take()
			if len(msgs) != 1 || msgs[0].Channel != tc.channel {
				t.Fatalf("published %v, want one on %s", msgs, tc.channel)
			}
			f := deliver(t, h, c, msgs[0])
			for key, want := range map[string]string{"space_id": tc.space, "room_id": tc.room, "user_id": tc.user} {
				got, present := f.id(key)
				if want == "" && present {
					t.Errorf("%s = %q, want it absent", key, got)
				} else if want != "" && got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			for key, want := range map[string]string{"space_id": tc.payloadSpace, "room_id": tc.payload} {
				got, present := f.d[key]
				if want == "" && present {
					t.Errorf("payload %s = %v, want it absent", key, got)
				} else if want != "" && got != want {
					t.Errorf("payload %s = %v, want %q", key, got, want)
				}
			}
			for _, internal := range []string{"scoped", "scope_space_id", "scope_room_id", "subscribe"} {
				if _, ok := f.raw[internal]; ok {
					t.Errorf("frame carries %s", internal)
				}
			}
		})
	}
}

// An API binary one release behind publishes envelopes with only the channel: both ids carry
// it, exactly as before, rather than going missing mid-upgrade.
func TestUnscopedEnvelopeKeepsTheLegacyIDs(t *testing.T) {
	rdb, pubs := rawRedis(t)
	h, c := scopeHub(t, rdb, nil, "300")
	PublishToSpace(context.Background(), rdb, 300, "MESSAGE_CREATE", map[string]interface{}{"room_id": "300"}, "")
	msgs := pubs.take()
	if len(msgs) != 1 {
		t.Fatalf("published %v", msgs)
	}
	if env := envelope(t, msgs[0]); env.Scoped {
		t.Fatal("the legacy publisher wrote a scope")
	}
	f := deliver(t, h, c, msgs[0])
	if s, _ := f.id("space_id"); s != "300" {
		t.Errorf("space_id %q", s)
	}
	if r, _ := f.id("room_id"); r != "300" {
		t.Errorf("room_id %q", r)
	}
}

// The routing keys are what a gateway one release behind reads: a room's event must keep
// naming the room's channel there, never the space, or a private room would reach everyone.
func TestScopedEnvelopeRoutesLikeTheLegacyOne(t *testing.T) {
	rdb, pubs := rawRedis(t)
	space := int64(100)
	PublishToRoom(context.Background(), rdb, InRoom(&space, 300).Subscribing(300), "THREAD_CREATE", nil, "")
	PublishToUserIn(context.Background(), rdb, 7, InSpace(100).About(300), "SPACE_ROOM_DELETE", nil, "")
	msgs := pubs.take()
	if len(msgs) != 2 {
		t.Fatalf("published %v", msgs)
	}
	room, user := envelope(t, msgs[0]), envelope(t, msgs[1])
	if room.SpaceID != "300" || room.UserID != "" || room.ScopeSpaceID != "100" || room.ScopeRoomID != "300" {
		t.Errorf("room envelope %+v", room)
	}
	if len(room.Subscribe) != 1 || room.Subscribe[0] != "300" {
		t.Errorf("subscribe %v", room.Subscribe)
	}
	if user.SpaceID != "" || user.UserID != "7" || user.ScopeSpaceID != "100" || user.ScopeRoomID != "300" {
		t.Errorf("user envelope %+v", user)
	}
}

// Callers publish one map to many channels (presence to every space, a room's create to two
// groups of members). The scope's ids go into a copy, never into the caller's map.
func TestScopedPayloadIsACopy(t *testing.T) {
	ctx := context.Background()
	rdb, pubs := rawRedis(t)
	shared := map[string]interface{}{"user_id": "7", "presence": "online"}
	PublishToSpaceChannel(ctx, rdb, InSpace(100), "PRESENCE_UPDATE", shared, "")
	PublishToSpaceChannel(ctx, rdb, InSpace(200), "PRESENCE_UPDATE", shared, "")
	PublishToUsersIn(ctx, rdb, []int64{7, 8}, InSpace(300).About(301), "SPACE_ROOM_CREATE", shared, "")
	if len(shared) != 2 {
		t.Fatalf("caller's map changed: %v", shared)
	}
	msgs := pubs.take()
	want := []struct{ space, room string }{{"100", ""}, {"200", ""}, {"300", "301"}, {"300", "301"}}
	if len(msgs) != len(want) {
		t.Fatalf("published %d, want %d", len(msgs), len(want))
	}
	for i, m := range msgs {
		d, _ := envelope(t, m).Data.(map[string]any)
		if d["space_id"] != want[i].space || d["user_id"] != "7" {
			t.Errorf("publish %d: %v", i, d)
		}
		if r, ok := d["room_id"]; (want[i].room == "" && ok) || (want[i].room != "" && r != want[i].room) {
			t.Errorf("publish %d: room_id %v", i, r)
		}
	}
}

// A payload that is not a map goes out as it is; a room event without a room goes nowhere.
func TestScopedHelpersEdgeCases(t *testing.T) {
	ctx := context.Background()
	rdb, pubs := rawRedis(t)
	type state struct {
		RoomID string `json:"room_id"`
	}
	PublishToRoom(ctx, rdb, InRoom(nil, 300), "VOICE_STATE_UPDATE", state{RoomID: "300"}, "")
	msgs := pubs.take()
	if len(msgs) != 1 {
		t.Fatalf("published %v", msgs)
	}
	if d, _ := envelope(t, msgs[0]).Data.(map[string]any); len(d) != 1 || d["room_id"] != "300" {
		t.Errorf("payload %v", d)
	}
	PublishToRoom(ctx, rdb, InSpace(100), "MESSAGE_CREATE", nil, "")
	PublishToSpaceChannel(ctx, rdb, InRoom(nil, 300), "SPACE_UPDATE", nil, "")
	if msgs := pubs.take(); len(msgs) != 0 {
		t.Errorf("published %v", msgs)
	}
	if sc := InSpace(1).Subscribing(2); len(sc.Subscribing(3).Subscribe) != 2 || len(sc.Subscribe) != 1 {
		t.Errorf("Subscribing shares its list: %v", sc.Subscribe)
	}
}

// scoperFake is reauthFake that also says what each channel is.
type scoperFake struct {
	reauthFake
	scopes map[string]Scope
}

func (f *scoperFake) CanAccessChannel(ctx context.Context, userID int64, channelID string) (bool, Scope, error) {
	ok, err := f.CanAccessRoom(ctx, userID, channelID)
	return ok, f.scopes[channelID], err
}

// op 2 names only a channel. What the channel is was learnt when the subscription was
// authorized, and the relay carries it; an authorizer that cannot tell leaves it unscoped.
func TestOpTwoCarriesTheChannelsScope(t *testing.T) {
	space := int64(100)
	auth := &scoperFake{
		reauthFake: reauthFake{answers: map[string]error{}, calls: map[string]int{}},
		scopes:     map[string]Scope{"300": InRoom(&space, 300), "100": InSpace(100)},
	}
	rdb, pubs := rawRedis(t)
	h, c := scopeHub(t, rdb, auth)
	c.handleSubscribe(map[string]interface{}{"space_id": "300"})
	c.handleSubscribe(map[string]interface{}{"space_id": "100"})
	if codes := errorFrames(c); len(codes) != 0 {
		t.Fatalf("errors %v", codes)
	}
	c.handleSend(map[string]interface{}{"space_id": "300", "type": "ping", "content": "x"})
	c.handleSend(map[string]interface{}{"space_id": "100", "type": "ping", "content": "x"})
	msgs := pubs.take()
	if len(msgs) != 2 {
		t.Fatalf("published %v", msgs)
	}
	if f := deliver(t, h, c, msgs[0]); f.raw["space_id"] != "100" || f.raw["room_id"] != "300" {
		t.Errorf("room relay %v", f.raw)
	}
	if f := deliver(t, h, c, msgs[1]); f.raw["space_id"] != "100" || f.raw["room_id"] != nil {
		t.Errorf("space relay %v", f.raw)
	}

	// Unsubscribing forgets it.
	c.handleUnsubscribe(map[string]interface{}{"space_id": "300"})
	if _, ok := c.scopeOf("300"); ok {
		t.Error("scope outlived its subscription")
	}

	// Without a ChannelScoper nothing is recorded, and the relay is the legacy shape.
	h2, c2 := scopeHub(t, rdb, &reauthFake{answers: map[string]error{}, calls: map[string]int{}})
	c2.handleSubscribe(map[string]interface{}{"space_id": "300"})
	c2.handleSend(map[string]interface{}{"space_id": "300", "type": "ping", "content": "x"})
	msgs = pubs.take()
	if len(msgs) != 1 || envelope(t, msgs[0]).Scoped {
		t.Fatalf("published %v", msgs)
	}
	if f := deliver(t, h2, c2, msgs[0]); f.raw["space_id"] != "300" || f.raw["room_id"] != "300" {
		t.Errorf("unscoped relay %v", f.raw)
	}
}

// A refused subscription records nothing.
func TestRefusedSubscriptionRecordsNoScope(t *testing.T) {
	auth := &scoperFake{
		reauthFake: reauthFake{answers: map[string]error{"300": errRefused}, calls: map[string]int{}},
		scopes:     map[string]Scope{"300": InRoom(nil, 300)},
	}
	_, c := scopeHub(t, nil, auth)
	c.handleSubscribe(map[string]interface{}{"space_id": "300"})
	if codes := errorFrames(c); len(codes) != 1 || codes[0] != 4403 {
		t.Fatalf("errors %v", codes)
	}
	if c.hasSub("space", "300") {
		t.Fatal("subscribed")
	}
	if _, ok := c.scopeOf("300"); ok {
		t.Fatal("scope recorded for a refused channel")
	}
}

// Every publisher names what its event is about. The legacy PublishToSpace knows only a
// channel - it put a room's id into space_id - so nothing outside publish.go may call it.
func TestNothingCallsTheUnscopedSpacePublisher(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "tmp", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if filepath.ToSlash(rel) == "internal/stargate/publish.go" {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if name == "PublishToSpace" {
				offenders = append(offenders, fset.Position(call.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("PublishToSpace called at %v: use PublishToRoom / PublishToSpaceChannel with the event's scope", offenders)
	}
}
