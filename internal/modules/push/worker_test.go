package push

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A world for the delivery tests: a space with a text channel and a thread, a group PM and
// a PM; Alice (1) writes, Bob (2), Carol (3) and Dave (4) read on their phones.
type world struct {
	f    *fakePushService
	repo *memRepo
	dir  *memDir
	svc  *Service
	devs map[int64]*testDevice
}

const (
	alice, bob, carol, dave       = 1, 2, 3, 4
	spaceID                       = 100
	channelID, threadID           = 101, 102
	groupID, pmID, privateChannel = 201, 202, 103
	roleMods                      = 300
)

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{f: newFakePushService(t), dir: newMemDir(), devs: map[int64]*testDevice{}}
	w.dir.users[alice] = &UserInfo{ID: alice, Username: "alice", DisplayName: "Alice", Avatar: "https://cdn.example/v1/avatars/1/a.webp"}
	w.dir.users[bob] = &UserInfo{ID: bob, Username: "bob"}
	w.dir.users[carol] = &UserInfo{ID: carol, Username: "carol", DisplayName: "Carol"}
	w.dir.users[dave] = &UserInfo{ID: dave, Username: "dave"}
	w.dir.spaces[spaceID] = &SpaceInfo{ID: spaceID, Name: "Strafe HQ"}
	w.dir.rooms[channelID] = &RoomInfo{ID: channelID, Type: roomTypeSpaceText, SpaceID: spaceID, Name: "general"}
	w.dir.rooms[privateChannel] = &RoomInfo{ID: privateChannel, Type: roomTypeSpaceText, SpaceID: spaceID, Name: "secret-plans"}
	w.dir.rooms[threadID] = &RoomInfo{ID: threadID, Type: roomTypeThread, SpaceID: spaceID, ParentID: channelID, Name: "release"}
	w.dir.rooms[groupID] = &RoomInfo{ID: groupID, Type: roomTypeGroupPM}
	w.dir.rooms[pmID] = &RoomInfo{ID: pmID, Type: roomTypePM}
	w.dir.members[groupID] = []int64{alice, bob, carol, dave}
	w.dir.roles[spaceID] = map[int64]string{roleMods: "Moderators"}
	w.dir.hidden[[2]int64{dave, privateChannel}] = true

	var subs []Subscription
	for _, uid := range []int64{bob, carol, dave} {
		d := newTestDevice(t)
		w.devs[uid] = d
		subs = append(subs, d.subscription(uid, uid*10, w.f.srv.URL+"/p/"+formatID(uid), DefaultPrefs()))
	}
	w.repo = newMemRepo(subs...)
	w.svc = newTestService(t, w.f, w.repo, w.dir)
	return w
}

// received maps each recipient to the payloads their device got, by the endpoint path.
func (w *world) received(t *testing.T) map[int64][]Payload {
	t.Helper()
	out := map[int64][]Payload{}
	for _, p := range w.f.pushes() {
		var uid int64
		for id, d := range w.devs {
			if plain, err := decryptAES128GCM(p.Body, d.priv, d.auth); err == nil {
				var pl Payload
				_ = json.Unmarshal(plain, &pl)
				out[id] = append(out[id], pl)
				uid = id
				break
			}
		}
		if uid == 0 {
			t.Fatal("a push no device can decrypt")
		}
	}
	return out
}

func msgJob(room int64, rt int, text string, e2ee bool, users []int64, mentioned []int64) *job {
	j := &job{T: TypeMessage, Users: users, Room: room, RoomType: rt, TS: 1760000000000,
		Message: &messageJob{ID: 999, Sender: alice, E2EE: e2ee, Text: text, Mentioned: mentioned}}
	if rt >= roomTypeSpaceText {
		j.Space = spaceID
	}
	return j
}

func TestProcessSpaceMessage(t *testing.T) {
	w := newWorld(t)
	w.repo.update(carol, carol*10, func(s *Subscription) { s.PrefSpace = ModeAll; s.PrefPreview = false })
	// Bob is mentioned (space default is mentions-only), Carol wants everything but no
	// previews, Dave wants mentions and is not mentioned.
	w.svc.process(context.Background(), msgJob(channelID, roomTypeSpaceText, "hi <@2>, see <#103>", false, []int64{bob, carol, dave}, []int64{bob}))
	got := w.received(t)
	if len(got[dave]) != 0 || len(got[bob]) != 1 || len(got[carol]) != 1 {
		t.Fatalf("deliveries: bob %d carol %d dave %d", len(got[bob]), len(got[carol]), len(got[dave]))
	}
	b := got[bob][0]
	if b.V != 1 || b.T != TypeMessage || b.UID != "2" || b.RoomID != "101" || b.RoomType != 3 || b.SpaceID != "100" ||
		b.MessageID != "999" || b.RoomName != "general" || b.SpaceName != "Strafe HQ" || !b.Mentioned || b.Encrypted {
		t.Errorf("bob's payload: %+v", b)
	}
	if b.Author == nil || b.Author.ID != "1" || b.Author.Name != "Alice" || b.Author.Avatar != "https://cdn.example/v1/avatars/1/a.webp" {
		t.Errorf("author: %+v", b.Author)
	}
	if b.Body != "hi @bob, see #secret-plans" {
		t.Errorf("bob's preview = %q", b.Body)
	}
	if c := got[carol][0]; c.Body != "" || c.Mentioned || c.UID != "3" {
		t.Errorf("carol's payload (no previews, not mentioned): %+v", c)
	}
}

