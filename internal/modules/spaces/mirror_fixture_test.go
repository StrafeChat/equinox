package spaces

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// The fixture for who hears about which room: the role fixture's space, members and roles with
// rooms, overrides, invites and a working snapshot cache behind them, and every gateway event
// recorded with who it went to.

// visRepo is orderRepo plus the override, member-list and invite reads and writes rooms need.
type visRepo struct {
	*orderRepo
	roleOv map[int64]map[int64]SpaceRoomRoleOverride // room -> role -> override
	userOv map[int64]map[int64]SpaceRoomUserOverride // room -> user -> override
	// overrideWrites counts every upsert and delete of an override.
	overrideWrites int
	invites        []SpaceInvite
	// joinDates is what AddMemberAt was given, by user.
	joinDates map[int64]time.Time
}

func newVisRepo() *visRepo {
	return &visRepo{
		orderRepo: roleFixtureRepo(),
		roleOv:    map[int64]map[int64]SpaceRoomRoleOverride{},
		userOv:    map[int64]map[int64]SpaceRoomUserOverride{},
		joinDates: map[int64]time.Time{},
	}
}

func (r *visRepo) ListMembers(_ context.Context, spaceID int64) ([]SpaceMember, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int64, 0, len(r.members[spaceID]))
	for uid := range r.members[spaceID] {
		ids = append(ids, uid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]SpaceMember, 0, len(ids))
	for _, uid := range ids {
		m := *r.members[spaceID][uid]
		m.RoleIDs = append([]int64(nil), m.RoleIDs...)
		out = append(out, m)
	}
	return out, nil
}

func (r *visRepo) AddMemberAt(_ context.Context, spaceID, userID int64, roleIDs []int64, _ MemberProfile, joinedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.members[spaceID][userID] = &SpaceMember{SpaceID: spaceID, UserID: userID, RoleIDs: setOf(roleIDs), JoinedAt: joinedAt}
	r.joinDates[userID] = joinedAt
	return nil
}

func (r *visRepo) ListRoomRoleOverrides(_ context.Context, _, roomID int64) ([]SpaceRoomRoleOverride, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SpaceRoomRoleOverride
	for _, o := range r.roleOv[roomID] {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoleID < out[j].RoleID })
	return out, nil
}

func (r *visRepo) ListRoomUserOverrides(_ context.Context, _, roomID int64) ([]SpaceRoomUserOverride, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SpaceRoomUserOverride
	for _, o := range r.userOv[roomID] {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

func (r *visRepo) UpsertRoomRoleOverride(_ context.Context, o *SpaceRoomRoleOverride) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrideWrites++
	if r.roleOv[o.RoomID] == nil {
		r.roleOv[o.RoomID] = map[int64]SpaceRoomRoleOverride{}
	}
	r.roleOv[o.RoomID][o.RoleID] = *o
	return nil
}

func (r *visRepo) DeleteRoomRoleOverride(_ context.Context, _, roomID, roleID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrideWrites++
	delete(r.roleOv[roomID], roleID)
	return nil
}

func (r *visRepo) UpsertRoomUserOverride(_ context.Context, o *SpaceRoomUserOverride) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrideWrites++
	if r.userOv[o.RoomID] == nil {
		r.userOv[o.RoomID] = map[int64]SpaceRoomUserOverride{}
	}
	r.userOv[o.RoomID][o.UserID] = *o
	return nil
}

func (r *visRepo) DeleteRoomUserOverride(_ context.Context, _, roomID, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrideWrites++
	delete(r.userOv[roomID], userID)
	return nil
}

func (r *visRepo) ListEmojis(context.Context, int64) ([]SpaceEmoji, error) { return nil, nil }

func (r *visRepo) UpdateEveryoneRoleID(_ context.Context, spaceID, roleID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spaces[spaceID].EveryoneRoleID = roleID
	return nil
}

func (r *visRepo) ListInvites(context.Context, int64) ([]SpaceInvite, error) {
	return append([]SpaceInvite(nil), r.invites...), nil
}

