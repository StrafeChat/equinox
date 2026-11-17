package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// ---- call log ---------------------------------------------------------------------------

// callLog records what the pipeline did, in order, as "name:id" entries.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(name string, id int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, fmt.Sprintf("%s:%d", name, id))
}

func (l *callLog) index(entry string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, c := range l.calls {
		if c == entry {
			return i
		}
	}
	return -1
}

func (l *callLog) has(entry string) bool { return l.index(entry) >= 0 }

func (l *callLog) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// ---- the fakeUsers / fakeSessions additions (moderation_test.go holds the rest) ---------

func (f *fakeUsers) DeleteAccount(_ context.Context, u *auth.User) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.log.add("identity", u.ID)
	delete(f.users, u.ID)
	return nil
}
func (f *fakeUsers) TombstoneShadow(context.Context, int64) error { return nil }

func (f *fakeSessions) DeleteAllForUser(_ context.Context, userID int64) error {
	f.log.add("sessions_delete", userID)
	return nil
}

// ---- fakes for AccountDeletionDeps ------------------------------------------------------

type fakeDelSpaces struct {
	log      *callLog
	memberOf map[int64][]int64
	owned    map[int64][]spaces.OwnedSpace
	warning  string
}

func (f *fakeDelSpaces) AccountSpaceIDs(_ context.Context, userID int64, known []int64) ([]int64, error) {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range append(append([]int64{}, f.memberOf[userID]...), known...) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}
func (f *fakeDelSpaces) OwnedSpaces(_ context.Context, userID int64, _ []int64) ([]spaces.OwnedSpace, error) {
	return f.owned[userID], nil
}
func (f *fakeDelSpaces) RemoveAccount(_ context.Context, userID, spaceID int64) (string, error) {
	f.log.add(fmt.Sprintf("space_leave:%d", spaceID), userID)
	return f.warning, nil
}

type fakeDelRooms struct {
	log     *callLog
	rows    map[int64][]rooms.RoomRow
	rooms   map[int64]*rooms.Room
	threads map[int64][]int64
}

func (f *fakeDelRooms) ListByUser(_ context.Context, userID int64) ([]rooms.RoomRow, error) {
	return f.rows[userID], nil
}
func (f *fakeDelRooms) GetByIDs(_ context.Context, ids []int64) ([]*rooms.Room, error) {
	out := make([]*rooms.Room, len(ids))
	for i, id := range ids {
		out[i] = f.rooms[id]
	}
	return out, nil
}
func (f *fakeDelRooms) ListUserThreadIDs(_ context.Context, userID int64) ([]int64, error) {
	return f.threads[userID], nil
}
func (f *fakeDelRooms) RemoveThreadMember(_ context.Context, roomID, userID int64) error {
	f.log.add(fmt.Sprintf("thread_leave:%d", roomID), userID)
	return nil
}

type fakeGroups struct{ log *callLog }

func (f *fakeGroups) RemoveParticipant(_ context.Context, actorID, roomID, targetID int64) error {
	if actorID != targetID {
		return errors.New("not a self-leave")
	}
	f.log.add(fmt.Sprintf("group_leave:%d", roomID), targetID)
	return nil
}

type fakeRels struct {
	log      *callLog
	incoming map[int64][]int64
	outgoing map[int64][]int64
}

func (f *fakeRels) RequestCounterparts(_ context.Context, userID int64) ([]int64, []int64, error) {
	return f.incoming[userID], f.outgoing[userID], nil
}
func (f *fakeRels) PurgeUser(_ context.Context, u *auth.User) (relationships.PurgeCounts, error) {
	f.log.add("relationships", u.ID)
	return relationships.PurgeCounts{Friends: len(u.Relationships)}, nil
}

// fakeCounter stands in for devices, OAuth grants and push subscriptions alike.
type fakeCounter struct {
	log  *callLog
	name string
	n    map[int64]int
}

func (f *fakeCounter) count(userID int64) (int, error) { return f.n[userID], nil }
func (f *fakeCounter) PurgeUser(_ context.Context, userID int64) (int, error) {
	f.log.add(f.name, userID)
	n := f.n[userID]
	delete(f.n, userID)
	return n, nil
}
func (f *fakeCounter) CountDevices(_ context.Context, id int64) (int, error) { return f.count(id) }
func (f *fakeCounter) CountGrants(_ context.Context, id int64) (int, error)  { return f.count(id) }
func (f *fakeCounter) CountSubscriptions(_ context.Context, id int64) (int, error) {
	return f.count(id)
}

