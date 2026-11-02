package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/voice"
	"github.com/StrafeChat/equinox/internal/redistest"
)

// Calls in mirrored spaces on the wire: the relays, the pull, moderation forwarded to the
// origin and running calls in a snapshot. A peer on either side may be older, so what is new
// here has to mean nothing to one that does not know it.

const (
	vsLounge = 300 // open voice room, on the origin
	vsSecret = 301 // @everyone denied View, Staff allowed
	vsText   = 302
	vsGhost  = 303 // a state whose user no longer exists
)

// vsFedRepo is roleFedRepo with rooms mapped: a room hosted here maps to itself.
type vsFedRepo struct {
	roleFedRepo
}

func (r *vsFedRepo) GetRoomMapping(_ context.Context, roomID int64) (*RoomMapping, error) {
	return &RoomMapping{RoomID: roomID, OriginDomain: "a.example", OriginRoomID: roomID}, nil
}

func (r *vsFedRepo) PutRoomMapping(context.Context, *RoomMapping) error { return nil }

// vsRooms is a room repository over a map.
type vsRooms struct {
	rooms.Repository
	byID map[int64]*rooms.Room
}

func (r *vsRooms) GetByID(_ context.Context, roomID int64) (*rooms.Room, error) {
	return r.byID[roomID], nil
}

func (r *vsRooms) GetByIDs(_ context.Context, ids []int64) ([]*rooms.Room, error) {
	out := make([]*rooms.Room, len(ids))
	for i, rid := range ids {
		out[i] = r.byID[rid]
	}
	return out, nil
}

func (r *vsRooms) GetParticipants(context.Context, int64) ([]int64, error) { return nil, nil }

func (r *vsRooms) ListBySpace(context.Context, int64) ([]rooms.RoomBySpaceRow, error) {
	out := make([]rooms.RoomBySpaceRow, 0, len(r.byID))
	for rid := range r.byID {
		out = append(out, rooms.RoomBySpaceRow{RoomID: rid})
	}
	return out, nil
}

func (r *vsRooms) ListThreadMemberIDs(context.Context, int64) ([]int64, error) { return nil, nil }

// vsSpaceRepo is roleRepo plus what the view rule reads.
type vsSpaceRepo struct {
	*roleRepo
	overs     map[int64][]spaces.SpaceRoomRoleOverride
	listFails bool
}

func (r *vsSpaceRepo) ListRoomRoleOverrides(_ context.Context, _, roomID int64) ([]spaces.SpaceRoomRoleOverride, error) {
	return r.overs[roomID], nil
}

func (r *vsSpaceRepo) ListRoomUserOverrides(context.Context, int64, int64) ([]spaces.SpaceRoomUserOverride, error) {
	return nil, nil
}

func (r *vsSpaceRepo) ListMembers(context.Context, int64) ([]spaces.SpaceMember, error) {
	if r.listFails {
		return nil, errUserRead
	}
	out := make([]spaces.SpaceMember, 0, len(r.members))
	for _, m := range r.members {
		out = append(out, *m)
	}
	return out, nil
}

// vsUsers is profileTestUsers with the batched read, and a read that fails on demand.
type vsUsers struct {
	*profileTestUsers
	failFor map[int64]bool
}

var errUserRead = errors.New("scylla: user read failed")

func (u *vsUsers) GetByID(ctx context.Context, uid int64) (*auth.User, error) {
	if u.failFor[uid] {
		return nil, errUserRead
	}
	return u.profileTestUsers.GetByID(ctx, uid)
}

func (u *vsUsers) GetByIDs(_ context.Context, ids []int64) ([]*auth.User, error) {
	out := make([]*auth.User, len(ids))
	for i, uid := range ids {
		out[i] = u.byID[uid]
	}
	return out, nil
}

// vsOrigin is rpOrigin hosting voice rooms, with a real voice service on an in-memory Redis.
type vsWorld struct {
	s     *Service
	store *voice.Store
	space *vsSpaceRepo
	users *vsUsers
}

func vsOrigin(t *testing.T) *vsWorld {
	t.Helper()
	s, repo := rpOrigin()
	space := int64(rpSpace)
	roomRepo := &vsRooms{byID: map[int64]*rooms.Room{
		vsLounge: {ID: vsLounge, Type: rooms.TypeSpaceVoice, SpaceID: &space},
		vsSecret: {ID: vsSecret, Type: rooms.TypeSpaceVoice, SpaceID: &space},
		vsGhost:  {ID: vsGhost, Type: rooms.TypeSpaceVoice, SpaceID: &space},
		vsText:   {ID: vsText, Type: rooms.TypeSpaceText, SpaceID: &space},
	}}
	store := &vsSpaceRepo{roleRepo: repo, overs: map[int64][]spaces.SpaceRoomRoleOverride{vsSecret: {
		{SpaceID: rpSpace, RoomID: vsSecret, RoleID: rpEveryone, Deny: permissions.PermViewRoom},
		{SpaceID: rpSpace, RoomID: vsSecret, RoleID: rpStaff, Allow: permissions.PermViewRoom},
	}}}
	users := &vsUsers{profileTestUsers: s.users.(*profileTestUsers), failFor: map[int64]bool{}}
	s.users = users
	s.roomRepo = roomRepo
	s.repo = &vsFedRepo{roleFedRepo: roleFedRepo{peers: []string{"b.example", "c.example"}, mapping: &SpaceMapping{SpaceID: rpSpace, OriginDomain: "a.example", OriginSpaceID: rpSpace}}}
	s.spaceSvc = spaces.NewService(store, roomRepo, users, nil, nil)
	s.spaceSvc.SetFederator(s)
	s.roomSvc = rooms.NewService(roomRepo, users, nil, nil, nil, s.spaceSvc)
	s.roomSvc.SetFederationInfo(s)
	rdb, _ := redistest.New()
	vstore := voice.NewStore(rdb, "")
	liveKit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/protobuf")
	}))
	t.Cleanup(liveKit.Close)
	lk := voice.NewLiveKit(config.VoiceConfig{InternalURL: liveKit.URL, APIKey: "k", APISecret: "secretsecretsecretsecretsecretsecret"})
	s.voice = voice.NewService(vstore, lk, s.roomSvc, s.spaceSvc, users, rdb, nil, nil)
	s.voice.SetFederator(s)
	return &vsWorld{s: s, store: vstore, space: store, users: users}
}

