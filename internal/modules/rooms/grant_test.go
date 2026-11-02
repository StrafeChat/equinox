package rooms

import (
	"context"
	"slices"
	"testing"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/redistest"
)

// grantRooms keeps rooms and their participants in memory.
type grantRooms struct {
	Repository
	rooms        map[int64]*Room
	participants map[int64][]int64
}

func (r *grantRooms) GetByID(_ context.Context, roomID int64) (*Room, error) {
	if room := r.rooms[roomID]; room != nil {
		cp := *room
		return &cp, nil
	}
	return nil, nil
}

func (r *grantRooms) GetParticipants(_ context.Context, roomID int64) ([]int64, error) {
	return slices.Clone(r.participants[roomID]), nil
}

func (r *grantRooms) AddParticipant(_ context.Context, roomID, userID int64) error {
	r.participants[roomID] = append(r.participants[roomID], userID)
	return nil
}

func (r *grantRooms) Create(_ context.Context, room *Room, ids []int64) error {
	cp := *room
	r.rooms[room.ID] = &cp
	r.participants[room.ID] = slices.Clone(ids)
	return nil
}

func (r *grantRooms) GetPMRoom(context.Context, int64, int64) (*Room, error) { return nil, nil }

func (r *grantRooms) UpdateLastMessageID(context.Context, int64, []int64, int64) error { return nil }

type grantUsers struct{ auth.UserRepository }

func (grantUsers) GetByID(_ context.Context, uid int64) (*auth.User, error) {
	return &auth.User{ID: uid, Username: "user" + id.Format(uid)}, nil
}

func (u grantUsers) GetByIDs(ctx context.Context, ids []int64) ([]*auth.User, error) {
	out := make([]*auth.User, len(ids))
	for i, uid := range ids {
		out[i], _ = u.GetByID(ctx, uid)
	}
	return out, nil
}

const groupRoom = 900

// grantService is a rooms service over a group PM of users 1 and 2, under the open PM policy,
// whose system notices are recorded by how many events were published before them.
func grantService(t *testing.T) (*Service, *redistest.Redis, *[]int) {
	t.Helper()
	rdb, rec := redistest.New()
	t.Cleanup(func() { _ = rdb.Close() })
	repo := &grantRooms{
		rooms:        map[int64]*Room{groupRoom: {ID: groupRoom, Type: TypeGroupPM, Name: "group", CreatorID: 1}},
		participants: map[int64][]int64{groupRoom: {1, 2}},
	}
	cfg := &config.Config{}
	cfg.Flags.PMPolicy = "open"
	var notices []int
	notice := func(context.Context, int64, []int64, string, string) (int64, error) {
		notices = append(notices, len(rec.Published()))
		return 1, nil
	}
	return NewService(repo, grantUsers{}, rdb, cfg, notice, nil), rec, &notices
}

// Someone added to a group PM gets the room first, and the room subscribes their sockets as it
// arrives (Subscribe), so "X added Y" - posted after it - reaches them live.
func TestAddParticipantSendsTheRoomBeforeTheNotice(t *testing.T) {
	svc, rec, notices := grantService(t)
	if err := svc.AddParticipant(context.Background(), 1, groupRoom, 3); err != nil {
		t.Fatal(err)
	}
	events := rec.Published()
	create := -1
	for i, e := range events {
		if e.Type == "ROOM_CREATE" && e.UserID == "3" {
			create = i
		}
	}
	if create < 0 {
		t.Fatalf("no ROOM_CREATE to the person added: %+v", events)
	}
	if len(*notices) != 1 || (*notices)[0] <= create {
		t.Fatalf("notice posted after %v events, ROOM_CREATE is event %d: want the room first", *notices, create)
	}
	if e := events[create]; !slices.Equal(e.Subscribe, []string{id.Format(groupRoom)}) || e.ScopeRoomID != id.Format(groupRoom) || e.ScopeSpaceID != "" {
		t.Errorf("ROOM_CREATE scope %q/%q subscribes %v", e.ScopeSpaceID, e.ScopeRoomID, e.Subscribe)
	}
	for _, e := range events {
		if e.Type == "ROOM_UPDATE" && len(e.Subscribe) > 0 {
			t.Errorf("ROOM_UPDATE to %s subscribes %v", e.UserID, e.Subscribe)
		}
	}
}

// A new PM goes to both participants - the creator's other sessions too, the way Discord sends
// CHANNEL_CREATE to the creator - each subscribed to it; a notes room to its one participant.
func TestCreatePMReachesBothSides(t *testing.T) {
	for _, tc := range []struct {
		name           string
		actor, target  int64
		wantRecipients []string
	}{
		{"pm", 1, 2, []string{"1", "2"}},
		{"notes", 1, 1, []string{"1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, rec, _ := grantService(t)
			rwp, created, err := svc.CreatePM(context.Background(), tc.actor, tc.target)
			if err != nil || !created {
				t.Fatalf("created %v, %v", created, err)
			}
			var to []string
			for _, e := range rec.Published() {
				if e.Type != "ROOM_CREATE" {
					continue
				}
				to = append(to, e.UserID)
				if !slices.Equal(e.Subscribe, []string{id.Format(rwp.ID)}) {
					t.Errorf("ROOM_CREATE to %s subscribes %v", e.UserID, e.Subscribe)
				}
			}
			slices.Sort(to)
			if !slices.Equal(to, tc.wantRecipients) {
				t.Fatalf("ROOM_CREATE to %v, want %v", to, tc.wantRecipients)
			}
		})
	}
}
