package spaces

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

var errListFailed = errors.New("scylla: list failed")

// orderRepo is the slice of Repository the role-order and member-role paths touch, in memory
// and safe for concurrent use (the lock tests write from many goroutines). Any other method
// panics through the nil embedded interface - an unexpected call is a bug.
type orderRepo struct {
	Repository
	mu      sync.Mutex
	spaces  map[int64]*Space
	roles   map[int64]map[int64]SpaceRole    // space -> role id -> role
	members map[int64]map[int64]*SpaceMember // space -> user -> member
	audits  []SpaceAuditEntry
	// What was written, in order: role ids through UpdateSpaceRole / InsertSpaceRole, and member
	// role writes as "set:<user>", "add:<user>:<role>", "remove:<user>:<role>".
	roleUpdates  []int64
	roleInserts  []int64
	memberWrites []string
	claimed      map[string]bool
	// yield makes every read and write give up the processor first, so goroutines racing
	// through a role write interleave between its read and its write.
	yield bool
	// failList makes ListSpaceRoles fail for that space.
	failList int64
	// failUpdateAt makes the failUpdateAt-th UpdateSpaceRole call (counting from 1) fail
	// without writing; updateCalls counts them.
	failUpdateAt, updateCalls int
	// onUpdate, when set, runs before every UpdateSpaceRole writes (outside the lock): a
	// change made by another request while a role write is under way.
	onUpdate func()
}

var errUpdateFailed = errors.New("scylla: update failed")

func (r *orderRepo) pause() {
	if r.yield {
		runtime.Gosched()
	}
}

func (r *orderRepo) GetByID(_ context.Context, spaceID int64) (*Space, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sp := r.spaces[spaceID]
	if sp == nil {
		return nil, nil
	}
	cp := *sp
	return &cp, nil
}