func (w *vsWorld) join(t *testing.T, roomID, userID int64) {
	t.Helper()
	st := &voice.State{UserID: userID, RoomID: roomID, SpaceID: rpSpace, SessionID: "s" + id.Format(userID), JoinedAt: time.Now().UTC()}
	if err := w.store.PutState(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

// relayLog is what went to each peer: domain -> bodies.
type relayLog struct {
	mu   sync.Mutex
	sent map[string][]any
	done chan struct{}
}

func recordRelays(s *Service) *relayLog {
	l := &relayLog{sent: map[string][]any{}, done: make(chan struct{}, 8)}
	s.outQueues = map[string]chan func(){}
	s.deliver = func(_ context.Context, domain, _, _ string, body any) error {
		l.mu.Lock()
		l.sent[domain] = append(l.sent[domain], body)
		l.mu.Unlock()
		l.done <- struct{}{}
		return nil
	}
	return l
}

func (l *relayLog) wait(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-l.done:
		case <-time.After(5 * time.Second):
			t.Fatal("no relay arrived")
		}
	}
}

// ---- states on the wire ----------------------------------------------------------------------

func TestVoiceStateFromWireTakesTheSpaceFromTheRoom(t *testing.T) {
	s, _ := mirrorWireFixture()
	remote := int64(70)
	s.users.(*profileTestUsers).byID[3] = shadowUser(3, remote, "a.example")
	spaceID := int64(9)
	joined := time.Now().UTC().Add(-time.Hour)
	wire := VoiceState{User: Profile{FID: FormatFID(remote, "a.example"), Username: "shadow"}, Identity: "@70:a.example.s1", SessionID: "s1", JoinedAt: joined}

	inSpace := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 77, SpaceID: &spaceID}}
	st, err := s.voiceStateFromWire(context.Background(), "a.example", inSpace, wire)
	if err != nil {
		t.Fatal(err)
	}
	if st.RoomID != 77 || st.SpaceID != 9 || st.UserID != 3 || st.Ident != "@70:a.example.s1" {
		t.Errorf("state %+v", st)
	}
	if !st.JoinedAt.Equal(joined) {
		t.Errorf("joined_at changed to %v", st.JoinedAt)
	}
	// On a mirror IssuedAt is when the origin was last heard, not when the user arrived.
	if time.Since(st.IssuedAt) > time.Minute {
		t.Errorf("issued at %v", st.IssuedAt)
	}

	pm, err := s.voiceStateFromWire(context.Background(), "a.example", &rooms.RoomWithParticipants{Room: rooms.Room{ID: 78}}, wire)
	if err != nil {
		t.Fatal(err)
	}
	if pm.SpaceID != 0 {
		t.Errorf("a PM's state has space %d", pm.SpaceID)
	}
}

func TestStatesThatCannotBeReadMakeTheListIncomplete(t *testing.T) {
	s, _ := mirrorWireFixture()
	s.users.(*profileTestUsers).byID[3] = shadowUser(3, 70, "a.example")
	room := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 77}}
	list := []VoiceState{{User: Profile{FID: FormatFID(70, "a.example"), Username: "shadow"}}, {User: Profile{FID: "nonsense"}}}
	got, complete := s.voiceStatesFromWire(context.Background(), "a.example", room, list)
	if len(got) != 1 || complete {
		t.Errorf("got %d, complete %v", len(got), complete)
	}
}

// ---- relays ------------------------------------------------------------------------------------

func hostedLounge(w *vsWorld) *rooms.RoomWithParticipants {
	room, _ := w.s.roomSvc.LoadRoom(context.Background(), vsLounge)
	return room
}

// A kick asked for by a mirror runs on a context that keeps relays from it: its reply carries
// the removal, but nothing in that reply says the member left their call.
func TestVoiceRelayReachesTheInstanceThatAsked(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	ctx, capture := WithRelayCapture(context.Background(), "b.example")
	st := &voice.State{UserID: rpMember, RoomID: vsLounge, SessionID: "s1"}
	w.s.AfterVoiceStateChanged(ctx, hostedLounge(w), st, true)
	log.wait(t, 2)
	if len(log.sent["b.example"]) != 1 || len(log.sent["c.example"]) != 1 {
		t.Errorf("sent %v", log.sent)
	}
	if got := capture.take(); len(got) != 0 {
		t.Errorf("captured %d: an applied reply would have dropped it", len(got))
	}
}

func TestCallRelayReachesTheInstanceThatAsked(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	ctx, capture := WithRelayCapture(context.Background(), "b.example")
	w.s.AfterCallChanged(ctx, hostedLounge(w), voice.EventCallDelete, nil, nil, false)
	log.wait(t, 2)
	if len(log.sent["b.example"]) != 1 {
		t.Errorf("sent %v", log.sent)
	}
	if got := capture.take(); len(got) != 0 {
		t.Errorf("captured %d", len(got))
	}
}

func TestSpaceRelaysStillSkipThePeerForEverythingElse(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	ctx, capture := WithRelayCapture(context.Background(), "b.example")
	w.s.fanOut(ctx, map[string]struct{}{"b.example": {}, "c.example": {}}, http.MethodPost, "/rooms/typing", "x")
	log.wait(t, 1)
	if len(log.sent["b.example"]) != 0 {
		t.Errorf("sent %v", log.sent)
	}
	if got := capture.take(); len(got) != 1 {
		t.Errorf("captured %d", len(got))
	}
}