func (r *visRepo) GetInviteByCode(_ context.Context, code string) (*SpaceInvite, error) {
	for i := range r.invites {
		if r.invites[i].Code == code {
			cp := r.invites[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *visRepo) CreateInvite(_ context.Context, inv *SpaceInvite) error {
	r.invites = append(r.invites, *inv)
	return nil
}

func (r *visRepo) DeleteInvite(_ context.Context, _ int64, code string) error {
	kept := r.invites[:0]
	for _, inv := range r.invites {
		if inv.Code != code {
			kept = append(kept, inv)
		}
	}
	r.invites = kept
	return nil
}

// UpdateSpaceFields writes the columns the mirror paths set.
func (r *visRepo) UpdateSpaceFields(_ context.Context, spaceID int64, set map[string]interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sp := r.spaces[spaceID]
	if sp == nil {
		return nil
	}
	if v, ok := set["everyone_role_id"].(int64); ok {
		sp.EveryoneRoleID = v
	}
	if v, ok := set["widget_invite_code"].(string); ok {
		sp.WidgetInviteCode = v
	}
	if v, ok := set["birthday_message"].(string); ok {
		sp.BirthdayMessage = v
	}
	if v, ok := set["birthday_channel_id"].(int64); ok {
		sp.BirthdayChannelID = &v
	}
	return nil
}

// visRooms is a room repository in memory. failOn makes the single-row read of that room fail
// (the batched read a snapshot is built from still works); writes names what was written, as
// "<what>:<room>".
type visRooms struct {
	rooms.Repository
	mu      sync.Mutex
	byID    map[int64]*rooms.Room
	members map[int64][]int64 // thread -> its members
	writes  []string
	failOn  int64
	// beforeCreate runs ahead of every room created: something else at work on the space
	// meanwhile, such as a gateway permission check rebuilding the snapshot.
	beforeCreate func()
}

var errRoomRead = errors.New("scylla: room read failed")

func newVisRooms() *visRooms {
	return &visRooms{byID: map[int64]*rooms.Room{}, members: map[int64][]int64{}}
}

func (r *visRooms) add(room rooms.Room) {
	space := int64(rfSpace)
	room.SpaceID = &space
	r.byID[room.ID] = &room
}

func (r *visRooms) wrote(what string, roomID int64) {
	r.writes = append(r.writes, what+":"+id.Format(roomID))
}

func (r *visRooms) GetByID(_ context.Context, roomID int64) (*rooms.Room, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOn == roomID {
		return nil, errRoomRead
	}
	room := r.byID[roomID]
	if room == nil {
		return nil, nil
	}
	cp := *room
	return &cp, nil
}

func (r *visRooms) GetByIDs(_ context.Context, ids []int64) ([]*rooms.Room, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*rooms.Room, len(ids))
	for i, rid := range ids {
		if room := r.byID[rid]; room != nil {
			cp := *room
			out[i] = &cp
		}
	}
	return out, nil
}

func (r *visRooms) ListBySpace(context.Context, int64) ([]rooms.RoomBySpaceRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]rooms.RoomBySpaceRow, 0, len(r.byID))
	for rid, room := range r.byID {
		out = append(out, rooms.RoomBySpaceRow{RoomID: rid, Position: room.Position})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoomID < out[j].RoomID })
	return out, nil
}

func (r *visRooms) CreateSpaceRoom(_ context.Context, room *rooms.Room) error {
	if r.beforeCreate != nil {
		r.beforeCreate()
	}
	r.wrote("create", room.ID)
	cp := *room
	r.byID[room.ID] = &cp
	return nil
}

func (r *visRooms) CreateThread(ctx context.Context, room *rooms.Room) error {
	return r.CreateSpaceRoom(ctx, room)
}

func (r *visRooms) UpdateSpaceRoom(_ context.Context, roomID int64, name, topic string, slowmode int) error {
	r.wrote("meta", roomID)
	room := r.byID[roomID]
	room.Name, room.Topic, room.SlowmodeSeconds = name, topic, slowmode
	return nil
}

func (r *visRooms) UpdateRoomE2EEEnabled(_ context.Context, roomID int64, enabled bool) error {
	r.wrote("e2ee", roomID)
	r.byID[roomID].E2EEEnabled = &enabled
	return nil
}

func (r *visRooms) UpdateVoiceSettings(_ context.Context, roomID int64, limit, bitrate int) error {
	r.wrote("voice", roomID)
	r.byID[roomID].UserLimit, r.byID[roomID].Bitrate = limit, bitrate
	return nil
}

func (r *visRooms) UpdateRoomPermissionsSynced(_ context.Context, roomID int64, synced bool) error {
	r.wrote("synced", roomID)
	r.byID[roomID].PermissionsSynced = &synced
	return nil
}

