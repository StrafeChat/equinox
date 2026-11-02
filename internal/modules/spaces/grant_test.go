package spaces

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// joinRooms is visRooms with the read state a join writes (history before it is read) and
// SPACE_CREATE reads back.
type joinRooms struct {
	*visRooms
	readMu sync.Mutex
	read   map[int64]map[int64]int64 // user -> room -> last read message
}

func (r *joinRooms) UpdateReadState(_ context.Context, userID, roomID, last int64) error {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if r.read[userID] == nil {
		r.read[userID] = map[int64]int64{}
	}
	r.read[userID][roomID] = last
	return nil
}

func (r *joinRooms) ListByUser(_ context.Context, userID int64) ([]rooms.RoomRow, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	var out []rooms.RoomRow
	for rid, last := range r.read[userID] {
		v := last
		out = append(out, rooms.RoomRow{UserID: userID, RoomID: rid, LastReadMessageID: &v})
	}
	return out, nil
}

func (r *joinRooms) GetMentionCount(context.Context, int64, int64) (int, error) { return 0, nil }
func (r *joinRooms) SetMentionCountBaseline(context.Context, int64, int64, int64) error {
	return nil
}
func (r *joinRooms) GetMentionCounts(context.Context, int64) (map[int64]int, error) {
	return map[int64]int{}, nil
}

const (
	joinerID    = 7
	lastMessage = 9000 // the public room's last message, before anyone joined
)

// joinWorld is the visibility fixture's space, with a system room, for a user joining it: the
// public room (the system room) with a thread in it, and the staff room only the member role
// may view. Its system messenger records the join notice in the event log, in order.
func joinWorld(t *testing.T) (*Service, *visRepo, *eventLog) {
	t.Helper()
	repo := newVisRepo()
	roomRepo := &joinRooms{visRooms: newVisRooms(), read: map[int64]map[int64]int64{}}
	public := textRoom(roomPublic, "general")
	last := int64(lastMessage)
	public.LastMessageID = &last
	roomRepo.add(public)
	roomRepo.add(textRoom(roomStaff, "staff"))
	roomRepo.add(threadRoom(roomThread, roomPublic, false))
	repo.roleOv[roomStaff] = map[int64]SpaceRoomRoleOverride{
		rfEveryone: {SpaceID: rfSpace, RoomID: roomStaff, RoleID: rfEveryone, Deny: denyView(0).Deny},
		rfMember:   {SpaceID: rfSpace, RoomID: roomStaff, RoleID: rfMember, Allow: allowView(0).Allow},
	}
	system := int64(roomPublic)
	repo.spaces[rfSpace].SystemRoomID = &system

	log := &eventLog{}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rdb.AddHook(&kvHook{eventLog: log, kv: map[string]string{}})
	t.Cleanup(func() { _ = rdb.Close() })
	cfg := &config.Config{}
	cfg.Database.Redis.CacheEnabled = true
	svc := NewService(repo, roomRepo, fixtureUsers(rfOwnerID, rfAdminID, rfModID, rfMemberID, rfPlainID, rfMod2ID, joinerID), rdb, cfg)
	svc.runLater = func(f func()) { f() }
	svc.SetFederator(&visFed{recordingFed: recordingFed{mirrors: map[int64]string{}}})
	svc.SetSystemMessenger(func(_ context.Context, roomID int64, _ []int64, eventType, _ string) (int64, error) {
		log.mu.Lock()
		defer log.mu.Unlock()
		log.events = append(log.events, sentEvent{Type: "MESSAGE_CREATE", Data: map[string]any{"room_id": id.Format(roomID), "system": eventType}})
		return 1, nil
	})
	return svc, repo, log
}

// position is where the first event of a type to a user (0: on a space channel) is in the log.
func position(t *testing.T, log *eventLog, eventType string, userID int64) int {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	for i, e := range log.events {
		if e.Type != eventType {
			continue
		}
		if userID == 0 || e.UserID == id.Format(userID) {
			return i
		}
	}
	t.Fatalf("no %s for user %d in %+v", eventType, userID, log.events)
	return -1
}

func spaceCreateFor(t *testing.T, log *eventLog, userID int64) sentEvent {
	t.Helper()
	for _, e := range log.of("SPACE_CREATE") {
		if e.UserID == id.Format(userID) {
			return e
		}
	}
	t.Fatalf("no SPACE_CREATE for %d", userID)
	return sentEvent{}
}