func TestMoveNamesTheRoomInTheOriginsIDs(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	dest, _ := w.s.roomSvc.LoadRoom(context.Background(), vsSecret)
	st := &voice.State{UserID: rpMember, RoomID: vsLounge, SessionID: "s1"}
	w.s.AfterVoiceStateMoved(context.Background(), hostedLounge(w), st, dest)
	log.wait(t, 2)
	ev, ok := log.sent["b.example"][0].(VoiceStateEvent)
	if !ok {
		t.Fatalf("relay %#v", log.sent["b.example"][0])
	}
	if !ev.Left || ev.MovedTo == nil || ev.MovedTo.OriginRoomID != id.Format(vsSecret) || ev.MovedTo.OriginDomain != "a.example" {
		t.Errorf("event %+v", ev)
	}
}

func TestPlainChangeNamesNoDestination(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	w.s.AfterVoiceStateChanged(context.Background(), hostedLounge(w), &voice.State{UserID: rpMember, RoomID: vsLounge}, true)
	log.wait(t, 2)
	ev := log.sent["b.example"][0].(VoiceStateEvent)
	if ev.MovedTo != nil {
		t.Errorf("event %+v", ev)
	}
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), "moved_to") {
		t.Errorf("wire %s", raw)
	}
}

func TestMovedToLocal(t *testing.T) {
	ctx := context.Background()
	s, _ := mirrorWireFixture() // mirrorFedRepo maps an origin id to id+1000
	cases := []struct {
		name string
		ref  *RoomRef
		want int64
	}{
		{"known", &RoomRef{OriginDomain: "a.example", OriginRoomID: "5"}, 1005},
		{"no move", nil, 0},
		{"named by another instance", &RoomRef{OriginDomain: "c.example", OriginRoomID: "5"}, 0},
		{"malformed", &RoomRef{OriginDomain: "a.example", OriginRoomID: "x"}, 0},
	}
	for _, tc := range cases {
		if got := s.movedToLocal(ctx, "a.example", tc.ref); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestLeaveNamesItsSession(t *testing.T) {
	s, _ := mirrorWireFixture()
	log := recordRelays(s)
	room := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 1300}}
	s.LeaveRemoteVoiceSession(context.Background(), "a.example", room, &auth.User{ID: 40}, "s1")
	log.wait(t, 1)
	sent, ok := log.sent["a.example"][0].(VoiceUserRequest)
	if !ok || sent.SessionID != "s1" || sent.Room.OriginRoomID != "300" {
		t.Fatalf("sent %+v", log.sent)
	}
	raw, _ := json.Marshal(sent)
	var got VoiceUserRequest
	if err := json.Unmarshal(raw, &got); err != nil || got.SessionID != "s1" {
		t.Fatalf("%s: %v", raw, err)
	}
	// A decline has no session, and an older peer never sends one.
	raw, _ = json.Marshal(VoiceUserRequest{User: "@1:b.example"})
	if strings.Contains(string(raw), "session_id") {
		t.Errorf("wire %s", raw)
	}
}

// voiceRoute is one of a.example's voice routes with the signature check done.
func voiceRoute(peer string, handler fiber.Handler) *fiber.App {
	app := fiber.New()
	app.Post("/voice", func(c fiber.Ctx) error {
		c.Locals(localsKeyDomain, peer)
		return c.Next()
	}, handler)
	return app
}

func post(t *testing.T, app *fiber.App, body any) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "/voice", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// The queued leave of a session the user has already rejoined over must not end the rejoin.
func TestVoiceLeaveHonoursTheSession(t *testing.T) {
	ctx := context.Background()
	w := vsOrigin(t)
	log := recordRelays(w.s)
	w.join(t, vsLounge, rpMember) // session "s5"
	app := voiceRoute("b.example", NewHandler(w.s).VoiceLeave)
	body := VoiceUserRequest{Room: hostedRef(vsLounge), User: FormatFID(rpMemberB, "b.example"), SessionID: "older"}
	if status := post(t, app, body); status != http.StatusNoContent {
		t.Fatalf("status %d", status)
	}
	if states, _ := w.store.RoomStates(ctx, vsLounge); len(states) != 1 {
		t.Fatalf("states %+v", states)
	}
	body.SessionID = "s5"
	if status := post(t, app, body); status != http.StatusNoContent {
		t.Fatalf("status %d", status)
	}
	if states, _ := w.store.RoomStates(ctx, vsLounge); len(states) != 0 {
		t.Fatalf("states %+v", states)
	}
	log.wait(t, 2)
}

// ---- the pull, origin side -----------------------------------------------------------------

// statesApp is a.example's POST /rooms/voice/states with the signature check done: requests
// come from peer.
func statesApp(s *Service, peer string) *fiber.App {
	app := fiber.New()
	app.Post("/states", func(c fiber.Ctx) error {
		c.Locals(localsKeyDomain, peer)
		return c.Next()
	}, NewHandler(s).VoiceStates)
	return app
}

func askStates(t *testing.T, app *fiber.App, refs ...RoomRef) (int, VoiceStatesReply) {
	t.Helper()
	raw, _ := json.Marshal(VoiceStatesRequest{Rooms: refs})
	req, _ := http.NewRequest(http.MethodPost, "/states", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var reply VoiceStatesReply
	_ = json.NewDecoder(res.Body).Decode(&reply)
	return res.StatusCode, reply
}

func hostedRef(roomID int64) RoomRef {
	return RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(roomID)}
}

func fids(list []VoiceState) []string {
	out := make([]string, 0, len(list))
	for i := range list {
		out = append(out, list[i].User.FID)
	}
	return out
}

func TestVoiceStatesAnswersForEachRoom(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsLounge, rpOwner)
	w.join(t, vsLounge, rpMember)
	status, reply := askStates(t, statesApp(w.s, "b.example"),
		hostedRef(vsLounge), hostedRef(vsText), hostedRef(9999), RoomRef{OriginDomain: "a.example", OriginRoomID: "x"})
	if status != http.StatusOK || len(reply.Rooms) != 4 {
		t.Fatalf("status %d, reply %+v", status, reply)
	}
	if got := reply.Rooms[0]; got.Gone || len(got.States) != 2 {
		t.Errorf("live room %+v", got)
	}
	if got := reply.Rooms[1]; got.Gone || len(got.States) != 0 || got.States == nil {
		t.Errorf("an empty room is an answer, not gone: %+v", got)
	}
	if !reply.Rooms[2].Gone || !reply.Rooms[3].Gone {
		t.Errorf("a room that does not exist: %+v", reply.Rooms[2:])
	}
}