func (r *visRooms) UpdateSpaceRoomParentAndPosition(_ context.Context, _, roomID int64, parent *int64, position int) error {
	r.wrote("parent", roomID)
	r.byID[roomID].ParentID, r.byID[roomID].Position = parent, position
	return nil
}

func (r *visRooms) UpdateSpaceRoomPosition(_ context.Context, _, roomID int64, position int) error {
	r.byID[roomID].Position = position
	return nil
}

func (r *visRooms) UpdateThread(_ context.Context, roomID int64, p rooms.ThreadPatch) error {
	r.wrote("thread", roomID)
	if p.Archived != nil {
		r.byID[roomID].ThreadArchived = p.Archived
	}
	if p.LastActiveAt != nil {
		r.byID[roomID].ThreadLastActiveAt = p.LastActiveAt
	}
	return nil
}

func (r *visRooms) DeleteSpaceRoom(_ context.Context, _, roomID int64) error {
	r.wrote("delete-room", roomID)
	delete(r.byID, roomID)
	return nil
}

func (r *visRooms) DeleteThread(_ context.Context, _, roomID int64) error {
	r.wrote("delete-thread", roomID)
	delete(r.byID, roomID)
	delete(r.members, roomID)
	return nil
}

func (r *visRooms) ListThreadMemberIDs(_ context.Context, roomID int64) ([]int64, error) {
	return append([]int64(nil), r.members[roomID]...), nil
}

func (r *visRooms) IsThreadMember(_ context.Context, roomID, userID int64) (bool, error) {
	for _, uid := range r.members[roomID] {
		if uid == userID {
			return true, nil
		}
	}
	return false, nil
}

func (r *visRooms) ListUserThreadIDs(_ context.Context, userID int64) ([]int64, error) {
	var out []int64
	for tid, ids := range r.members {
		for _, uid := range ids {
			if uid == userID {
				out = append(out, tid)
			}
		}
	}
	return out, nil
}

func (r *visRooms) CountThreadMembers(_ context.Context, roomID int64) (int, error) {
	return len(r.members[roomID]), nil
}

func (r *visRooms) ThreadMessageCounts(_ context.Context, ids []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(ids))
	for _, rid := range ids {
		out[rid] = 7
	}
	return out, nil
}

// kvHook is eventLog with an actual cache behind it: GET, SET and DEL work, so the snapshot
// stays stale until something drops it, the way it does against Redis. A test that announces
// before invalidating therefore fails here where it would pass against a cache of misses.
type kvHook struct {
	*eventLog
	mu sync.Mutex
	kv map[string]string
}

func (h *kvHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	publish := h.eventLog.ProcessHook(next)
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		args := cmd.Args()
		key := ""
		if len(args) > 1 {
			key, _ = args[1].(string)
		}
		switch strings.ToLower(cmd.Name()) {
		case "get":
			v, ok := h.kv[key]
			if !ok {
				cmd.SetErr(redis.Nil)
				return redis.Nil
			}
			cmd.(*redis.StringCmd).SetVal(v)
		case "set", "setnx":
			var v string
			switch raw := args[2].(type) {
			case []byte:
				v = string(raw)
			case string:
				v = raw
			}
			if b, ok := cmd.(*redis.BoolCmd); ok {
				_, taken := h.kv[key]
				if !taken {
					h.kv[key] = v
				}
				b.SetVal(!taken)
				return nil
			}
			h.kv[key] = v
			cmd.(*redis.StatusCmd).SetVal("OK")
		case "del":
			delete(h.kv, key)
			cmd.(*redis.IntCmd).SetVal(1)
		default:
			return publish(ctx, cmd)
		}
		return nil
	}
}

func (h *kvHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return h.eventLog.ProcessPipelineHook(next)
}

// visWorld is the whole fixture.
type visWorld struct {
	repo  *visRepo
	rooms *visRooms
	fed   *visFed
	svc   *Service
	log   *eventLog
	users *profileUsers
}

// visFed is recordingFed that also records room relays, in order.
type visFed struct {
	recordingFed
	roomRelays []string // "upsert:<room>", "delete:<room>", "reorder"
	synced     map[int64]*bool
	greetings  []int64
}

