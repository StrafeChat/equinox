package voice

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/redistest"
)

// The fixture for what crosses instances: the real Store and the real spaces and rooms services
// on an in-memory Redis and in-memory repositories, a stub LiveKit, and every gateway event and
// federation call recorded. This instance is a.local (see fakeFed); a test picks a room hosted
// here or on b.local.

const (
	wSpace    = 500
	wEveryone = 600
	wStaff    = 601

	wLounge = 10 // an open voice room
	wStage  = 11 // another
	wSecret = 12 // @everyone denied View, Staff allowed

	wOwner  = 1 // a.local's own, owns the space
	wAlice  = 2 // a.local's own, Staff
	wCarol  = 3 // a.local's own, no role
	wRemote = 4 // a shadow of b.local's user 40, no role
)

// fakeLiveKit is a stub of LiveKit's room service: it says who is in a room and records who it
// was told to remove.
type fakeLiveKit struct {
	mu      sync.Mutex
	present map[string][]string // LiveKit room name -> identities
	removed []string
	listed  int
}

func (f *fakeLiveKit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/protobuf")
	body := make([]byte, 0, 256)
	buf := make([]byte, 256)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/ListParticipants"):
		f.listed++
		var req livekit.ListParticipantsRequest
		_ = proto.Unmarshal(body, &req)
		res := &livekit.ListParticipantsResponse{}
		for _, ident := range f.present[req.Room] {
			res.Participants = append(res.Participants, &livekit.ParticipantInfo{Identity: ident})
		}
		raw, _ := proto.Marshal(res)
		_, _ = w.Write(raw)
	case strings.HasSuffix(r.URL.Path, "/RemoveParticipant"):
		var req livekit.RoomParticipantIdentity
		_ = proto.Unmarshal(body, &req)
		f.removed = append(f.removed, req.Identity)
	}
}

// recordingFed is fakeFed with a script: rooms on b.local are mirrored, and what is asked of
// the origin is recorded.
type recordingFed struct {
	fakeFed
	mu       sync.Mutex
	mirrored map[int64]bool // spaces hosted on b.local

	asks    [][]int64 // the rooms of each RemoteVoiceStates, in order
	answer  func(list []*rooms.RoomWithParticipants) ([]RoomSync, error)
	join    func() (*RemoteJoin, error)
	leaves  []string // "<room>:<user>:<session>"
	relays  []string // "state:<room>:<user>", "left:<room>:<user>", "moved:<room>:<user>:<dest>"
	forward []string // "moderate:<target>", "disconnect:<target>"
	reply   error
}

func (f *recordingFed) SpaceFederation(_ context.Context, spaceID int64) *spaces.SpaceFederation {
	if f.mirrored[spaceID] {
		return &spaces.SpaceFederation{OriginDomain: "b.local", OriginID: "9"}
	}
	return nil
}

func (f *recordingFed) JoinRemoteVoice(context.Context, string, *rooms.RoomWithParticipants, *auth.User, JoinInput) (*RemoteJoin, error) {
	return f.join()
}

func (f *recordingFed) LeaveRemoteVoiceSession(_ context.Context, _ string, room *rooms.RoomWithParticipants, user *auth.User, sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaves = append(f.leaves, id.Format(room.ID)+":"+id.Format(user.ID)+":"+sessionID)
}

func (f *recordingFed) ModerateRemoteVoice(_ context.Context, _ string, _ int64, _ *auth.User, targetID int64, _ ModerateInput) error {
	f.forward = append(f.forward, "moderate:"+id.Format(targetID))
	return f.reply
}

func (f *recordingFed) DisconnectRemoteVoice(_ context.Context, _ string, _ int64, _ *auth.User, targetID int64) error {
	f.forward = append(f.forward, "disconnect:"+id.Format(targetID))
	return f.reply
}

func (f *recordingFed) RemoteVoiceStates(_ context.Context, _ string, list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
	ids := make([]int64, 0, len(list))
	for _, room := range list {
		ids = append(ids, room.ID)
	}
	f.mu.Lock()
	f.asks = append(f.asks, ids)
	f.mu.Unlock()
	return f.answer(list)
}

func (f *recordingFed) AfterVoiceStateChanged(_ context.Context, room *rooms.RoomWithParticipants, st *State, left bool) {
	what := "state:"
	if left {
		what = "left:"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relays = append(f.relays, what+id.Format(room.ID)+":"+id.Format(st.UserID))
}

func (f *recordingFed) AfterVoiceStateMoved(_ context.Context, room *rooms.RoomWithParticipants, st *State, dest *rooms.RoomWithParticipants) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relays = append(f.relays, "moved:"+id.Format(room.ID)+":"+id.Format(st.UserID)+":"+id.Format(dest.ID))
}

// worldRepo is the slice of spaces.Repository the view rule reads.
type worldRepo struct {
	spaces.Repository
	mu       sync.Mutex
	space    *spaces.Space
	members  map[int64][]int64 // user -> roles
	roles    []spaces.SpaceRole
	overs    map[int64][]spaces.SpaceRoomRoleOverride
	failRead bool
}

