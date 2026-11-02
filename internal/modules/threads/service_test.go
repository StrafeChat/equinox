package threads

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// What a thread tells the instances mirroring its space. The origin owns a thread's state, so
// whatever changes it here has to be relayed, in an order a mirror can follow.

const (
	testSpace   = 7
	testChannel = 100
	testThread  = 200
)

type fakeRooms struct {
	rooms.Repository
	byID    map[int64]*rooms.Room
	counted int64
	events  *[]string
}

func (f *fakeRooms) GetByID(_ context.Context, roomID int64) (*rooms.Room, error) {
	return f.byID[roomID], nil
}

func (f *fakeRooms) GetByIDs(context.Context, []int64) ([]*rooms.Room, error) { return nil, nil }

func (f *fakeRooms) GetParticipants(context.Context, int64) ([]int64, error) { return nil, nil }

func (f *fakeRooms) UpdateThread(_ context.Context, threadID int64, p rooms.ThreadPatch) error {
	if p.Archived != nil {
		f.byID[threadID].ThreadArchived = p.Archived
	}
	return nil
}

func (f *fakeRooms) EnqueueThreadArchive(context.Context, int64, time.Time) error { return nil }

func (f *fakeRooms) AddThreadMessages(_ context.Context, _ int64, n int64) error {
	f.counted += n
	return nil
}

func (f *fakeRooms) IsThreadMember(context.Context, int64, int64) (bool, error) { return true, nil }

func (f *fakeRooms) ListThreadMemberIDs(context.Context, int64) ([]int64, error) { return nil, nil }

func (f *fakeRooms) ThreadMessageCounts(context.Context, []int64) (map[int64]int64, error) {
	return nil, nil
}

func (f *fakeRooms) ListUserThreadIDs(context.Context, int64) ([]int64, error) { return nil, nil }

func (f *fakeRooms) CountThreadMembers(context.Context, int64) (int, error) { return 1, nil }

func (f *fakeRooms) ListBySpace(context.Context, int64) ([]rooms.RoomBySpaceRow, error) {
	return nil, nil
}

func (f *fakeRooms) CreateThread(_ context.Context, room *rooms.Room) error {
	f.byID[room.ID] = room
	return nil
}

func (f *fakeRooms) AddThreadMember(context.Context, int64, int64, time.Time) error { return nil }

type fakeSpaces struct{ spaces.Repository }

func (fakeSpaces) GetByID(context.Context, int64) (*spaces.Space, error) {
	return &spaces.Space{ID: testSpace, OwnerID: 1, EveryoneRoleID: 9}, nil
}
func (fakeSpaces) ListSpaceRoles(context.Context, int64) ([]spaces.SpaceRole, error) {
	return nil, nil
}
func (fakeSpaces) ListMembers(context.Context, int64) ([]spaces.SpaceMember, error) {
	return nil, nil
}
func (fakeSpaces) ListRoomRoleOverrides(context.Context, int64, int64) ([]spaces.SpaceRoomRoleOverride, error) {
	return nil, nil
}
func (fakeSpaces) ListRoomUserOverrides(context.Context, int64, int64) ([]spaces.SpaceRoomUserOverride, error) {
	return nil, nil
}

// relaySpy is the federator the spaces service reports room changes to.
type relaySpy struct {
	spaces.Federator
	events *[]string
	rooms  []*rooms.Room
}

func (r *relaySpy) AfterSpaceRoomChanged(_ context.Context, _ int64, room *rooms.Room, _ spaces.RoomOverrides, _ bool) {
	*r.events = append(*r.events, "room relayed")
	r.rooms = append(r.rooms, room)
}

func threadWorld(room *rooms.Room) (*Service, *fakeRooms, *relaySpy) {
	events := &[]string{}
	space, parent := int64(testSpace), int64(testChannel)
	room.ID, room.Type, room.SpaceID, room.ParentID = testThread, rooms.TypeThread, &space, &parent
	roomRepo := &fakeRooms{byID: map[int64]*rooms.Room{
		testThread:  room,
		testChannel: {ID: testChannel, Type: rooms.TypeSpaceText, SpaceID: &space},
	}, events: events}
	spy := &relaySpy{events: events}
	spaceSvc := spaces.NewService(fakeSpaces{}, roomRepo, nil, nil, nil)
	spaceSvc.SetFederator(spy)
	notify := func(_ context.Context, _ int64, _ []int64, eventType, _ string) (int64, error) {
		*events = append(*events, "notice "+eventType)
		return 1, nil
	}
	return NewService(roomRepo, nil, spaceSvc, nil, nil, nil, notify), roomRepo, spy
}

// Before this, a message from anyone unarchived the thread here and every mirror went on
// refusing reactions and edits in it until the next resync.
func TestAMessageThatUnarchivesAThreadIsRelayed(t *testing.T) {
	archived := true
	s, _, spy := threadWorld(&rooms.Room{ThreadArchived: &archived})
	s.OnThreadMessage(context.Background(), s.mustLoad(t), 1, nil)
	if len(spy.rooms) != 1 || spy.rooms[0].ID != testThread || spy.rooms[0].ThreadIsArchived() {
		t.Fatalf("relayed %+v", spy.rooms)
	}
}

func (s *Service) mustLoad(t *testing.T) *rooms.Room {
	t.Helper()
	room, err := s.loadThread(context.Background(), testThread)
	if err != nil {
		t.Fatal(err)
	}
	return room
}

// An ordinary message in a live thread changes nothing a mirror holds; relaying each one would
// be a room upsert per message.
func TestAMessageInALiveThreadIsNotARoomChange(t *testing.T) {
	archived := false
	s, roomRepo, spy := threadWorld(&rooms.Room{ThreadArchived: &archived})
	s.OnThreadMessage(context.Background(), s.mustLoad(t), 1, nil)
	if len(spy.rooms) != 0 {
		t.Fatalf("relayed %+v", spy.rooms)
	}
	if roomRepo.counted != 1 {
		t.Fatalf("counted %d", roomRepo.counted)
	}
}

// A locked thread stays archived whoever writes in it, so there is nothing to tell.
func TestAMessageInALockedThreadRelaysNothing(t *testing.T) {
	archived, locked := true, true
	s, _, spy := threadWorld(&rooms.Room{ThreadArchived: &archived, ThreadLocked: &locked})
	s.OnThreadMessage(context.Background(), s.mustLoad(t), 1, nil)
	if len(spy.rooms) != 0 {
		t.Fatalf("relayed %+v", spy.rooms)
	}
}

// The notice names the thread, so a mirror has to have the thread first: relays to one peer
// keep their order.
func TestANewThreadIsRelayedBeforeItsNotice(t *testing.T) {
	s, roomRepo, _ := threadWorld(&rooms.Room{})
	parent := roomRepo.byID[testChannel]
	if _, err := s.create(context.Background(), 1, parent, 201, CreateInput{Name: "plans"}, false, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"room relayed", "notice " + SystemThreadCreated}
	if !slices.Equal(*roomRepo.events, want) {
		t.Fatalf("order %v", *roomRepo.events)
	}
}