// A room link names a private channel only to those who can see it.
func TestPreviewHidesPrivateRoomNames(t *testing.T) {
	w := newWorld(t)
	w.svc.process(context.Background(), msgJob(channelID, roomTypeSpaceText, "plans in <#103>", false, []int64{dave}, []int64{dave}))
	got := w.received(t)
	if len(got[dave]) != 1 || got[dave][0].Body != "plans in #room" {
		t.Fatalf("dave got %+v", got[dave])
	}
}

// A message in a channel the recipient cannot view tells them nothing at all.
func TestNoPushFromInvisibleRoom(t *testing.T) {
	w := newWorld(t)
	w.svc.process(context.Background(), msgJob(privateChannel, roomTypeSpaceText, "@everyone", false, []int64{bob, dave}, nil))
	got := w.received(t)
	if len(got[dave]) != 0 {
		t.Error("pushed a private channel to someone who cannot see it")
	}
	_ = got
}

func TestProcessEveryoneAndRoleMentions(t *testing.T) {
	w := newWorld(t)
	j := msgJob(channelID, roomTypeSpaceText, "<@&300> and @everyone", false, []int64{bob, carol, dave}, nil)
	j.Message.Everyone = true
	w.svc.process(context.Background(), j)
	got := w.received(t)
	for _, uid := range []int64{bob, carol, dave} {
		if len(got[uid]) != 1 || !got[uid][0].Mentioned {
			t.Errorf("user %d: %+v", uid, got[uid])
		}
	}
	if got[bob][0].Body != "@Moderators and @everyone" {
		t.Errorf("preview %q", got[bob][0].Body)
	}
}

func TestProcessE2EEMessageCarriesNoBody(t *testing.T) {
	w := newWorld(t)
	// Even if a producer slipped text in, an E2EE message never shows it.
	w.svc.process(context.Background(), msgJob(pmID, roomTypePM, "should never appear", true, []int64{bob}, nil))
	got := w.received(t)
	if len(got[bob]) != 1 {
		t.Fatalf("bob got %d", len(got[bob]))
	}
	p := got[bob][0]
	if !p.Encrypted || p.Body != "" || p.RoomName != "" || p.SpaceID != "" {
		t.Errorf("E2EE PM payload: %+v", p)
	}
}

func TestProcessGroupPMName(t *testing.T) {
	w := newWorld(t)
	w.svc.process(context.Background(), msgJob(groupID, roomTypeGroupPM, "", true, []int64{bob}, nil))
	got := w.received(t)
	if len(got[bob]) != 1 || got[bob][0].RoomName != "Alice, Carol, dave" {
		t.Fatalf("group name: %+v", got[bob])
	}
	w2 := newWorld(t)
	w2.dir.rooms[groupID].Name = "Weekend plans"
	w2.svc.process(context.Background(), msgJob(groupID, roomTypeGroupPM, "", true, []int64{bob}, nil))
	if g := w2.received(t)[bob]; len(g) != 1 || g[0].RoomName != "Weekend plans" {
		t.Fatalf("named group: %+v", g)
	}
}

func TestProcessThreadMessage(t *testing.T) {
	w := newWorld(t)
	j := msgJob(threadID, roomTypeThread, "thread reply <a:wave:9>", false, []int64{bob}, []int64{bob})
	j.Message.Attachments = 2
	w.svc.process(context.Background(), j)
	got := w.received(t)
	if len(got[bob]) != 1 {
		t.Fatal("no push")
	}
	p := got[bob][0]
	if p.RoomType != 6 || p.RoomName != "release" || p.ParentName != "general" || p.SpaceName != "Strafe HQ" || p.Attach != 2 || p.Body != "thread reply :wave:" {
		t.Errorf("thread payload: %+v", p)
	}
}