var errRepoDown = errors.New("scylla: read failed")

func (r *worldRepo) GetByID(context.Context, int64) (*spaces.Space, error) {
	if r.failRead {
		return nil, errRepoDown
	}
	cp := *r.space
	return &cp, nil
}

func (r *worldRepo) ListSpaceRoles(context.Context, int64) ([]spaces.SpaceRole, error) {
	return append([]spaces.SpaceRole(nil), r.roles...), nil
}

func (r *worldRepo) ListRoomRoleOverrides(_ context.Context, _, roomID int64) ([]spaces.SpaceRoomRoleOverride, error) {
	return r.overs[roomID], nil
}

func (r *worldRepo) ListRoomUserOverrides(context.Context, int64, int64) ([]spaces.SpaceRoomUserOverride, error) {
	return nil, nil
}

func (r *worldRepo) ListMembers(context.Context, int64) ([]spaces.SpaceMember, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int64, 0, len(r.members))
	for uid := range r.members {
		ids = append(ids, uid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]spaces.SpaceMember, 0, len(ids))
	for _, uid := range ids {
		out = append(out, spaces.SpaceMember{SpaceID: wSpace, UserID: uid, RoleIDs: r.members[uid]})
	}
	return out, nil
}

func (r *worldRepo) MemberRoleIDs(_ context.Context, _, userID int64) ([]int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	roles, ok := r.members[userID]
	return roles, ok, nil
}

// worldRooms is the slice of rooms.Repository LoadRoom and the snapshot read.
type worldRooms struct {
	rooms.Repository
	mu      sync.Mutex
	byID    map[int64]*rooms.Room
	failFor map[int64]bool
}

func (r *worldRooms) GetByID(_ context.Context, roomID int64) (*rooms.Room, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failFor[roomID] {
		return nil, errRepoDown
	}
	room := r.byID[roomID]
	if room == nil {
		return nil, nil
	}
	cp := *room
	return &cp, nil
}

func (r *worldRooms) GetByIDs(_ context.Context, ids []int64) ([]*rooms.Room, error) {
	out := make([]*rooms.Room, len(ids))
	for i, rid := range ids {
		if room := r.byID[rid]; room != nil {
			cp := *room
			out[i] = &cp
		}
	}
	return out, nil
}

func (r *worldRooms) GetParticipants(context.Context, int64) ([]int64, error) { return nil, nil }