func TestVoiceStatesAnswersGoneToAnInstanceNotInTheSpace(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsLounge, rpOwner)
	status, reply := askStates(t, statesApp(w.s, "d.example"), hostedRef(vsLounge))
	if status != http.StatusOK || len(reply.Rooms) != 1 || !reply.Rooms[0].Gone || len(reply.Rooms[0].States) != 0 {
		t.Fatalf("status %d, reply %+v", status, reply)
	}
}

func TestVoiceStatesWithoutVoiceHereIsGone(t *testing.T) {
	w := vsOrigin(t)
	w.s.voice = nil
	status, reply := askStates(t, statesApp(w.s, "b.example"), hostedRef(vsLounge))
	if status != http.StatusOK || !reply.Rooms[0].Gone {
		t.Fatalf("status %d, reply %+v", status, reply)
	}
}

func TestVoiceStatesRefusesAnOversizedBatch(t *testing.T) {
	w := vsOrigin(t)
	refs := make([]RoomRef, voice.MaxSyncRooms+1)
	for i := range refs {
		refs[i] = hostedRef(vsLounge)
	}
	if status, _ := askStates(t, statesApp(w.s, "b.example"), refs...); status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
}

// A room nobody on the peer can view: its occupants are not that peer's to know. An answer of
// nobody also clears what a mirror held from before its last viewer left.
func TestVoiceStatesHidesAPrivateRoomFromAPeerWithNoViewer(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsSecret, rpHigh)
	_, reply := askStates(t, statesApp(w.s, "b.example"), hostedRef(vsSecret))
	if got := reply.Rooms[0]; got.Gone || len(got.States) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestVoiceStatesShowsAPrivateRoomToAPeerWithAViewer(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsSecret, rpHigh)
	w.space.members[rpMember].RoleIDs = append(w.space.members[rpMember].RoleIDs, rpStaff)
	_, reply := askStates(t, statesApp(w.s, "b.example"), hostedRef(vsSecret))
	if got := reply.Rooms[0]; len(got.States) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// A peer's own user is told what is known of their own call, viewer or not: a mirror that
// heard nobody was there would hang them up.
func TestVoiceStatesAlwaysTellsAPeerAboutItsOwnUser(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsSecret, rpHigh)
	w.join(t, vsSecret, rpMember)
	_, reply := askStates(t, statesApp(w.s, "b.example"), hostedRef(vsSecret))
	got := fids(reply.Rooms[0].States)
	if len(got) != 1 || got[0] != FormatFID(rpMemberB, "b.example") {
		t.Fatalf("got %v", got)
	}
}

// "Nobody can view it" read from a failed lookup would clear a live call on the mirror.
func TestVoiceStatesSaysNothingWhenTheViewersCannotBeRead(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsSecret, rpHigh)
	w.join(t, vsLounge, rpOwner)
	w.space.listFails = true
	status, reply := askStates(t, statesApp(w.s, "b.example"), hostedRef(vsSecret), hostedRef(vsLounge))
	if status != http.StatusOK || len(reply.Rooms) != 1 || reply.Rooms[0].Room.OriginRoomID != id.Format(vsLounge) {
		t.Fatalf("status %d, reply %+v", status, reply)
	}
}

func TestVoiceStatesLeavesOutAnAccountThatIsGone(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsGhost, rpOwner)
	w.join(t, vsGhost, 12345)
	_, reply := askStates(t, statesApp(w.s, "b.example"), hostedRef(vsGhost))
	if got := reply.Rooms[0]; len(got.States) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// A user that could not be read must not read as a user who left: the mirror would hang them up.
func TestVoiceStatesFailsRatherThanAnswerForAnUnreadableUser(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsLounge, rpOwner)
	w.users.failFor[rpOwner] = true
	status, _ := askStates(t, statesApp(w.s, "b.example"), hostedRef(vsLounge))
	if status != http.StatusInternalServerError {
		t.Fatalf("status %d", status)
	}
}

// ---- the pull, mirror side -------------------------------------------------------------------

// pullFixture is b.example asking a.example, which answers with reply.
func pullFixture(t *testing.T, reply func(c fiber.Ctx) error) (*Service, *rooms.RoomWithParticipants) {
	t.Helper()
	app := fiber.New()
	app.Post(BasePath+"/rooms/voice/states", reply)
	s, _ := resyncFixture(t, app, false)
	space := int64(rpMirror)
	s.users.(*profileTestUsers).byID[3] = shadowUser(3, 70, "a.example")
	room := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 1300, SpaceID: &space}}
	room.Federation = &rooms.Federation{OriginDomain: "a.example", OriginID: "300"}
	return s, room
}

func TestRemoteVoiceStatesConvertsTheAnswer(t *testing.T) {
	var asked VoiceStatesRequest
	s, room := pullFixture(t, func(c fiber.Ctx) error {
		_ = json.Unmarshal(c.Body(), &asked)
		return c.JSON(VoiceStatesReply{Rooms: []VoiceRoomStates{
			{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "300"}, States: []VoiceState{{User: Profile{FID: FormatFID(70, "a.example"), Username: "shadow"}, SessionID: "s1"}}},
			{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "301"}, States: []VoiceState{}}, // never asked for
		}})
	})
	got, err := s.RemoteVoiceStates(context.Background(), "a.example", []*rooms.RoomWithParticipants{room})
	if err != nil {
		t.Fatal(err)
	}
	if len(asked.Rooms) != 1 || asked.Rooms[0].OriginRoomID != "300" {
		t.Errorf("asked %+v", asked)
	}
	if len(got) != 1 || got[0].RoomID != 1300 || got[0].Gone || got[0].Incomplete {
		t.Fatalf("got %+v", got)
	}
	if st := got[0].States; len(st) != 1 || st[0].UserID != 3 || st[0].RoomID != 1300 || st[0].SpaceID != rpMirror {
		t.Fatalf("states %+v", st)
	}
}