// roomIDsOf is the ids of a SPACE_CREATE's rooms, sorted.
func roomIDsOf(t *testing.T, e sentEvent) []string {
	t.Helper()
	list, ok := e.Data["rooms"].([]any)
	if !ok {
		t.Fatalf("SPACE_CREATE without rooms: %+v", e.Data)
	}
	var out []string
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		s, _ := m["id"].(string)
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func sortedIDs(ids ...int64) []string {
	out := make([]string, 0, len(ids))
	for _, x := range ids {
		out = append(out, id.Format(x))
	}
	slices.Sort(out)
	return out
}

// Joining sends SPACE_CREATE before anything else about the join - Discord's GUILD_CREATE
// before the join message - because the gateway subscribes the joiner's sockets as it delivers
// it: their own join notice and SPACE_MEMBER_ADD then reach them live. It names the space and
// every room and active thread the joiner may view, and nothing they may not.
func TestJoinAnnouncesTheSpaceFirst(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		roles []int64
		rooms []int64
	}{
		{"plain member", []int64{rfEveryone}, []int64{roomPublic, roomThread}},
		{"member role", []int64{rfEveryone, rfMember}, []int64{roomPublic, roomStaff, roomThread}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, log := joinWorld(t)
			sp, _ := repo.GetByID(ctx, rfSpace)
			if err := svc.addMember(ctx, sp, joinerID, tc.roles); err != nil {
				t.Fatal(err)
			}
			create := position(t, log, "SPACE_CREATE", joinerID)
			notice := position(t, log, "MESSAGE_CREATE", 0)
			added := position(t, log, "SPACE_MEMBER_ADD", 0)
			if !(create < notice && notice < added) {
				t.Fatalf("SPACE_CREATE at %d, join notice at %d, SPACE_MEMBER_ADD at %d: want them in that order", create, notice, added)
			}
			e := spaceCreateFor(t, log, joinerID)
			if !e.Scoped || e.ScopeSpaceID != id.Format(rfSpace) || e.ScopeRoomID != "" {
				t.Errorf("SPACE_CREATE scope %v %q %q", e.Scoped, e.ScopeSpaceID, e.ScopeRoomID)
			}
			if got, want := roomIDsOf(t, e), sortedIDs(tc.rooms...); !slices.Equal(got, want) {
				t.Errorf("rooms %v, want %v", got, want)
			}
			sub := slices.Clone(e.Subscribe)
			slices.Sort(sub)
			if want := sortedIDs(append([]int64{rfSpace}, tc.rooms...)...); !slices.Equal(sub, want) {
				t.Errorf("subscribes %v, want %v", sub, want)
			}
			if e.Data["everyone_role_id"] != id.Format(rfEveryone) || e.Data["roles"] == nil {
				t.Errorf("SPACE_CREATE without its roles: %+v", e.Data)
			}
		})
	}
}

// SPACE_CREATE's rooms are READY's space_rooms entries for the same user, built by the same
// code (ViewerSpaceRooms): overrides, global identity, a thread's state, and the read state the
// join just wrote, so the space does not open with every room unread.
func TestSpaceCreateRoomsAreREADYs(t *testing.T) {
	ctx := context.Background()
	svc, repo, log := joinWorld(t)
	sp, _ := repo.GetByID(ctx, rfSpace)
	if err := svc.addMember(ctx, sp, joinerID, []int64{rfEveryone, rfMember}); err != nil {
		t.Fatal(err)
	}
	ready, _, _, err := svc.ViewerSpaceRooms(ctx, joinerID, rfSpace, svc.LoadReadState(ctx, joinerID))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ready)
	var want []any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got := spaceCreateFor(t, log, joinerID).Data["rooms"]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SPACE_CREATE rooms\n%v\nREADY's\n%v", got, want)
	}
	for _, r := range want {
		m := r.(map[string]any)
		switch m["id"] {
		case id.Format(roomPublic):
			if m["last_read_message_id"] != id.Format(lastMessage) {
				t.Errorf("public room read marker %v, want %d", m["last_read_message_id"], lastMessage)
			}
			if m["federation"] == nil {
				t.Error("public room without its federation identity")
			}
		case id.Format(roomThread):
			if th, _ := m["thread"].(map[string]any); th == nil || th["message_count"] == nil {
				t.Errorf("thread without its state: %v", m)
			}
		}
	}
}

// A mirror announcing a member it stored keeps the same order: SPACE_CREATE, then
// SPACE_MEMBER_ADD.
func TestMirrorMemberAnnouncedSpaceFirst(t *testing.T) {
	ctx := context.Background()
	svc, repo, log := joinWorld(t)
	if err := repo.AddMember(ctx, rfSpace, joinerID, []int64{rfEveryone}, MemberProfile{}); err != nil {
		t.Fatal(err)
	}
	svc.AnnounceMirrorMember(ctx, rfSpace, joinerID)
	if create, added := position(t, log, "SPACE_CREATE", joinerID), position(t, log, "SPACE_MEMBER_ADD", 0); create > added {
		t.Fatalf("SPACE_CREATE at %d after SPACE_MEMBER_ADD at %d", create, added)
	}
	if got, want := roomIDsOf(t, spaceCreateFor(t, log, joinerID)), sortedIDs(roomPublic, roomThread); !slices.Equal(got, want) {
		t.Errorf("rooms %v, want %v", got, want)
	}
}

// A room created open to everyone goes out on the space's channel and opens itself to whoever
// hears it; a private one goes to its viewers, each subscribed to it.
func TestRoomCreateOpensTheRoom(t *testing.T) {
	ctx := context.Background()
	t.Run("open to everyone", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.settle(t)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(textRoom(roomPublic, "public")), false); err != nil {
			t.Fatal(err)
		}
		creates := w.log.of("SPACE_ROOM_CREATE")
		if len(creates) != 1 || creates[0].SpaceID == "" {
			t.Fatalf("creates %+v", creates)
		}
		if !slices.Equal(creates[0].Subscribe, []string{id.Format(roomPublic)}) {
			t.Errorf("subscribes %v", creates[0].Subscribe)
		}
	})
	t.Run("private", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.settle(t)
		mr := mirrored(textRoom(roomStaff, "staff"), denyView(rfEveryone), allowView(rfMember))
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mr, false); err != nil {
			t.Fatal(err)
		}
		creates := w.log.of("SPACE_ROOM_CREATE")
		if len(creates) == 0 {
			t.Fatal("no SPACE_ROOM_CREATE")
		}
		for _, e := range creates {
			if e.UserID == "" || !slices.Equal(e.Subscribe, []string{id.Format(roomStaff)}) {
				t.Errorf("create to %q subscribes %v", e.UserID, e.Subscribe)
			}
		}
	})
}