func TestProcessRespectsUserLevelRules(t *testing.T) {
	w := newWorld(t)
	w.dir.users[bob].Status = "dnd"
	w.dir.users[carol].Blocks = []int64{alice}
	future := time.Now().Add(time.Hour)
	w.dir.settings[[2]int64{dave, pmID}] = &RoomSettings{MutedUntil: &future}
	w.svc.process(context.Background(), msgJob(pmID, roomTypePM, "", true, []int64{bob, carol, dave}, nil))
	if n := len(w.f.pushes()); n != 0 {
		t.Errorf("%d pushes to a DND, a blocking and a muting recipient", n)
	}
}

func TestProcessDropsRevokedSessionsAndDeadEndpoints(t *testing.T) {
	w := newWorld(t)
	w.dir.revoked[[2]int64{carol, carol * 10}] = true
	// Bob's push service says his subscription is gone.
	w.f.statuses = []int{http.StatusGone}
	w.svc.process(context.Background(), msgJob(pmID, roomTypePM, "", true, []int64{bob, carol}, nil))
	if s, _ := w.repo.Get(context.Background(), carol, carol*10); s != nil {
		t.Error("subscription of a revoked session kept")
	}
	if s, _ := w.repo.Get(context.Background(), bob, bob*10); s != nil {
		t.Error("subscription the push service called gone kept")
	}
	if n := len(w.f.pushes()); n != 1 {
		t.Errorf("%d pushes, want only Bob's attempt", n)
	}
}

func TestRejectedDeliveriesDeleteAfterFive(t *testing.T) {
	w := newWorld(t)
	for i := 1; i <= 5; i++ {
		w.f.statuses = []int{http.StatusForbidden}
		w.svc.process(context.Background(), msgJob(pmID, roomTypePM, "", true, []int64{bob}, nil))
		s, _ := w.repo.Get(context.Background(), bob, bob*10)
		if i < 5 && (s == nil || s.Failures != i || s.LastError != "http 403") {
			t.Fatalf("after %d rejections: %+v", i, s)
		}
		if i == 5 && s != nil {
			t.Fatal("subscription kept after five rejections")
		}
	}
	// A success clears the streak.
	w = newWorld(t)
	w.f.statuses = []int{http.StatusForbidden}
	w.svc.process(context.Background(), msgJob(pmID, roomTypePM, "", true, []int64{bob}, nil))
	w.svc.process(context.Background(), msgJob(pmID, roomTypePM, "", true, []int64{bob}, nil))
	if s, _ := w.repo.Get(context.Background(), bob, bob*10); s == nil || s.Failures != 0 || s.LastError != "" || s.LastSuccessAt == nil {
		t.Errorf("after a success: %+v", s)
	}
}

func TestProcessCallJobs(t *testing.T) {
	w := newWorld(t)
	w.repo.update(dave, dave*10, func(s *Subscription) { s.PrefCalls = false })
	w.dir.users[bob].Status = "dnd" // calls ring through DND, as on the web
	w.svc.process(context.Background(), &job{T: TypeCall, Users: []int64{bob, carol, dave}, Room: groupID, RoomType: roomTypeGroupPM, TS: 5, Caller: alice})
	got := w.received(t)
	if len(got[bob]) != 1 || len(got[carol]) != 1 || len(got[dave]) != 0 {
		t.Fatalf("call deliveries: %d %d %d", len(got[bob]), len(got[carol]), len(got[dave]))
	}
	c := got[bob][0]
	if c.T != TypeCall || c.Author == nil || c.Author.Name != "Alice" || c.RoomName != "Alice, Carol, dave" || c.RoomID != "201" || c.RoomType != 2 {
		t.Errorf("call payload: %+v", c)
	}
	headers := w.f.pushes()[0].Header
	if headers.Get("Topic") != "c201" || headers.Get("Ttl") != "45" || headers.Get("Urgency") != "high" {
		t.Errorf("call headers: %v", headers)
	}

	w = newWorld(t)
	w.repo.update(dave, dave*10, func(s *Subscription) { s.PrefCalls = false })
	w.svc.process(context.Background(), &job{T: TypeCallEnd, Users: []int64{bob, dave}, Room: groupID, RoomType: roomTypeGroupPM, TS: 6})
	got = w.received(t)
	if len(got[bob]) != 1 || len(got[dave]) != 0 {
		t.Fatalf("call_end deliveries: %d %d", len(got[bob]), len(got[dave]))
	}
	e := got[bob][0]
	if e.T != TypeCallEnd || e.RoomID != "201" || e.RoomType != 2 || e.Author != nil || e.SpaceID != "" || e.MessageID != "" {
		t.Errorf("call_end payload: %+v", e)
	}
	if h := w.f.pushes()[0].Header; h.Get("Urgency") != "normal" || h.Get("Topic") != "c201" || h.Get("Ttl") != "60" {
		t.Errorf("call_end headers: %v", h)
	}
}