func TestRemoteVoiceStatesPassesGoneOn(t *testing.T) {
	s, room := pullFixture(t, func(c fiber.Ctx) error {
		return c.JSON(VoiceStatesReply{Rooms: []VoiceRoomStates{{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "300"}, Gone: true, States: []VoiceState{}}}})
	})
	got, err := s.RemoteVoiceStates(context.Background(), "a.example", []*rooms.RoomWithParticipants{room})
	if err != nil || len(got) != 1 || !got[0].Gone {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestRemoteVoiceStatesMarksAnUnreadableStateIncomplete(t *testing.T) {
	s, room := pullFixture(t, func(c fiber.Ctx) error {
		return c.JSON(VoiceStatesReply{Rooms: []VoiceRoomStates{{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "300"}, States: []VoiceState{{User: Profile{FID: "nonsense"}}}}}})
	})
	got, err := s.RemoteVoiceStates(context.Background(), "a.example", []*rooms.RoomWithParticipants{room})
	if err != nil || len(got) != 1 || !got[0].Incomplete {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestRemoteVoiceStatesTakesNoRoomFromAnotherOrigin(t *testing.T) {
	s, room := pullFixture(t, func(c fiber.Ctx) error {
		return c.JSON(VoiceStatesReply{Rooms: []VoiceRoomStates{{Room: RoomRef{OriginDomain: "c.example", OriginRoomID: "300"}, Gone: true}}})
	})
	got, err := s.RemoteVoiceStates(context.Background(), "a.example", []*rooms.RoomWithParticipants{room})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

// Only a 404 for the whole request means an older origin: every other failure is an origin
// not answering, and must count towards the silence limit.
func TestRemoteVoiceStatesFailures(t *testing.T) {
	cases := []struct {
		name    string
		reply   func(c fiber.Ctx) error
		legacy  bool
		wantErr bool
	}{
		{"route not there", func(c fiber.Ctx) error { return c.SendStatus(http.StatusNotFound) }, true, true},
		{"failing", func(c fiber.Ctx) error { return c.SendStatus(http.StatusInternalServerError) }, false, true},
		{"refused", func(c fiber.Ctx) error { return c.SendStatus(http.StatusForbidden) }, false, true},
		{"a 200 saying nothing", func(c fiber.Ctx) error { return c.SendString("{}") }, false, true},
		{"truncated", func(c fiber.Ctx) error { return c.SendString(`{"rooms":[{"room"`) }, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, room := pullFixture(t, tc.reply)
			_, err := s.RemoteVoiceStates(context.Background(), "a.example", []*rooms.RoomWithParticipants{room})
			if err == nil {
				t.Fatal("no error")
			}
			if got := errors.Is(err, voice.ErrOriginPredatesSync); got != tc.legacy {
				t.Fatalf("legacy = %v (%v)", got, err)
			}
		})
	}
}

// ---- moderation --------------------------------------------------------------------------------

func TestVoiceModerationOverTheWire(t *testing.T) {
	ctx := context.Background()
	mirrorKey, originKey := testSigner(t), testSigner(t)
	var ops []string
	cases := []struct {
		name   string
		setup  func(*vsWorld)
		status int // of the error the mirror's moderator sees; 0 for none
		call   func(*Service, *auth.User) error
	}{
		{
			name: "target not in voice", status: http.StatusConflict,
			call: func(m *Service, mod *auth.User) error {
				return m.DisconnectRemoteVoice(ctx, "a.example", rpMirror, mod, rpMemberB)
			},
		},
		{
			name: "voice off at the origin", status: http.StatusServiceUnavailable,
			setup: func(w *vsWorld) { w.s.voice = nil },
			call: func(m *Service, mod *auth.User) error {
				return m.DisconnectRemoteVoice(ctx, "a.example", rpMirror, mod, rpMemberB)
			},
		},
		{
			// A mirror's copy of the roles decides nothing: the origin checks the permission.
			name: "no permission at the origin", status: http.StatusForbidden,
			setup: func(w *vsWorld) { w.join(t, vsLounge, rpMember) },
			call: func(m *Service, mod *auth.User) error {
				return m.DisconnectRemoteVoice(ctx, "a.example", rpMirror, mod, rpMemberB)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := vsOrigin(t)
			if tc.setup != nil {
				tc.setup(w)
			}
			app := rpOriginApp(w.s, mirrorKey, &ops)
			mirror, _ := rpMirrorOf(app, mirrorKey, originKey)
			mirror.repo = &mirrorRoomsRepo{roleFedRepo: mirror.repo.(*roleFedRepo)}
			err := tc.call(mirror, mirror.users.(*profileTestUsers).byID[rpModB])
			var oe *voice.OriginError
			if !errors.As(err, &oe) || oe.Status != tc.status {
				t.Fatalf("err = %v, want status %d", err, tc.status)
			}
		})
	}
}

// The whole trip, and the reason relays ignore the exclusion: b.example asks, a.example acts
// under a context that withholds relays from b.example, and b.example still hears the left.
// Nothing else would ever remove the state there.
func TestDisconnectAskedForByAMirrorIsRelayedBackToIt(t *testing.T) {
	ctx := context.Background()
	mirrorKey, originKey := testSigner(t), testSigner(t)
	w := vsOrigin(t)
	log := recordRelays(w.s)
	for i := range w.space.roles {
		if w.space.roles[i].ID == rpModRole {
			w.space.roles[i].Permissions |= permissions.PermMoveMembers | permissions.PermViewRoom
		}
	}
	w.join(t, vsLounge, rpMember)
	var ops []string
	app := rpOriginApp(w.s, mirrorKey, &ops)
	mirror, _ := rpMirrorOf(app, mirrorKey, originKey)
	mod := mirror.users.(*profileTestUsers).byID[rpModB]
	if err := mirror.DisconnectRemoteVoice(ctx, "a.example", rpMirror, mod, rpMemberB); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0] != "voice.disconnect" {
		t.Fatalf("ops %v", ops)
	}
	if states, _ := w.store.RoomStates(ctx, vsLounge); len(states) != 0 {
		t.Fatalf("still at the origin: %+v", states)
	}
	log.wait(t, 2)
	ev, ok := log.sent["b.example"][0].(VoiceStateEvent)
	if !ok || !ev.Left {
		t.Fatalf("relay to the mirror that asked: %+v", log.sent["b.example"])
	}
}

// mirrorRoomsRepo maps a mirror's room to the origin's id, 1000 lower.
type mirrorRoomsRepo struct{ *roleFedRepo }

func (r *mirrorRoomsRepo) GetRoomMapping(_ context.Context, roomID int64) (*RoomMapping, error) {
	return &RoomMapping{RoomID: roomID, OriginDomain: "a.example", OriginRoomID: roomID - 1000}, nil
}

func TestModerationFromAMirrorNamesTheOriginsRoom(t *testing.T) {
	ctx := context.Background()
	var got manageVoice
	var op string
	app := fiber.New()
	app.Post(BasePath+"/spaces/manage", func(c fiber.Ctx) error {
		var req SpaceManageRequest
		_ = json.Unmarshal(c.Body(), &req)
		op = req.Op
		_ = json.Unmarshal(req.Params, &got)
		return c.JSON(SpaceManageReply{Events: []RelayEvent{}})
	})
	s, _ := resyncFixture(t, app, false)
	mod := &auth.User{ID: 41, Username: "mod"}
	s.users.(*profileTestUsers).byID[41] = mod
	s.users.(*profileTestUsers).byID[3] = shadowUser(3, 70, "a.example")
	mute, dest := true, "1305"
	if err := s.ModerateRemoteVoice(ctx, "a.example", rpMirror, mod, 3, voice.ModerateInput{Mute: &mute, RoomID: &dest}); err != nil {
		t.Fatal(err)
	}
	if op != "voice.moderate" || got.User != FormatFID(70, "a.example") || got.Mute == nil || !*got.Mute || got.Deaf != nil {
		t.Fatalf("op %q, params %+v", op, got)
	}
	if got.Room == nil || *got.Room != "305" {
		t.Fatalf("room %v, want the origin's id", got.Room)
	}
}

// An older origin has no such op: the moderator gets its refusal, not a 500.
func TestModerationAgainstAnOlderOrigin(t *testing.T) {
	app := fiber.New()
	app.Post(BasePath+"/spaces/manage", func(c fiber.Ctx) error {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": errUnknownAction.Error()})
	})
	s, _ := resyncFixture(t, app, false)
	mod := &auth.User{ID: 41, Username: "mod"}
	s.users.(*profileTestUsers).byID[41] = mod
	s.users.(*profileTestUsers).byID[3] = shadowUser(3, 70, "a.example")
	err := s.DisconnectRemoteVoice(context.Background(), "a.example", rpMirror, mod, 3)
	var oe *voice.OriginError
	if !errors.As(err, &oe) || oe.Status != http.StatusBadRequest {
		t.Fatalf("err %v", err)
	}
}

func TestVoiceOriginError(t *testing.T) {
	var oe *voice.OriginError
	if err := voiceOriginError(&spaces.OriginError{Status: 403, Message: "no"}); !errors.As(err, &oe) || oe.Status != 403 || oe.Message != "no" {
		t.Errorf("origin refusal became %v", err)
	}
	if err := voiceOriginError(spaces.ErrOriginUnavailable); !errors.Is(err, voice.ErrOriginUnavailable) {
		t.Errorf("unreachable origin became %v", err)
	}
	if voiceOriginError(nil) != nil {
		t.Error("success became an error")
	}
	if err := voiceOriginError(spaces.ErrNotMember); !errors.Is(err, spaces.ErrNotMember) {
		t.Errorf("a local refusal became %v", err)
	}
}

// ---- running calls in a snapshot ---------------------------------------------------------------

func TestSnapshotNamesRunningCalls(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsLounge, rpMember)
	w.join(t, vsSecret, rpHigh)
	list := []*rooms.Room{w.s.roomRepo.(*vsRooms).byID[vsLounge], w.s.roomRepo.(*vsRooms).byID[vsSecret], w.s.roomRepo.(*vsRooms).byID[vsText]}
	got := w.s.voiceRoomsFor(context.Background(), rpSpace, "b.example", list)
	if len(got) != 1 || got[0].Room.OriginRoomID != id.Format(vsLounge) || len(got[0].States) != 1 {
		t.Fatalf("got %+v: b.example has nobody who can view the private room", got)
	}
}