type fakeDelApps struct {
	log  *callLog
	apps map[int64]*applications.Application
}

func (f *fakeDelApps) ListByOwner(_ context.Context, ownerID int64) ([]applications.Application, error) {
	var out []applications.Application
	for _, a := range f.apps {
		if a.OwnerID == ownerID {
			out = append(out, *a)
		}
	}
	return out, nil
}
func (f *fakeDelApps) AppForBot(_ context.Context, botUserID int64) (*applications.Application, error) {
	for _, a := range f.apps {
		if a.BotUserID == botUserID {
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeDelApps) RevokeBotToken(_ context.Context, appID int64) error {
	f.log.add("bot_token", appID)
	return nil
}
func (f *fakeDelApps) AdminDelete(_ context.Context, appID int64) error {
	f.log.add("app_delete", appID)
	delete(f.apps, appID)
	return nil
}

type fakeDelVoice struct{ log *callLog }

func (f *fakeDelVoice) Leave(_ context.Context, userID int64) error {
	f.log.add("voice", userID)
	return nil
}

type fakeDelFed struct {
	log      *callLog
	audience map[string]struct{}
	told     map[string]struct{}
}

func (f *fakeDelFed) DeletionAudience(_ context.Context, u *auth.User, _ []int64) map[string]struct{} {
	f.log.add("audience", u.ID)
	return f.audience
}
func (f *fakeDelFed) AfterAccountDeleted(_ context.Context, u *auth.User, domains map[string]struct{}) {
	f.log.add("notice", u.ID)
	f.told = domains
}

type fakePartitions struct{ log *callLog }

func (f *fakePartitions) DeleteUserPartitions(_ context.Context, userID int64) error {
	f.log.add("partitions", userID)
	return nil
}

// fakeSpaceRepo is the slice of spaces.Repository a takedown reads (unimplemented methods
// panic, which is the right test outcome).
type fakeSpaceRepo struct {
	spaces.Repository
	byID map[int64]*spaces.Space
}

func (f *fakeSpaceRepo) GetByID(_ context.Context, id int64) (*spaces.Space, error) {
	return f.byID[id], nil
}

type fakeRemover struct{ log *callLog }

func (f *fakeRemover) TakeDownSpace(_ context.Context, _, spaceID int64) error {
	f.log.add("takedown", spaceID)
	return nil
}
func (f *fakeRemover) SetOfficial(context.Context, int64, bool) (*spaces.Space, error) {
	return nil, nil
}

// ---- environment ------------------------------------------------------------------------

const (
	bobID       = int64(30)
	sysID       = int64(31)
	admin2ID    = int64(32)
	ownerID     = int64(33)
	botID       = int64(40) // the bot of application helperApp, owned by ownerID
	helperApp   = int64(40)
	orphanBotID = int64(41)
	pmRoom      = int64(100)
	groupRoom   = int64(101)
	spaceA      = int64(200)
	spaceB      = int64(201)
	ownedLocal  = int64(202)
	ownedRemote = int64(203)
)

type delEnv struct {
	svc      *Service
	mod      *fakeModRepo
	repo     *fakeRepo
	users    *fakeUsers
	sessions *fakeSessions
	log      *callLog
	spaces   *fakeDelSpaces
	apps     *fakeDelApps
	fed      *fakeDelFed
	reported []int64 // ids that received an official notice (none: notices are off)
}

func newDelEnv(t *testing.T) *delEnv {
	t.Helper()
	log := &callLog{}
	dob := time.Date(1990, 5, 17, 0, 0, 0, 0, time.UTC)
	users := &fakeUsers{log: log, users: map[int64]*auth.User{
		adminID:     {ID: adminID, Username: "admin", Email: "admin@x"},
		aliceID:     {ID: aliceID, Username: "alice", Email: "alice@x", Relationships: []int64{bobID}, Spaces: []int64{spaceA}, DateOfBirth: dob},
		bobID:       {ID: bobID, Username: "bob", Email: "bob@x", Relationships: []int64{aliceID}},
		sysID:       {ID: sysID, Username: "system", Email: "system@x"},
		admin2ID:    {ID: admin2ID, Username: "admin2", Email: "admin2@x"},
		ownerID:     {ID: ownerID, Username: "owner", Email: "owner@x"},
		botID:       {ID: botID, Username: "helper", Email: "bot+40@bots.invalid", Bot: true, Spaces: []int64{spaceB}},
		orphanBotID: {ID: orphanBotID, Username: "lost", Email: "bot+41@bots.invalid", Bot: true},
		remoteID:    {ID: remoteID, Username: "far", HomeDomain: "other.example", RemoteID: &remoteOrigin},
	}}
	sessions := &fakeSessions{log: log}
	mod := newFakeModRepo()
	repo := newFakeRepo()
	_ = repo.SetInstanceAdmin(context.Background(), admin2ID, true)
	svc := newTestService(repo, adminID)
	svc.systemUserID = sysID
	svc.SetModeration(ModerationDeps{
		Repo:     mod,
		Users:    users,
		Sessions: sessions,
		Spaces: &fakeSpaceRepo{byID: map[int64]*spaces.Space{
			ownedLocal: {ID: ownedLocal, Name: "Owned", OwnerID: ownerID},
		}},
		Remover: &fakeRemover{log: log},
	})
	sp := &fakeDelSpaces{log: log,
		memberOf: map[int64][]int64{aliceID: {spaceA, spaceB}, ownerID: {ownedLocal}},
		owned:    map[int64][]spaces.OwnedSpace{},
	}
	apps := &fakeDelApps{log: log, apps: map[int64]*applications.Application{
		helperApp: {ID: helperApp, OwnerID: ownerID, Name: "Helper", BotUserID: botID},
	}}
	fed := &fakeDelFed{log: log, audience: map[string]struct{}{"b.example": {}}}
	svc.SetAccountDeletion(AccountDeletionDeps{
		Spaces: sp,
		Rooms: &fakeDelRooms{log: log,
			rows: map[int64][]rooms.RoomRow{aliceID: {{RoomID: pmRoom}, {RoomID: groupRoom}}},
			rooms: map[int64]*rooms.Room{
				pmRoom:    {ID: pmRoom, Type: rooms.TypePM},
				groupRoom: {ID: groupRoom, Type: rooms.TypeGroupPM},
			},
			threads: map[int64][]int64{aliceID: {300}},
		},
		Groups:        &fakeGroups{log: log},
		Relationships: &fakeRels{log: log, incoming: map[int64][]int64{aliceID: {50}}, outgoing: map[int64][]int64{}},
		Devices:       &fakeCounter{log: log, name: "devices", n: map[int64]int{aliceID: 2}},
		OAuth:         &fakeCounter{log: log, name: "oauth", n: map[int64]int{aliceID: 1}},
		Push:          &fakeCounter{log: log, name: "push", n: map[int64]int{aliceID: 3}},
		Apps:          apps,
		Voice:         &fakeDelVoice{log: log},
		Federation:    fed,
		Partitions:    &fakePartitions{log: log},
		Broadcast:     func(_ context.Context, u *auth.User) { log.add("broadcast", u.ID) },
		Publish: func(_ context.Context, userID int64, event string, data map[string]interface{}) {
			log.add(event+"/"+fmt.Sprint(data["reason"]), userID)
		},
	})
	return &delEnv{svc: svc, mod: mod, repo: repo, users: users, sessions: sessions, log: log, spaces: sp, apps: apps, fed: fed}
}

func (e *delEnv) audit() []AuditEntry {
	out, _ := e.mod.ListAudit(context.Background(), 100)
	return out
}

// ---- refusals ---------------------------------------------------------------------------

func TestDeleteAccountRefusals(t *testing.T) {
	e := newDelEnv(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		actor  int64
		target int64
		in     DeleteAccountInput
		want   error
	}{
		{"self", adminID, adminID, DeleteAccountInput{ConfirmUsername: "admin"}, ErrDeleteSelf},
		{"system account by id", adminID, sysID, DeleteAccountInput{ConfirmUsername: "system"}, ErrDeleteSystem},
		{"admin by column", adminID, admin2ID, DeleteAccountInput{ConfirmUsername: "admin2"}, ErrDeleteAdmin},
		{"remote shadow", adminID, remoteID, DeleteAccountInput{ConfirmUsername: "far"}, ErrCannotBanRemote},
		{"not an admin", aliceID, bobID, DeleteAccountInput{ConfirmUsername: "bob"}, ErrNotAdmin},
		{"missing", adminID, 999, DeleteAccountInput{ConfirmUsername: "x"}, ErrUserNotFound},
		{"confirm case differs", adminID, bobID, DeleteAccountInput{ConfirmUsername: "Bob"}, ErrConfirmMismatch},
		{"confirm empty", adminID, bobID, DeleteAccountInput{}, ErrConfirmMismatch},
	}
	for _, c := range cases {
		if _, err := e.svc.DeleteAccount(ctx, c.actor, c.target, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// The system flag refuses on its own, whatever the id; so does an env administrator.
	e.users.users[77] = &auth.User{ID: 77, Username: "sys2", System: true}
	if _, err := e.svc.DeleteAccount(ctx, adminID, 77, DeleteAccountInput{ConfirmUsername: "sys2"}); !errors.Is(err, ErrDeleteSystem) {
		t.Errorf("system flag: %v", err)
	}
	e.svc.cfg.Flags.InstanceAdmins = append(e.svc.cfg.Flags.InstanceAdmins, bobID)
	if _, err := e.svc.DeleteAccount(ctx, adminID, bobID, DeleteAccountInput{ConfirmUsername: "bob"}); !errors.Is(err, ErrDeleteAdmin) {
		t.Errorf("env admin: %v", err)
	}
	if len(e.sessions.revoked) != 0 || e.log.count("identity") != 0 {
		t.Fatalf("a refused deletion touched something: revoked %v, log %v", e.sessions.revoked, e.log.calls)
	}
	// The preview says why instead of failing.
	p, err := e.svc.PreviewAccountDeletion(ctx, adminID, admin2ID)
	if err != nil || p.Refusal != RefusalAdmin {
		t.Fatalf("preview of an admin: %+v %v", p, err)
	}
}

// ---- the pipeline -----------------------------------------------------------------------

func TestDeleteAccountPipelineOrder(t *testing.T) {
	e := newDelEnv(t)
	ctx := context.Background()
	res, err := e.svc.DeleteAccount(ctx, adminID, aliceID, DeleteAccountInput{ConfirmUsername: " alice "})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if res.Kind != DeletionKindUser || res.Username != "alice" || res.UserID != aliceID {
		t.Fatalf("result %+v", res)
	}
	at := func(entry string) int {
		i := e.log.index(entry)
		if i < 0 {
			t.Fatalf("%s never happened; log %v", entry, e.log.calls)
		}
		return i
	}
	revoke := at("revoke:2")
	revoked := at("SESSION_REVOKED/deleted:2")
	audience := at("audience:2")
	firstTeardown := at("broadcast:2")
	notice := at("notice:2")
	identity := at("identity:2")
	if !(revoke < revoked && revoked < audience && audience < firstTeardown) {
		t.Errorf("access must be cut, then the audience read, before any teardown: %v", e.log.calls)
	}
	for _, step := range []string{"voice:2", "space_leave:200:2", "space_leave:201:2", "thread_leave:300:2",
		"group_leave:101:2", "relationships:2", "devices:2", "oauth:2", "push:2"} {
		if i := at(step); i < firstTeardown || i > notice {
			t.Errorf("%s at %d, want between broadcast %d and the notice %d", step, i, firstTeardown, notice)
		}
	}
	if notice > identity {
		t.Errorf("the peer notice must be queued before the identity batch: %v", e.log.calls)
	}
	if i := at("partitions:2"); i < identity {
		t.Errorf("partitions dropped before the identity: %v", e.log.calls)
	}
	// A 1:1 PM keeps its participant; the group is left.
	if e.log.has("group_leave:100:2") {
		t.Error("a 1:1 PM was left")
	}
	if e.users.users[aliceID] != nil {
		t.Error("users row still there")
	}
	if _, ok := e.fed.told["b.example"]; !ok {
		t.Errorf("peers told %v", e.fed.told)
	}
	want := DeletionCounts{Devices: 2, Spaces: 2, Friends: 1, PendingRequests: 1, GroupPMs: 1, PMs: 1, AuthorizedApps: 1, PushSubscriptions: 3, RemoteInstances: 1}
	if res.Counts != want {
		t.Errorf("counts %+v, want %+v", res.Counts, want)
	}
	audit := e.audit()
	if len(audit) != 1 || audit[0].Action != AuditUserDelete || audit[0].TargetID != aliceID || audit[0].ActorID != adminID {
		t.Fatalf("audit %+v", audit)
	}
	r := audit[0].Reason
	if !strings.HasPrefix(r, "alice (user) - ") || !strings.Contains(r, "spaces 2") || !strings.Contains(r, "devices 2") ||
		!strings.Contains(r, "friends 1") || strings.Contains(r, "alice@x") {
		t.Errorf("audit reason %q", r)
	}
	// Deleting again: the account is gone.
	if _, err := e.svc.DeleteAccount(ctx, adminID, aliceID, DeleteAccountInput{ConfirmUsername: "alice"}); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("second delete = %v", err)
	}
}

func TestDeleteAccountIdentityFailureIsRetryable(t *testing.T) {
	e := newDelEnv(t)
	ctx := context.Background()
	e.users.deleteErr = errors.New("scylla timeout")
	_, err := e.svc.DeleteAccount(ctx, adminID, aliceID, DeleteAccountInput{ConfirmUsername: "alice"})
	var stage *DeletionStageError
	if !errors.As(err, &stage) || stage.Stage != "identity" {
		t.Fatalf("err = %v, want the identity stage", err)
	}
	if e.users.users[aliceID] == nil {
		t.Fatal("the row went although the batch failed")
	}
	if len(e.audit()) != 0 {
		t.Error("a failed deletion was audited as done")
	}
	e.users.deleteErr = nil
	if _, err := e.svc.DeleteAccount(ctx, adminID, aliceID, DeleteAccountInput{ConfirmUsername: "alice"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if e.users.users[aliceID] != nil {
		t.Fatal("retry did not finish the job")
	}
	// The lock was released both times.
	if _, held := e.svc.deleting.Load(aliceID); held {
		t.Error("lock still held")
	}
}

func TestDeleteAccountLock(t *testing.T) {
	e := newDelEnv(t)
	e.svc.deleting.Store(aliceID, struct{}{})
	if _, err := e.svc.DeleteAccount(context.Background(), adminID, aliceID, DeleteAccountInput{ConfirmUsername: "alice"}); !errors.Is(err, ErrDeletionInProgress) {
		t.Fatalf("err = %v, want in progress", err)
	}
	if len(e.sessions.revoked) != 0 {
		t.Fatal("a locked-out run revoked sessions")
	}
}

func TestDeleteAccountBlockers(t *testing.T) {
	ctx := context.Background()

	t.Run("space hosted here", func(t *testing.T) {
		e := newDelEnv(t)
		e.apps.apps = map[int64]*applications.Application{}
		e.spaces.owned[ownerID] = []spaces.OwnedSpace{{ID: ownedLocal, Name: "Owned", MemberCount: 4}}
		_, err := e.svc.DeleteAccount(ctx, adminID, ownerID, DeleteAccountInput{ConfirmUsername: "owner"})
		var blocked *DeletionBlockedError
		if !errors.As(err, &blocked) || len(blocked.Blockers.OwnedSpaces) != 1 {
			t.Fatalf("err = %v, want blocked by the space", err)
		}
		if len(e.sessions.revoked) != 0 {
			t.Fatal("a blocked deletion revoked sessions")
		}
		res, err := e.svc.DeleteAccount(ctx, adminID, ownerID, DeleteAccountInput{ConfirmUsername: "owner", TakeDownSpaces: true})
		if err != nil {
			t.Fatalf("with take_down_spaces: %v", err)
		}
		if !e.log.has("takedown:202") || len(res.SpacesTakenDown) != 1 {
			t.Fatalf("takedown not run: %v / %+v", e.log.calls, res)
		}
		if e.log.has("space_leave:202:33") {
			t.Error("left a space that was taken down")
		}
		actions := []string{}
		for _, a := range e.audit() {
			actions = append(actions, a.Action)
		}
		if strings.Join(actions, ",") != AuditSpaceTakedown+","+AuditUserDelete {
			t.Errorf("audit %v", actions)
		}
	})

	t.Run("space hosted elsewhere", func(t *testing.T) {
		e := newDelEnv(t)
		e.apps.apps = map[int64]*applications.Application{}
		e.spaces.owned[ownerID] = []spaces.OwnedSpace{{ID: ownedRemote, Name: "Far", HostedOn: "b.example"}}
		_, err := e.svc.DeleteAccount(ctx, adminID, ownerID, DeleteAccountInput{ConfirmUsername: "owner", TakeDownSpaces: true, DeleteApplications: true})
		var blocked *DeletionBlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("err = %v, want blocked even with both flags", err)
		}
	})

	t.Run("owned application", func(t *testing.T) {
		e := newDelEnv(t)
		_, err := e.svc.DeleteAccount(ctx, adminID, ownerID, DeleteAccountInput{ConfirmUsername: "owner"})
		var blocked *DeletionBlockedError
		if !errors.As(err, &blocked) || len(blocked.Blockers.OwnedApplications) != 1 || blocked.Blockers.OwnedApplications[0].BotUsername != "helper" {
			t.Fatalf("err = %v, want blocked by the application", err)
		}
		res, err := e.svc.DeleteAccount(ctx, adminID, ownerID, DeleteAccountInput{ConfirmUsername: "owner", DeleteApplications: true})
		if err != nil {
			t.Fatalf("with delete_applications: %v", err)
		}
		botGone, appGone := e.log.index("identity:40"), e.log.index("app_delete:40")
		if botGone < 0 || appGone < 0 || botGone > appGone {
			t.Fatalf("the bot must be deleted, then its application: %v", e.log.calls)
		}
		if !e.log.has("bot_token:40") || !e.log.has("revoke:40") {
			t.Errorf("the bot's access was not cut: %v", e.log.calls)
		}
		if e.log.count("app_delete:") != 1 {
			t.Errorf("application deleted %d times", e.log.count("app_delete:"))
		}
		if len(res.ApplicationsDeleted) != 1 || res.ApplicationsDeleted[0].BotUserID != botID {
			t.Errorf("result %+v", res.ApplicationsDeleted)
		}
		actions := []string{}
		for _, a := range e.audit() {
			actions = append(actions, a.Action)
		}
		if strings.Join(actions, ",") != AuditBotDelete+","+AuditApplicationDelete+","+AuditUserDelete {
			t.Errorf("audit %v", actions)
		}
	})
}

func TestDeleteBotDirectlyTakesItsApplication(t *testing.T) {
	e := newDelEnv(t)
	res, err := e.svc.DeleteAccount(context.Background(), adminID, botID, DeleteAccountInput{ConfirmUsername: "helper"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != DeletionKindBot || !e.log.has("bot_token:40") || !e.log.has("app_delete:40") || !e.log.has("space_leave:201:40") {
		t.Fatalf("result %+v, log %v", res, e.log.calls)
	}
	if e.log.index("bot_token:40") > e.log.index("SESSION_REVOKED/deleted:40") {
		t.Error("token revoked after the sockets were told")
	}
	audit := e.audit()
	if len(audit) != 2 || audit[0].Action != AuditBotDelete || audit[1].Action != AuditApplicationDelete || audit[1].TargetType != TargetApplication {
		t.Fatalf("audit %+v", audit)
	}
}

func TestDeleteOrphanBot(t *testing.T) {
	e := newDelEnv(t)
	ctx := context.Background()
	p, err := e.svc.PreviewAccountDeletion(ctx, adminID, orphanBotID)
	if err != nil || p.Kind != DeletionKindBot || p.Application == nil || !p.Application.Orphan {
		t.Fatalf("preview %+v %v", p, err)
	}
	if _, err := e.svc.DeleteAccount(ctx, adminID, orphanBotID, DeleteAccountInput{ConfirmUsername: "lost"}); err != nil {
		t.Fatal(err)
	}
	if e.users.users[orphanBotID] != nil || e.log.count("app_delete:") != 0 {
		t.Fatalf("orphan: %v", e.log.calls)
	}
}

// The developer deleting their own application deletes its bot account through the same
// pipeline (D2) - audited with the developer as the actor - and leaves the application row
// to the applications module.
func TestDeleteApplicationBotByDeveloper(t *testing.T) {
	e := newDelEnv(t)
	if err := e.svc.DeleteApplicationBot(context.Background(), ownerID, botID); err != nil {
		t.Fatal(err)
	}
	if e.users.users[botID] != nil || e.log.count("app_delete:") != 0 {
		t.Fatalf("log %v", e.log.calls)
	}
	audit := e.audit()
	if len(audit) != 1 || audit[0].Action != AuditBotDelete || audit[0].ActorID != ownerID || !strings.Contains(audit[0].Reason, "application deleted by its owner") {
		t.Fatalf("audit %+v", audit)
	}
	// Not a bot: nothing happens.
	if err := e.svc.DeleteApplicationBot(context.Background(), ownerID, bobID); err != nil || e.users.users[bobID] == nil {
		t.Fatalf("a person was deleted as a bot: %v", err)
	}
}

func TestDeleteAccountClosesReportsAbout(t *testing.T) {
	e := newDelEnv(t)
	ctx := context.Background()
	about, _ := e.svc.CreateReport(ctx, bobID, CreateReportInput{TargetType: TargetUser, TargetID: "2", Reason: "spam"})
	by, _ := e.svc.CreateReport(ctx, aliceID, CreateReportInput{TargetType: TargetUser, TargetID: "30", Reason: "spam"})
	_ = e.mod.CreateBan(ctx, &Ban{UserID: aliceID, Reason: "spam"})
	res, err := e.svc.DeleteAccount(ctx, adminID, aliceID, DeleteAccountInput{ConfirmUsername: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.mod.GetReport(ctx, about.ID)
	if got.Status != ReportResolved || got.Resolution != ResolutionAccountDeleted || got.ResolvedBy == nil || *got.ResolvedBy != adminID {
		t.Errorf("report about = %+v", got)
	}
	if got, _ := e.mod.GetReport(ctx, by.ID); got.Status != ReportOpen {
		t.Errorf("report by the account = %+v, want left open", got)
	}
	if res.Counts.OpenReportsAbout != 1 {
		t.Errorf("open reports counted %d", res.Counts.OpenReportsAbout)
	}
	if b, _ := e.mod.GetBan(ctx, aliceID); b != nil || !res.BanRemoved {
		t.Errorf("ban row %+v, BanRemoved %v", b, res.BanRemoved)
	}
}

// notify must not create an official PM for an account that no longer exists.
func TestNotifySkipsMissingAccount(t *testing.T) {
	e := newDelEnv(t)
	e.svc.mod.RoomsSvc = rooms.NewService(nil, e.users, nil, nil, nil, nil)
	e.svc.mod.MsgsSvc = messages.NewService(nil, nil, e.users, nil, nil, nil)
	// A nil rooms repository would panic if EnsureSystemPM ran.
	if err := e.svc.notify(context.Background(), 999, "hello"); err != nil {
		t.Fatalf("notify to a missing account: %v", err)
	}
}

func TestPreviewCountsAndBlockers(t *testing.T) {
	e := newDelEnv(t)
	p, err := e.svc.PreviewAccountDeletion(context.Background(), adminID, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Refusal != "" || p.Kind != DeletionKindUser || len(p.Blockers.OwnedApplications) != 1 || p.Blockers.OwnedApplications[0].BotSpaceCount != 1 {
		t.Fatalf("preview %+v", p)
	}
	if len(e.sessions.revoked) != 0 || e.log.count("identity") != 0 {
		t.Fatal("a preview changed something")
	}
	if _, err := e.svc.PreviewAccountDeletion(context.Background(), aliceID, ownerID); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("non-admin preview: %v", err)
	}
	bp, err := e.svc.PreviewAccountDeletion(context.Background(), adminID, botID)
	if err != nil || bp.Kind != DeletionKindBot || bp.Application == nil || bp.Application.ID != helperApp || bp.Application.Owner == nil {
		t.Fatalf("bot preview %+v %v", bp, err)
	}
}

// ---- HTTP mapping -----------------------------------------------------------------------

func TestDeletionErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{&DeletionBlockedError{Blockers: DeletionBlockers{OwnedSpaces: []OwnedSpaceBlocker{{ID: 5, Name: "S", HostedOn: "b.example"}}}}, 409, "blocked"},
		{&DeletionStageError{Stage: "identity", Err: errors.New("x")}, 500, ""},
		{ErrConfirmMismatch, 400, "confirm_mismatch"},
		{ErrCannotBanRemote, 400, "remote"},
		{ErrDeleteSelf, 403, "self"},
		{ErrDeleteSystem, 403, "system"},
		{ErrDeleteAdmin, 403, "admin"},
		{ErrDeletionInProgress, 409, "in_progress"},
		{ErrUserNotFound, 404, ""},
		{ErrNotAdmin, 403, ""},
	}
	for _, c := range cases {
		app := fiber.New()
		err := c.err
		app.Get("/", func(ctx fiber.Ctx) error { return deletionError(ctx, err) })
		resp, rerr := app.Test(httptest.NewRequest("GET", "/", nil))
		if rerr != nil {
			t.Fatal(rerr)
		}
		raw, _ := io.ReadAll(resp.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		if resp.StatusCode != c.status {
			t.Errorf("%v: status %d, want %d (%s)", c.err, resp.StatusCode, c.status, raw)
		}
		if c.code != "" && body["code"] != c.code {
			t.Errorf("%v: code %v, want %s", c.err, body["code"], c.code)
		}
		if _, ok := c.err.(*DeletionStageError); ok && body["stage"] != "identity" {
			t.Errorf("stage error body %s", raw)
		}
		if _, ok := c.err.(*DeletionBlockedError); ok && !strings.Contains(string(raw), `"hosted_on":"b.example"`) {
			t.Errorf("blocked body %s", raw)
		}
	}
	// The blocked error also maps through the moderation switch.
	app := fiber.New()
	app.Get("/", func(ctx fiber.Ctx) error { return moderationError(ctx, &DeletionBlockedError{}) })
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	if resp.StatusCode != 409 {
		t.Errorf("moderationError(blocked) = %d", resp.StatusCode)
	}
}

// A developer deleting their application runs the bot's deletion; when the bot owns a space
// (ownership can be transferred to a bot member) or is already being deleted, the developer
// got a bare 500 "internal error". They are the applications module's conflicts now.
func TestDeleteApplicationBotConflicts(t *testing.T) {
	ctx := context.Background()
	e := newDelEnv(t)
	e.spaces.owned[botID] = []spaces.OwnedSpace{{ID: ownedLocal, Name: "Owned"}}
	if err := e.svc.DeleteApplicationBot(ctx, ownerID, botID); !errors.Is(err, applications.ErrBotOwnsSpace) {
		t.Fatalf("bot owning a space: %v", err)
	}
	if e.users.users[botID] == nil || len(e.sessions.revoked) != 0 {
		t.Fatal("a blocked bot deletion went ahead")
	}
	e.spaces.owned[botID] = nil
	unlock, err := e.svc.lockDeletion(ctx, adminID, botID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := e.svc.DeleteApplicationBot(ctx, ownerID, botID); !errors.Is(err, applications.ErrBotBeingDeleted) {
		t.Fatalf("bot already being deleted: %v", err)
	}
}

// The admin lists hydrate their rows with Users.GetByIDs, which answers nil for an id with no
// row. Before account deletion no users row ever went away; now a report filed by or about a
// deleted account, an audit entry whose actor was deleted (a developer who deleted their own
// application, then was deleted) or a ban/IP ban naming one made the whole page panic (500).
// Each list must keep its rows and render the missing account as null.
func TestAdminListsSurviveDeletedAccounts(t *testing.T) {
	ctx := context.Background()
	e := newDelEnv(t)
	gone := int64(990)
	now := time.Now().UTC()
	_ = e.mod.CreateReport(ctx, &Report{ID: 1, ReporterID: gone, TargetType: TargetUser, TargetID: bobID, Reason: "spam", Status: ReportOpen, CreatedAt: now})
	_ = e.mod.CreateReport(ctx, &Report{ID: 2, ReporterID: bobID, TargetType: TargetUser, TargetID: gone, Reason: "spam", Status: ReportOpen, CreatedAt: now})
	_ = e.mod.AppendAudit(ctx, &AuditEntry{ActorID: gone, Action: AuditBotDelete, TargetType: TargetUser, TargetID: botID, CreatedAt: now})
	_ = e.mod.CreateBan(ctx, &Ban{UserID: gone, BannedBy: adminID, CreatedAt: now})
	_ = e.mod.CreateIPBan(ctx, &IPBan{CIDR: "10.0.0.0/24", BannedBy: gone, CreatedAt: now})

	h := NewHandler(e.svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(auth.LocalsKeyUser, e.users.users[adminID])
		return c.Next()
	})
	app.Get("/reports", h.ListReports)
	app.Get("/audit", h.ListAudit)
	app.Get("/bans", h.ListBans)
	app.Get("/ip_bans", h.ListIPBans)
	for _, path := range []string{"/reports", "/audit", "/bans", "/ip_bans"} {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Errorf("%s = %d %s", path, resp.StatusCode, raw)
			continue
		}
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
			t.Errorf("%s: rows %s", path, raw)
		}
	}
}