func TestProcessReadJob(t *testing.T) {
	w := newWorld(t)
	w.svc.process(context.Background(), &job{T: TypeRead, Users: []int64{bob}, Room: channelID, RoomType: roomTypeSpaceText, Space: spaceID, TS: 7, ReadTo: 999})
	got := w.received(t)
	if len(got[bob]) != 1 {
		t.Fatal("no read push")
	}
	r := got[bob][0]
	if r.T != TypeRead || r.RoomID != "101" || r.RoomType != 3 || r.SpaceID != "100" || r.MessageID != "999" || r.UID != "2" {
		t.Errorf("read payload: %+v", r)
	}
	if h := w.f.pushes()[0].Header; h.Get("Urgency") != "low" || h.Get("Topic") != "r101" || h.Get("Ttl") != "3600" {
		t.Errorf("read headers: %v", h)
	}
}

func TestMarshalPayloadFitsTheLargestBucket(t *testing.T) {
	huge := strings.Repeat("ü", 1000)
	p := &Payload{V: 1, T: TypeMessage, UID: "1", Body: strings.Repeat("x", 1500), RoomName: huge, SpaceName: huge,
		Author: &Author{ID: "1", Name: huge, Avatar: "https://cdn.example/" + strings.Repeat("a", 400)}}
	b, err := marshalPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > MaxPayloadBytes {
		t.Fatalf("payload is %d bytes", len(b))
	}
	if p.Body != "" || p.Author.Avatar != "" {
		t.Error("detail not shed in order")
	}
}

func TestSubscribeAndTest(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	dev := newTestDevice(t)
	in := &SubscribeInput{Endpoint: w.f.srv.URL + "/p/new", Kind: KindAPNs}
	in.Keys.P256dh, in.Keys.Auth = b64(dev.priv.PublicKey().Bytes()), b64(dev.auth)
	in.Prefs.Space = strPtr(ModeAll)
	sub, err := w.svc.Subscribe(ctx, 5, 50, in)
	if err != nil {
		t.Fatal(err)
	}
	v := sub.View()
	if v.Kind != KindAPNs || v.EndpointHost != strings.TrimPrefix(w.f.srv.URL, "https://") || v.Prefs.Space != ModeAll || v.Prefs.PM != ModeAll || !v.Prefs.Preview || !v.Prefs.Calls {
		t.Errorf("view: %+v", v)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "/p/new") || strings.Contains(string(raw), in.Keys.Auth) {
		t.Errorf("view leaks the endpoint path or keys: %s", raw)
	}

	updated, err := w.svc.Update(ctx, 5, 50, &UpdateInput{Prefs: PrefsPatch{Calls: boolPtr(false)}})
	if err != nil || updated.Prefs().Calls || updated.Prefs().Space != ModeAll {
		t.Fatalf("update: %+v %v", updated, err)
	}

	res, err := w.svc.Test(ctx, 5, 50)
	if err != nil || !res.Delivered {
		t.Fatalf("test push: %+v %v", res, err)
	}
	pushes := w.f.pushes()
	p := dev.open(t, pushes[len(pushes)-1])
	if p.T != TypeTest || p.UID != "5" || p.V != 1 || p.RoomID != "" {
		t.Errorf("test payload: %+v", p)
	}
	if h := pushes[len(pushes)-1].Header; h.Get("Ttl") != "300" || h.Get("Urgency") != "high" {
		t.Errorf("test headers: %v", h)
	}

	w.f.statuses = []int{http.StatusGone}
	res, err = w.svc.Test(ctx, 5, 50)
	if err != nil || res.Delivered || res.Status != http.StatusGone || res.Error == "" {
		t.Fatalf("test push to a gone subscription: %+v %v", res, err)
	}
	if _, err := w.svc.Get(ctx, 5, 50); err != ErrNoSubscription {
		t.Errorf("gone subscription still there: %v", err)
	}
	if err := w.svc.Delete(ctx, 5, 50); err != nil {
		t.Errorf("delete is not idempotent: %v", err)
	}
	if _, err := w.svc.Test(ctx, 5, 50); err != ErrNoSubscription {
		t.Errorf("test without a subscription: %v", err)
	}
}

// A row left holding only delivery bookkeeping (an outcome written just after a delete)
// is not a subscription.
func TestBookkeepingOnlyRowIsDropped(t *testing.T) {
	w := newWorld(t)
	_ = w.repo.RecordFailure(context.Background(), 7, 70, "http 500", 1)
	if _, err := w.svc.Get(context.Background(), 7, 70); err != ErrNoSubscription {
		t.Errorf("keyless row reads as a subscription: %v", err)
	}
	w.svc.process(context.Background(), msgJob(pmID, roomTypePM, "", true, []int64{7}, nil))
	if s, _ := w.repo.Get(context.Background(), 7, 70); s != nil {
		t.Error("keyless row not cleaned up")
	}
}