// What building a whole snapshot reads beyond the view rule.
func (r *vsSpaceRepo) CountMembers(context.Context, int64) (int, error) { return len(r.members), nil }

func (r *vsSpaceRepo) ListEmojis(context.Context, int64) ([]spaces.SpaceEmoji, error) {
	return nil, nil
}

func (r *vsSpaceRepo) ListMembersPage(context.Context, int64, int64, int) ([]spaces.SpaceMember, error) {
	return nil, nil
}

// What a resync writes, none of which these tests look at.
func (r *vsSpaceRepo) UpdateSpaceFields(context.Context, int64, map[string]interface{}) error {
	return nil
}

func (r *vsSpaceRepo) ListInvites(context.Context, int64) ([]spaces.SpaceInvite, error) {
	return nil, nil
}

func (r *vsSpaceRepo) DeleteSpaceRole(context.Context, int64, int64) error { return nil }

func (r *vsRooms) UpdateSpaceRoom(context.Context, int64, string, string, int) error { return nil }

func (r *vsRooms) UpdateSpaceRoomParentAndPosition(context.Context, int64, int64, *int64, int) error {
	return nil
}

func (r *vsRooms) UpdateRoomPermissionsSynced(context.Context, int64, bool) error { return nil }

func (r *vsRooms) UpdateVoiceSettings(context.Context, int64, int, int) error { return nil }

func (r *vsRooms) UpdateRoomE2EEEnabled(context.Context, int64, bool) error { return nil }

func TestWholeSnapshotCarriesRunningCalls(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsLounge, rpMember)
	snap, err := w.s.snapshot(context.Background(), &spaces.Space{ID: rpSpace}, "b.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.VoiceRooms) != 1 || snap.VoiceRooms[0].Room.OriginRoomID != id.Format(vsLounge) {
		t.Fatalf("voice rooms %+v", snap.VoiceRooms)
	}
}