func (f *visFed) AfterSpaceRoomChanged(_ context.Context, _ int64, room *rooms.Room, _ RoomOverrides, deleted bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	what := "upsert:"
	if deleted {
		what = "delete:"
	} else {
		if f.synced == nil {
			f.synced = map[int64]*bool{}
		}
		f.synced[room.ID] = room.PermissionsSynced
	}
	f.roomRelays = append(f.roomRelays, what+id.Format(room.ID))
}

func (f *visFed) AfterSpaceRoomsReordered(context.Context, int64, []*rooms.Room) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roomRelays = append(f.roomRelays, "reorder")
}

func (f *visFed) AfterSpaceRoleChanged(context.Context, int64, *SpaceRole, bool) {}

func (f *visFed) RoomFederations(_ context.Context, ids []int64) map[int64]*rooms.Federation {
	out := make(map[int64]*rooms.Federation, len(ids))
	for _, rid := range ids {
		out[rid] = &rooms.Federation{OriginDomain: "a.example", OriginID: id.Format(rid + 1000)}
	}
	return out
}

func (f *visFed) RemoteBirthdayGreeting(_ context.Context, _ string, _ int64, user *auth.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.greetings = append(f.greetings, user.ID)
	return nil
}

// newVisWorld is the space with the snapshot cache on. A test that wants it to be a mirror
// sets fed.mirrors[rfSpace].
func newVisWorld() *visWorld {
	repo := newVisRepo()
	roomRepo := newVisRooms()
	log := &eventLog{}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rdb.AddHook(&kvHook{eventLog: log, kv: map[string]string{}})
	cfg := &config.Config{}
	cfg.Database.Redis.CacheEnabled = true
	users := fixtureUsers(rfOwnerID, rfAdminID, rfModID, rfMemberID, rfPlainID, rfMod2ID)
	svc := NewService(repo, roomRepo, users, rdb, cfg)
	svc.runLater = func(f func()) { f() } // so a test sees what a change announces
	fed := &visFed{recordingFed: recordingFed{mirrors: map[int64]string{}}}
	svc.SetFederator(fed)
	return &visWorld{repo: repo, rooms: roomRepo, fed: fed, svc: svc, log: log, users: users}
}

// mirror makes the fixture's space a mirror of a.example's.
func (w *visWorld) mirror() *visWorld {
	w.fed.mirrors[rfSpace] = "a.example"
	return w
}

// deny stores an override denying View on a room for a role.
func (w *visWorld) deny(roomID, roleID int64) {
	w.repo.roleOv[roomID] = map[int64]SpaceRoomRoleOverride{roleID: {SpaceID: rfSpace, RoomID: roomID, RoleID: roleID, Deny: permissions.PermViewRoom}}
}

// allow adds to a room's overrides one allowing View for a role.
func (w *visWorld) allow(roomID, roleID int64) {
	if w.repo.roleOv[roomID] == nil {
		w.repo.roleOv[roomID] = map[int64]SpaceRoomRoleOverride{}
	}
	w.repo.roleOv[roomID][roleID] = SpaceRoomRoleOverride{SpaceID: rfSpace, RoomID: roomID, RoleID: roleID, Allow: permissions.PermViewRoom}
}

// settle drops whatever the fixture's setup cached or sent, so a test sees only what it did.
func (w *visWorld) settle(t *testing.T) {
	t.Helper()
	w.svc.invalidateSnapshot(context.Background(), rfSpace)
	if _, err := w.svc.Snapshot(context.Background(), rfSpace); err != nil {
		t.Fatal(err)
	}
	w.log.mu.Lock()
	w.log.events = nil
	w.log.mu.Unlock()
	w.rooms.writes = nil
	w.repo.overrideWrites = 0
	w.fed.roomRelays = nil
}

// sentTo is who an event went to: the ids of users it was addressed to one by one, and whether
// it went to the whole space channel. A filter narrows it to events about one room.
func (w *visWorld) sentTo(eventType string, roomID int64) (users []int64, spaceWide bool) {
	for _, e := range w.log.of(eventType) {
		if raw, _ := e.Data["room_id"].(string); roomID != 0 && raw != id.Format(roomID) {
			if raw2, _ := e.Data["id"].(string); raw2 != id.Format(roomID) {
				continue
			}
		}
		if e.SpaceID != "" {
			spaceWide = true
			continue
		}
		uid, err := id.Parse(e.UserID)
		if err == nil {
			users = append(users, uid)
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i] < users[j] })
	return users, spaceWide
}

func i64s(v ...int64) []int64 { return v }

func boolp(v bool) *bool { return &v }