func (r *worldRooms) ListBySpace(context.Context, int64) ([]rooms.RoomBySpaceRow, error) {
	out := make([]rooms.RoomBySpaceRow, 0, len(r.byID))
	for rid, room := range r.byID {
		if room.SpaceID != nil {
			out = append(out, rooms.RoomBySpaceRow{RoomID: rid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoomID < out[j].RoomID })
	return out, nil
}

func (r *worldRooms) ListThreadMemberIDs(context.Context, int64) ([]int64, error) { return nil, nil }

type worldUsers struct {
	auth.UserRepository
	byID map[int64]*auth.User
}

func (u *worldUsers) GetByID(_ context.Context, uid int64) (*auth.User, error) {
	return u.byID[uid], nil
}

func (u *worldUsers) GetByIDs(_ context.Context, ids []int64) ([]*auth.User, error) {
	out := make([]*auth.User, len(ids))
	for i, uid := range ids {
		out[i] = u.byID[uid]
	}
	return out, nil
}

// fedInfo answers LoadRoom's federation lookup from the world: origin per room, or unreadable.
type fedInfo struct {
	mu     sync.Mutex
	origin map[int64]string
}

func (f *fedInfo) RoomFederation(_ context.Context, roomID int64) *rooms.Federation {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.origin[roomID]; ok {
		return &rooms.Federation{OriginDomain: d, OriginID: id.Format(roomID + 1000)}
	}
	return nil
}

type world struct {
	t     *testing.T
	svc   *Service
	store *Store
	mem   *redistest.Redis
	repo  *worldRepo
	rooms *worldRooms
	users *worldUsers
	fed   *recordingFed
	info  *fedInfo
	lk    *fakeLiveKit
}

// newWorld is a space hosted on a.local, federated: the rooms have mappings naming this
// instance. mirror() makes it b.local's instead.
func newWorld(t *testing.T) *world {
	t.Helper()
	rdb, mem := redistest.New()
	cfg := &config.Config{}

	space := int64(wSpace)
	voiceRoom := func(roomID int64) *rooms.Room {
		return &rooms.Room{ID: roomID, Type: rooms.TypeSpaceVoice, SpaceID: &space, Name: "voice"}
	}
	roomRepo := &worldRooms{byID: map[int64]*rooms.Room{wLounge: voiceRoom(wLounge), wStage: voiceRoom(wStage), wSecret: voiceRoom(wSecret)}, failFor: map[int64]bool{}}
	repo := &worldRepo{
		space: &spaces.Space{ID: wSpace, OwnerID: wOwner, EveryoneRoleID: wEveryone},
		members: map[int64][]int64{
			wOwner: {wEveryone}, wAlice: {wEveryone, wStaff}, wCarol: {wEveryone}, wRemote: {wEveryone},
		},
		roles: []spaces.SpaceRole{
			{SpaceID: wSpace, ID: wEveryone, Name: spaces.EveryoneRoleName, Permissions: permissions.DefaultEveryone},
			{SpaceID: wSpace, ID: wStaff, Name: "Staff", Position: 1},
		},
		overs: map[int64][]spaces.SpaceRoomRoleOverride{wSecret: {
			{SpaceID: wSpace, RoomID: wSecret, RoleID: wEveryone, Deny: permissions.PermViewRoom},
			{SpaceID: wSpace, RoomID: wSecret, RoleID: wStaff, Allow: permissions.PermViewRoom},
		}},
	}
	remoteID := int64(40)
	users := &worldUsers{byID: map[int64]*auth.User{
		wOwner: {ID: wOwner, Username: "owner"}, wAlice: {ID: wAlice, Username: "alice"}, wCarol: {ID: wCarol, Username: "carol"},
		wRemote: {ID: wRemote, Username: "bob", HomeDomain: "b.local", RemoteID: &remoteID},
	}}
	spaceSvc := spaces.NewService(repo, roomRepo, users, rdb, cfg)
	roomSvc := rooms.NewService(roomRepo, users, rdb, cfg, nil, spaceSvc)
	info := &fedInfo{origin: map[int64]string{wLounge: "a.local", wStage: "a.local", wSecret: "a.local"}}
	roomSvc.SetFederationInfo(info)

	lk := &fakeLiveKit{present: map[string][]string{}}
	srv := httptest.NewServer(lk)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().(*net.TCPAddr)

	store := NewStore(rdb, "")
	svc := NewService(store, NewLiveKit(config.VoiceConfig{InternalURL: "http://127.0.0.1:" + strconv.Itoa(addr.Port), APIKey: "k", APISecret: "secretsecretsecretsecretsecretsecret"}), roomSvc, spaceSvc, users, rdb, cfg, nil)
	fed := &recordingFed{mirrored: map[int64]bool{}}
	svc.SetFederator(fed)
	return &world{t: t, svc: svc, store: store, mem: mem, repo: repo, rooms: roomRepo, users: users, fed: fed, info: info, lk: lk}
}

// mirror makes the space (and every room in it) one b.local hosts.
func (w *world) mirror() *world {
	w.fed.mirrored[wSpace] = true
	w.info.mu.Lock()
	defer w.info.mu.Unlock()
	for rid := range w.info.origin {
		w.info.origin[rid] = "b.local"
	}
	return w
}

func (w *world) room(roomID int64) *rooms.RoomWithParticipants {
	w.t.Helper()
	room, err := w.svc.rooms.LoadRoom(context.Background(), roomID)
	if err != nil {
		w.t.Fatal(err)
	}
	return room
}

// state is a mirrored state as it would arrive, freshly heard.
func (w *world) state(roomID, userID int64, session string) *State {
	return &State{UserID: userID, RoomID: roomID, SpaceID: wSpace, SessionID: session, Ident: "@" + itoa(userID) + ":a.local." + session, JoinedAt: time.Now().UTC(), IssuedAt: time.Now().UTC()}
}

// put writes a state heard some time ago.
func (w *world) put(st *State) *State {
	w.t.Helper()
	st.IssuedAt = time.Now().UTC().Add(-time.Hour)
	if err := w.store.PutState(context.Background(), st); err != nil {
		w.t.Fatal(err)
	}
	return st
}

// held is who the room holds, as "user:session", sorted.
func (w *world) held(roomID int64) []string {
	w.t.Helper()
	states, err := w.store.RoomStates(context.Background(), roomID)
	if err != nil {
		w.t.Fatal(err)
	}
	out := make([]string, 0, len(states))
	for i := range states {
		out = append(out, id.Format(states[i].UserID)+":"+states[i].SessionID)
	}
	sort.Strings(out)
	return out
}

func (w *world) active(roomID int64) bool {
	w.t.Helper()
	ids, err := w.store.ActiveRooms(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	for _, rid := range ids {
		if rid == roomID {
			return true
		}
	}
	return false
}

func (w *world) events() []redistest.Event { return w.mem.Published() }

// sweep runs reconcile as the next replica to tick: the lock from the last one has expired.
func (w *world) sweep() {
	w.mem.DeleteString(w.store.syncKey("lock", "b.local"), w.store.syncKey("lock", "c.local"))
	w.svc.reconcile(context.Background())
}

// voiceEvents narrows events to one room's VOICE_STATE_UPDATEs.
func (w *world) voiceEvents(roomID int64) []redistest.Event {
	var out []redistest.Event
	for _, e := range w.events() {
		if e.Type == EventVoiceStateUpdate && e.Data["room_id"] == id.Format(roomID) {
			out = append(out, e)
		}
	}
	return out
}

func eq[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