// ListSpaceRoles answers in id order, the table's clustering order.
func (r *orderRepo) ListSpaceRoles(_ context.Context, spaceID int64) ([]SpaceRole, error) {
	r.pause()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failList != 0 && r.failList == spaceID {
		return nil, errListFailed
	}
	out := make([]SpaceRole, 0, len(r.roles[spaceID]))
	for _, role := range r.roles[spaceID] {
		out = append(out, role)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *orderRepo) GetSpaceRole(_ context.Context, spaceID, roleID int64) (*SpaceRole, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	role, ok := r.roles[spaceID][roleID]
	if !ok {
		return nil, nil
	}
	return &role, nil
}

func (r *orderRepo) InsertSpaceRole(_ context.Context, role *SpaceRole) error {
	r.pause()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.roles[role.SpaceID] == nil {
		r.roles[role.SpaceID] = map[int64]SpaceRole{}
	}
	r.roles[role.SpaceID][role.ID] = *role
	r.roleInserts = append(r.roleInserts, role.ID)
	return nil
}

// UpdateSpaceRole writes the columns the real UPDATE does - and, like it, writes a row that is
// not there (without bot_id or created_at), which is what a position written back onto a
// deleted role would do.
func (r *orderRepo) UpdateSpaceRole(_ context.Context, role *SpaceRole) error {
	r.pause()
	if r.onUpdate != nil {
		r.onUpdate()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
	if r.failUpdateAt != 0 && r.updateCalls == r.failUpdateAt {
		return errUpdateFailed
	}
	if r.roles[role.SpaceID] == nil {
		r.roles[role.SpaceID] = map[int64]SpaceRole{}
	}
	cur := r.roles[role.SpaceID][role.ID]
	cur.SpaceID, cur.ID = role.SpaceID, role.ID
	cur.Name, cur.Permissions, cur.Position = role.Name, role.Permissions, role.Position
	cur.Color, cur.Hoist, cur.Mentionable, cur.UpdatedAt = role.Color, role.Hoist, role.Mentionable, role.UpdatedAt
	r.roles[role.SpaceID][role.ID] = cur
	r.roleUpdates = append(r.roleUpdates, role.ID)
	return nil
}

func (r *orderRepo) DeleteSpaceRole(_ context.Context, spaceID, roleID int64) error {
	r.pause()
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.roles[spaceID], roleID)
	return nil
}

func (r *orderRepo) member(spaceID, userID int64) *SpaceMember {
	m := r.members[spaceID][userID]
	if !liveMember(m) {
		return nil
	}
	return m
}

func (r *orderRepo) IsMember(_ context.Context, spaceID, userID int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.member(spaceID, userID) != nil, nil
}

func (r *orderRepo) MemberRoleIDs(_ context.Context, spaceID, userID int64) ([]int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.member(spaceID, userID)
	if m == nil {
		return nil, false, nil
	}
	return append([]int64(nil), m.RoleIDs...), true, nil
}

func (r *orderRepo) GetMember(_ context.Context, spaceID, userID int64) (*SpaceMember, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.member(spaceID, userID)
	if m == nil {
		return nil, nil
	}
	cp := *m
	cp.RoleIDs = append([]int64(nil), m.RoleIDs...)
	return &cp, nil
}

// row is the member row a role write lands on; like the real UPDATE, a write for someone who is
// not a member makes a row without joined_at, which reads treat as absent.
func (r *orderRepo) row(spaceID, userID int64) *SpaceMember {
	if r.members[spaceID] == nil {
		r.members[spaceID] = map[int64]*SpaceMember{}
	}
	m := r.members[spaceID][userID]
	if m == nil {
		m = &SpaceMember{SpaceID: spaceID, UserID: userID}
		r.members[spaceID][userID] = m
	}
	return m
}

// setOf is role_ids as the set<bigint> it is: no duplicates, in value order.
func setOf(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, x := range ids {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (r *orderRepo) SetMemberRoleIDs(_ context.Context, spaceID, userID int64, roleIDs []int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.row(spaceID, userID).RoleIDs = setOf(roleIDs)
	r.memberWrites = append(r.memberWrites, "set:"+id.Format(userID))
	return nil
}

func (r *orderRepo) AddMemberRoleID(_ context.Context, spaceID, userID, roleID int64) error {
	r.pause()
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.row(spaceID, userID)
	m.RoleIDs = setOf(append(m.RoleIDs, roleID))
	r.memberWrites = append(r.memberWrites, "add:"+id.Format(userID)+":"+id.Format(roleID))
	return nil
}

func (r *orderRepo) RemoveMemberRoleID(_ context.Context, spaceID, userID, roleID int64) error {
	r.pause()
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.row(spaceID, userID)
	m.RoleIDs = removeInt64FromSlice(m.RoleIDs, roleID)
	r.memberWrites = append(r.memberWrites, "remove:"+id.Format(userID)+":"+id.Format(roleID))
	return nil
}

// The member add a bot install makes.
func (r *orderRepo) AddMember(_ context.Context, spaceID, userID int64, roleIDs []int64, _ MemberProfile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.members[spaceID] == nil {
		r.members[spaceID] = map[int64]*SpaceMember{}
	}
	r.members[spaceID][userID] = &SpaceMember{SpaceID: spaceID, UserID: userID, RoleIDs: setOf(roleIDs), JoinedAt: time.Now().UTC()}
	return nil
}

func (r *orderRepo) AddSpaceToUserSet(context.Context, int64, int64) error { return nil }

func (r *orderRepo) GetBan(context.Context, int64, int64) (*SpaceBan, error) { return nil, nil }

func (r *orderRepo) InsertAuditEntry(_ context.Context, e *SpaceAuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, *e)
	return nil
}

func (r *orderRepo) ForEachSpaceRole(_ context.Context, fn func(spaceID, roleID int64, name string, permissions, botID int64) error) error {
	r.mu.Lock()
	type row struct {
		space int64
		role  SpaceRole
	}
	var rows []row
	for sid, roles := range r.roles {
		for _, role := range roles {
			rows = append(rows, row{sid, role})
		}
	}
	r.mu.Unlock()
	for _, x := range rows {
		if err := fn(x.space, x.role.ID, x.role.Name, x.role.Permissions, x.role.BotID); err != nil {
			return err
		}
	}
	return nil
}

func (r *orderRepo) ClaimDataMigration(_ context.Context, name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimed[name] {
		return false, nil
	}
	r.claimed[name] = true
	return true, nil
}

func (r *orderRepo) ReleaseDataMigration(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.claimed, name)
	return nil
}

// positions is every role's position in a space, by id.
func (r *orderRepo) positions(spaceID int64) map[int64]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[int64]int{}
	for rid, role := range r.roles[spaceID] {
		out[rid] = role.Position
	}
	return out
}

func (r *orderRepo) roleIDsOf(spaceID, userID int64) []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.members[spaceID][userID]; m != nil {
		return append([]int64(nil), m.RoleIDs...)
	}
	return nil
}

// assertUniquePositions fails unless every custom role of the space sits at its own position,
// all >= 1, with @everyone at 0 (R6).
func assertUniquePositions(t *testing.T, r *orderRepo, spaceID int64) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	everyone := r.spaces[spaceID].EveryoneRoleID
	at := map[int]int64{}
	for rid, role := range r.roles[spaceID] {
		if rid == everyone {
			if role.Position != 0 {
				t.Errorf("@everyone moved to %d", role.Position)
			}
			continue
		}
		if role.Position < 1 {
			t.Errorf("role %d at %d", rid, role.Position)
		}
		if other, dup := at[role.Position]; dup {
			t.Errorf("roles %d and %d share position %d", other, rid, role.Position)
		}
		at[role.Position] = rid
	}
}

