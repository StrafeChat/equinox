package messages

import (
	"context"
	"reflect"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// The column a send must fill (roomE2EEOff) and the e2ee_enabled every wire form reports
// (rooms.Room.E2EEOn) must agree for every room that carries messages, or a client
// encrypts for a room the server stores as plaintext (or the reverse) and every send is
// refused.
func TestRoomE2EEOffAgreesWithWireFlag(t *testing.T) {
	space := int64(7)
	on, off := true, false
	for _, typ := range []int{rooms.TypePM, rooms.TypeGroupPM, rooms.TypeSpaceText, rooms.TypeSpaceVoice, rooms.TypeThread} {
		for _, flag := range []*bool{nil, &on, &off} {
			r := &rooms.Room{Type: typ, E2EEEnabled: flag}
			if typ != rooms.TypePM && typ != rooms.TypeGroupPM {
				r.SpaceID = &space
			}
			if roomE2EEOff(r) == r.E2EEOn() {
				t.Errorf("type %d flag %v: roomE2EEOff = %v but E2EEOn = %v", typ, flagString(flag), roomE2EEOff(r), r.E2EEOn())
			}
		}
	}
}

func flagString(b *bool) string {
	if b == nil {
		return "null"
	}
	if *b {
		return "true"
	}
	return "false"
}

func TestRoleMentionRecipients(t *testing.T) {
	// notify is already sender-free and deduped (resolveMentionNotifySet); 2 was mentioned
	// directly only, 3 directly and through a role, 4 through a role only, 5 holds the role
	// but is not in the notify set (the sender, say).
	got := roleMentionRecipients([]int64{2, 3, 4}, []int64{3, 4, 4, 5})
	if want := []int64{3, 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("recipients = %v, want %v", got, want)
	}
	if got := roleMentionRecipients([]int64{2, 3}, nil); got != nil {
		t.Errorf("no role mention: recipients = %v, want none", got)
	}
}

// fakeMentionRooms answers the two reads a role-mention update makes; any other
// Repository method panics on the nil embedded interface.
type fakeMentionRooms struct {
	rooms.Repository
	totals    map[int64]int
	baselines map[int64]int64
}

func (f *fakeMentionRooms) GetMentionCount(_ context.Context, userID, _ int64) (int, error) {
	return f.totals[userID], nil
}

func (f *fakeMentionRooms) GetRoomRow(_ context.Context, userID, roomID int64) (*rooms.RoomRow, error) {
	b, ok := f.baselines[userID]
	if !ok {
		return nil, nil // no rooms_by_user row yet
	}
	return &rooms.RoomRow{UserID: userID, RoomID: roomID, MentionCountBaseline: &b}, nil
}

type fakeMentionAuth struct {
	SpaceChannelAuth
	perms map[int64]int64
}

func (f *fakeMentionAuth) EffectiveChannelPermissions(_ context.Context, userID, _, _ int64) (int64, error) {
	return f.perms[userID], nil
}

// Each role holder who can see the room gets their own badge as it now stands (raw total
// minus their ack baseline), with the message it is as of - and nothing else.
func TestEachRoleMentionUpdate(t *testing.T) {
	space := int64(7)
	room := &rooms.Room{ID: 100, Type: rooms.TypeSpaceText, SpaceID: &space}
	view := permissions.PermViewRoom
	s := &Service{
		rooms: &fakeMentionRooms{
			totals:    map[int64]int{2: 3, 3: 1, 4: 5, 5: 2},
			baselines: map[int64]int64{2: 1, 4: 6, 5: 0},
		},
		spaceAuth: &fakeMentionAuth{perms: map[int64]int64{2: view, 3: view, 4: view, 5: 0}},
	}
	got := map[int64]map[string]interface{}{}
	s.eachRoleMentionUpdate(context.Background(), room, 555, []int64{2, 3, 4, 5}, func(uid int64, payload map[string]interface{}) {
		got[uid] = payload
	})

	want := map[int64]int64{
		2: 2, // 3 total, acked at 1
		3: 1, // no rooms_by_user row: nothing acked yet
		4: 0, // baseline past the total (acked in between) - never negative
	}
	if len(got) != len(want) {
		t.Fatalf("updates for %v, want users %v", keys(got), want)
	}
	for uid, count := range want {
		p, ok := got[uid]
		if !ok {
			t.Errorf("user %d got no update", uid)
			continue
		}
		if p["mention_count"] != count {
			t.Errorf("user %d mention_count = %v, want %d", uid, p["mention_count"], count)
		}
		if p["room_id"] != "100" || p["message_id"] != "555" {
			t.Errorf("user %d payload = %v", uid, p)
		}
		if len(p) != 3 {
			t.Errorf("user %d payload carries more than the count: %v", uid, p)
		}
	}
	if _, ok := got[5]; ok {
		t.Error("user who cannot view the room was told about it")
	}
}

// Without a space there is no role to have been mentioned through.
func TestEachRoleMentionUpdateSkipsNonSpaceRooms(t *testing.T) {
	s := &Service{rooms: &fakeMentionRooms{}, spaceAuth: &fakeMentionAuth{}}
	s.eachRoleMentionUpdate(context.Background(), &rooms.Room{ID: 1, Type: rooms.TypeGroupPM}, 2, []int64{3}, func(int64, map[string]interface{}) {
		t.Error("update emitted for a room outside any space")
	})
}

func keys(m map[int64]map[string]interface{}) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