func TestSnapshotWithNothingRunningSaysNothing(t *testing.T) {
	w := vsOrigin(t)
	list := []*rooms.Room{w.s.roomRepo.(*vsRooms).byID[vsLounge]}
	if got := w.s.voiceRoomsFor(context.Background(), rpSpace, "b.example", list); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	raw, _ := json.Marshal(SpaceSnapshot{})
	if strings.Contains(string(raw), "voice_rooms") {
		t.Errorf("wire %s", raw)
	}
}

func TestSnapshotWithoutVoiceHereSaysNothing(t *testing.T) {
	w := vsOrigin(t)
	w.join(t, vsLounge, rpMember)
	w.s.voice = nil
	list := []*rooms.Room{w.s.roomRepo.(*vsRooms).byID[vsLounge]}
	if got := w.s.voiceRoomsFor(context.Background(), rpSpace, "b.example", list); got != nil {
		t.Fatalf("got %+v", got)
	}
}

// The client reads 4 MiB of a snapshot, and a join that cannot read its reply fails.
func TestSnapshotCapsItsCalls(t *testing.T) {
	w := vsOrigin(t)
	big := strings.Repeat("x", maxSnapshotVoiceBytes/2+1)
	w.users.byID[rpMember].Bio = big
	w.users.byID[rpMod].Bio = big
	w.join(t, vsLounge, rpMember)
	w.join(t, vsGhost, rpMod)
	repo := w.s.roomRepo.(*vsRooms)
	got := w.s.voiceRoomsFor(context.Background(), rpSpace, "b.example", []*rooms.Room{repo.byID[vsLounge], repo.byID[vsGhost]})
	if len(got) != 1 {
		t.Fatalf("got %d rooms", len(got))
	}
}

func TestVoiceStatesCapsItsAnswer(t *testing.T) {
	w := vsOrigin(t)
	big := strings.Repeat("x", maxVoiceSyncBytes/2+1)
	w.users.byID[rpMember].Bio = big
	w.users.byID[rpMod].Bio = big
	w.join(t, vsLounge, rpMember)
	w.join(t, vsGhost, rpMod)
	status, reply := askStates(t, statesApp(w.s, "b.example"), hostedRef(vsLounge), hostedRef(vsGhost))
	if status != http.StatusOK || len(reply.Rooms) != 1 {
		t.Fatalf("status %d, %d rooms", status, len(reply.Rooms))
	}
}

// applyFixture is b.example mirroring the origin's rooms as id+1000, voice on.
func applyFixture(t *testing.T) (*Service, *voice.Store) {
	t.Helper()
	s, store, _, _ := vsMirror(t)
	return s, store
}

func applyFixtureWithEvents(t *testing.T) (*Service, *voice.Store, *redistest.Redis) {
	t.Helper()
	s, store, mem, _ := vsMirror(t)
	return s, store, mem
}

func vsMirror(t *testing.T) (*Service, *voice.Store, *redistest.Redis, *vsSpaceRepo) {
	t.Helper()
	s, _ := mirrorWireFixture()
	users := &vsUsers{profileTestUsers: s.users.(*profileTestUsers), failFor: map[int64]bool{}}
	users.byID[3] = shadowUser(3, 70, "a.example")
	s.users = users
	space := int64(rpMirror)
	roomRepo := &vsRooms{byID: map[int64]*rooms.Room{
		1300: {ID: 1300, Type: rooms.TypeSpaceVoice, SpaceID: &space},
		1301: {ID: 1301, Type: rooms.TypeSpaceVoice, SpaceID: &space},
	}}
	s.roomRepo = roomRepo
	repo := &vsSpaceRepo{roleRepo: newRoleRepo(&spaces.Space{ID: rpMirror, OwnerID: rpOwnerB, EveryoneRoleID: rpEveryone}, rpRoles(rpMirror, 0), map[int64]*spaces.SpaceMember{})}
	s.spaceSvc = spaces.NewService(repo, roomRepo, users, nil, nil)
	s.spaceSvc.SetFederator(s)
	s.roomSvc = rooms.NewService(roomRepo, users, nil, nil, nil, s.spaceSvc)
	rdb, mem := redistest.New()
	vstore := voice.NewStore(rdb, "")
	s.voice = voice.NewService(vstore, nil, s.roomSvc, s.spaceSvc, users, rdb, nil, nil)
	return s, vstore, mem, repo
}

func TestApplyVoiceRoomsDiscoversACall(t *testing.T) {
	ctx := context.Background()
	s, store := applyFixture(t)
	wire := VoiceState{User: Profile{FID: FormatFID(70, "a.example"), Username: "shadow"}, SessionID: "s1", Identity: "@70:a.example.s1"}
	s.applyVoiceRooms(ctx, "a.example", []VoiceRoomStates{
		{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "300"}, States: []VoiceState{wire}},
		{Room: RoomRef{OriginDomain: "c.example", OriginRoomID: "301"}, States: []VoiceState{wire}}, // not its to name
		{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "x"}, States: []VoiceState{wire}},
	}, time.Now())
	states, err := store.RoomStates(ctx, 1300)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].UserID != 3 || states[0].SpaceID != rpMirror {
		t.Fatalf("states %+v", states)
	}
	if other, _ := store.RoomStates(ctx, 1301); len(other) != 0 {
		t.Fatalf("wrote a room another origin named: %+v", other)
	}
}