// noRooms is a room repository for a space without rooms (a bot install's join bookkeeping
// lists them).
type noRooms struct{ rooms.Repository }

func (noRooms) ListBySpace(context.Context, int64) ([]rooms.RoomBySpaceRow, error) { return nil, nil }
func (noRooms) GetByIDs(context.Context, []int64) ([]*rooms.Room, error)           { return nil, nil }

// recordingFed is a Federator that records what reaches it. mirrors names the spaces it reports
// as hosted on another instance; every method not defined here panics through the nil
// embedded interface.
type recordingFed struct {
	Federator
	mu           sync.Mutex
	mirrors      map[int64]string
	roleRelays   []SpaceRole
	memberRelays []int64
	setRoles     []remoteSetRoles
	reorders     [][]RolePosition
	changes      []remoteChangeRole
	// changeErr is what RemoteChangeMemberRole answers (after recording the call).
	changeErr error
}

type remoteChangeRole struct {
	origin  string
	actor   int64
	target  int64
	role    int64
	add     bool
	spaceID int64
}

type remoteSetRoles struct {
	origin  string
	actor   int64
	target  int64
	roleIDs []int64
	spaceID int64
}

func (f *recordingFed) SpaceFederation(_ context.Context, spaceID int64) *SpaceFederation {
	if d := f.mirrors[spaceID]; d != "" {
		return &SpaceFederation{OriginDomain: d, OriginID: "7"}
	}
	return nil
}

func (f *recordingFed) RoomFederations(context.Context, []int64) map[int64]*rooms.Federation {
	return nil
}

func (f *recordingFed) AfterSpaceRoleChanged(_ context.Context, _ int64, role *SpaceRole, deleted bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !deleted {
		f.roleRelays = append(f.roleRelays, *role)
	}
}

func (f *recordingFed) AfterSpaceMemberUpdated(_ context.Context, _ int64, m *SpaceMember) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memberRelays = append(f.memberRelays, m.UserID)
}

func (f *recordingFed) AfterSpaceMemberAdded(context.Context, int64, *SpaceMember) {}

func (f *recordingFed) RemoteSetMemberRoles(_ context.Context, origin string, spaceID int64, user *auth.User, targetID int64, roleIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, len(roleIDs))
	copy(ids, roleIDs)
	f.setRoles = append(f.setRoles, remoteSetRoles{origin: origin, actor: user.ID, target: targetID, roleIDs: ids, spaceID: spaceID})
	return nil
}

func (f *recordingFed) RemoteChangeMemberRole(_ context.Context, origin string, spaceID int64, user *auth.User, targetID, roleID int64, add bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, remoteChangeRole{origin: origin, actor: user.ID, target: targetID, role: roleID, add: add, spaceID: spaceID})
	return f.changeErr
}

func (f *recordingFed) RemoteReorderRoles(_ context.Context, _ string, _ int64, _ *auth.User, moves []RolePosition) ([]SpaceRole, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reorders = append(f.reorders, append([]RolePosition(nil), moves...))
	return nil, nil
}

// relayed is the ids of the roles relayed, in order.
func (f *recordingFed) relayed() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, 0, len(f.roleRelays))
	for _, r := range f.roleRelays {
		out = append(out, r.ID)
	}
	return out
}

// The role fixture: space 500, owner 1.
//
//	@everyone 50 (0) < member 51 (1) < mod 52 (2: Manage Roles, Kick, Ban, Manage Nicknames)
//	< admin 53 (3: Administrator)
//
// Members: owner 1 (no roles), admin 2 (admin), mod 3 (mod), member 4 (member), plain 5 (none),
// mod2 6 (mod - the same rank as 3).
const (
	rfSpace    = 500
	rfEveryone = 50
	rfMember   = 51
	rfMod      = 52
	rfAdmin    = 53

	rfOwnerID  = 1
	rfAdminID  = 2
	rfModID    = 3
	rfMemberID = 4
	rfPlainID  = 5
	rfMod2ID   = 6
)

const rfModPerms = permissions.PermManageRoles | permissions.PermKickMembers | permissions.PermBanMembers | permissions.PermManageNicknames