// The relayed left of a move reaches the mirror's clients with the mirror's id for the room.
func TestVoiceStateHandlerTranslatesTheMove(t *testing.T) {
	ctx := context.Background()
	s, store, mem := applyFixtureWithEvents(t)
	st := &voice.State{UserID: 3, RoomID: 1300, SpaceID: rpMirror, SessionID: "s1", Ident: "@70:a.example.s1", IssuedAt: time.Now().Add(-time.Hour)}
	if err := store.PutState(ctx, st); err != nil {
		t.Fatal(err)
	}
	app := voiceRoute("a.example", NewHandler(s).VoiceState)
	to := RoomRef{OriginDomain: "a.example", OriginRoomID: "301"}
	status := post(t, app, VoiceStateEvent{
		Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "300"}, Left: true, MovedTo: &to,
		State: VoiceState{User: Profile{FID: FormatFID(70, "a.example"), Username: "shadow"}, SessionID: "s1", Identity: "@70:a.example.s1"},
	})
	if status != http.StatusNoContent {
		t.Fatalf("status %d", status)
	}
	if states, _ := store.RoomStates(ctx, 1300); len(states) != 0 {
		t.Fatalf("states %+v", states)
	}
	events := mem.Published()
	if len(events) != 1 || events[0].Data["moved_to"] != "1301" || events[0].Data["left"] != true {
		t.Fatalf("events %+v", events)
	}
}

func TestApplyVoiceRoomsWithoutVoiceHere(t *testing.T) {
	s, _ := applyFixture(t)
	s.voice = nil
	s.applyVoiceRooms(context.Background(), "a.example", []VoiceRoomStates{{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "300"}}}, time.Now())
}

// What a join is answered with reaches the voice store, on the path a member takes into a
// space this instance already mirrors and the Discover path alike.
func TestJoiningASpaceDiscoversItsCalls(t *testing.T) {
	ctx := context.Background()
	wire := VoiceState{User: Profile{FID: FormatFID(70, "a.example"), Username: "shadow"}, SessionID: "s1", Identity: "@70:a.example.s1"}
	snap := SpaceSnapshot{
		Space:      SpaceRef{OriginDomain: "a.example", OriginSpaceID: id.Format(rpSpace)},
		VoiceRooms: []VoiceRoomStates{{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "300"}, States: []VoiceState{wire}}},
	}
	app := fiber.New()
	answer := func(c fiber.Ctx) error { return c.JSON(snap) }
	app.Post(BasePath+"/spaces/join", answer)
	app.Post(BasePath+"/spaces/join_listed", answer)
	joins := map[string]func(*Service, *auth.User) error{
		"invite": func(s *Service, user *auth.User) error {
			_, err := s.JoinRemoteSpace(ctx, "a.example", "code", user)
			return err
		},
		"discover": func(s *Service, user *auth.User) error {
			_, err := s.JoinListedSpace(ctx, "a.example", rpSpace, user.ID)
			return err
		},
	}
	for name, join := range joins {
		t.Run(name, func(t *testing.T) {
			s, store, _, space := vsMirror(t)
			key, originKey := testSigner(t), testSigner(t)
			s.client = &Client{cfg: s.fcfg, signer: key, discovery: knownPeer(s.fcfg, "a.example", originKey), http: &http.Client{Transport: appTransport{app}}}
			user := &auth.User{ID: rpMemberB, Username: "member"}
			s.users.(*vsUsers).byID[rpMemberB] = user
			space.members[rpMemberB] = rpMemberRow(rpMirror, rpMemberB)
			if err := join(s, user); err != nil {
				t.Fatal(err)
			}
			states, err := store.RoomStates(ctx, 1300)
			if err != nil {
				t.Fatal(err)
			}
			if len(states) != 1 || states[0].UserID != 3 {
				t.Fatalf("states %+v", states)
			}
		})
	}
}

// A resync is what finds a call running after this instance restarted, or lost its Redis.
func TestResyncDiscoversACall(t *testing.T) {
	ctx := context.Background()
	wire := VoiceState{User: Profile{FID: FormatFID(70, "a.example"), Username: "shadow"}, SessionID: "s1", Identity: "@70:a.example.s1"}
	snap := SpaceSnapshot{
		Space: SpaceRef{OriginDomain: "a.example", OriginSpaceID: id.Format(rpSpace)},
		Info:  SpaceInfo{Name: "s", Owner: FormatFID(1, "a.example"), EveryoneRole: id.Format(rpEveryone)},
		Rooms: []SpaceRoomWire{
			{ID: "300", Type: rooms.TypeSpaceVoice, Name: "voice"},
			{ID: "301", Type: rooms.TypeSpaceVoice, Name: "voice"},
		},
		Members:     []SpaceMemberWire{{User: Profile{FID: FormatFID(rpMemberB, "b.example"), Username: "member"}, Roles: []string{id.Format(rpEveryone)}}},
		MemberCount: 1,
		VoiceRooms:  []VoiceRoomStates{{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: "300"}, States: []VoiceState{wire}}},
	}
	app := fiber.New()
	app.Post(BasePath+"/spaces/sync", func(c fiber.Ctx) error { return c.JSON(snap) })
	s, store, _, repo := vsMirror(t)
	key, originKey := testSigner(t), testSigner(t)
	s.client = &Client{cfg: s.fcfg, signer: key, discovery: knownPeer(s.fcfg, "a.example", originKey), http: &http.Client{Transport: appTransport{app}}}
	repo.members[rpMemberB] = rpMemberRow(rpMirror, rpMemberB)
	s.users.(*vsUsers).byID[1] = shadowUser(1, 1, "a.example")
	s.users.(*vsUsers).byID[rpMemberB] = &auth.User{ID: rpMemberB, Username: "member"}
	m := &SpaceMapping{SpaceID: rpMirror, OriginDomain: "a.example", OriginSpaceID: rpSpace}
	if err := s.ResyncMirror(ctx, m); err != nil {
		t.Fatal(err)
	}
	states, err := store.RoomStates(ctx, 1300)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].UserID != 3 {
		t.Fatalf("states %+v", states)
	}
}

// An older peer's JSON means what it always did.
func TestOlderPeersWire(t *testing.T) {
	var ev VoiceStateEvent
	if err := json.Unmarshal([]byte(`{"room":{"origin_domain":"a.example","origin_room_id":"5"},"state":{"session_id":"s1"},"left":true}`), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.MovedTo != nil || !ev.Left {
		t.Errorf("event %+v", ev)
	}
	var snap SpaceSnapshot
	if err := json.Unmarshal([]byte(`{"space":{},"info":{},"members":[]}`), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.VoiceRooms != nil {
		t.Errorf("snapshot %+v", snap)
	}
}