func roleFixtureRepo() *orderRepo {
	joined := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	r := &orderRepo{
		spaces: map[int64]*Space{rfSpace: {ID: rfSpace, OwnerID: rfOwnerID, EveryoneRoleID: rfEveryone, Name: "Roles"}},
		roles: map[int64]map[int64]SpaceRole{rfSpace: {
			rfEveryone: {SpaceID: rfSpace, ID: rfEveryone, Name: EveryoneRoleName, Position: 0, Permissions: permissions.DefaultEveryone},
			rfMember:   {SpaceID: rfSpace, ID: rfMember, Name: "member", Position: 1},
			rfMod:      {SpaceID: rfSpace, ID: rfMod, Name: "mod", Position: 2, Permissions: rfModPerms},
			rfAdmin:    {SpaceID: rfSpace, ID: rfAdmin, Name: "admin", Position: 3, Permissions: permissions.PermAdministrator},
		}},
		members: map[int64]map[int64]*SpaceMember{rfSpace: {}},
		claimed: map[string]bool{},
	}
	add := func(uid int64, roles ...int64) {
		r.members[rfSpace][uid] = &SpaceMember{SpaceID: rfSpace, UserID: uid, RoleIDs: setOf(append([]int64{rfEveryone}, roles...)), JoinedAt: joined}
	}
	add(rfOwnerID)
	add(rfAdminID, rfAdmin)
	add(rfModID, rfMod)
	add(rfMemberID, rfMember)
	add(rfPlainID)
	add(rfMod2ID, rfMod)
	return r
}

// addRole puts a custom role into a space of the fixture.
func (r *orderRepo) addRole(spaceID int64, role SpaceRole) {
	r.mu.Lock()
	defer r.mu.Unlock()
	role.SpaceID = spaceID
	r.roles[spaceID][role.ID] = role
}

func fixtureUsers(ids ...int64) *profileUsers {
	u := &profileUsers{users: map[int64]*auth.User{}}
	for _, uid := range ids {
		u.users[uid] = &auth.User{ID: uid, Username: "user" + id.Format(uid)}
	}
	return u
}

// roleService is a Service over r with a recording federator (every space local unless named in
// fed.mirrors) and users 1-6.
func roleService(r *orderRepo) (*Service, *recordingFed) {
	return roleServiceWith(r, nil)
}

func roleServiceWith(r *orderRepo, rdb *redis.Client) (*Service, *recordingFed) {
	s := NewService(r, noRooms{}, fixtureUsers(rfOwnerID, rfAdminID, rfModID, rfMemberID, rfPlainID, rfMod2ID), rdb, nil)
	fed := &recordingFed{mirrors: map[int64]string{}}
	s.SetFederator(fed)
	return s, fed
}

// roleServiceWithEvents is roleService that also records the gateway events it publishes.
func roleServiceWithEvents(r *orderRepo) (*Service, *recordingFed, *eventLog) {
	log := &eventLog{}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rdb.AddHook(log)
	s, fed := roleServiceWith(r, rdb)
	return s, fed, log
}

// eventLog is a go-redis hook that answers every command itself, so the client never dials:
// a PUBLISH (a gateway event) is decoded and recorded, anything else is a cache miss.
type eventLog struct {
	mu     sync.Mutex
	events []sentEvent
}

type sentEvent struct {
	Type string
	Data map[string]any
}

func (l *eventLog) DialHook(next redis.DialHook) redis.DialHook { return next }

func (l *eventLog) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if !strings.EqualFold(cmd.Name(), "publish") || len(args) != 3 {
			cmd.SetErr(redis.Nil)
			return redis.Nil
		}
		var raw []byte
		switch v := args[2].(type) {
		case []byte:
			raw = v
		case string:
			raw = []byte(v)
		}
		var env struct {
			Type string         `json:"t"`
			Data map[string]any `json:"d"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			cmd.SetErr(err)
			return err
		}
		l.mu.Lock()
		l.events = append(l.events, sentEvent{Type: env.Type, Data: env.Data})
		l.mu.Unlock()
		return nil
	}
}

func (l *eventLog) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return redis.Nil }
}

// of is the events of one type, in the order they were published.
func (l *eventLog) of(eventType string) []sentEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []sentEvent
	for _, e := range l.events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// rolePositions reads an event's "roles" list back as role id -> position.
func rolePositions(t *testing.T, e sentEvent) map[int64]int {
	t.Helper()
	list, ok := e.Data["roles"].([]any)
	if !ok {
		t.Fatalf("%s carries no role list: %v", e.Type, e.Data)
	}
	out := make(map[int64]int, len(list))
	for _, x := range list {
		m, _ := x.(map[string]any)
		raw, _ := m["id"].(string)
		rid, err := id.Parse(raw)
		if err != nil {
			t.Fatalf("role id %v: %v", m["id"], err)
		}
		pos, _ := m["position"].(float64)
		out[rid] = int(pos)
	}
	return out
}
