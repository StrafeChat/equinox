package main

import (
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// READY used to report every room with a null e2ee_enabled as encrypted, so a thread the
// user had joined (listed in READY's rooms, not only under space_rooms) came through as
// E2EE although the server stores its messages as plaintext.
func TestReadyRoomMapE2EEDefault(t *testing.T) {
	space := int64(7)
	on, off := true, false
	cases := []struct {
		name string
		room rooms.Room
		want bool
	}{
		{"pm null", rooms.Room{ID: 1, Type: rooms.TypePM}, true},
		{"group pm null", rooms.Room{ID: 2, Type: rooms.TypeGroupPM, CreatorID: 9}, true},
		{"group pm off", rooms.Room{ID: 3, Type: rooms.TypeGroupPM, CreatorID: 9, E2EEEnabled: &off}, false},
		{"thread null", rooms.Room{ID: 4, Type: rooms.TypeThread, SpaceID: &space}, false},
		{"archived thread null", rooms.Room{ID: 5, Type: rooms.TypeThread, SpaceID: &space, ThreadArchived: &on}, false},
		{"space text null", rooms.Room{ID: 6, Type: rooms.TypeSpaceText, SpaceID: &space}, false},
		{"thread on", rooms.Room{ID: 7, Type: rooms.TypeThread, SpaceID: &space, E2EEEnabled: &on}, true},
	}
	for _, c := range cases {
		m := readyRoomMap(rooms.RoomWithParticipants{Room: c.room})
		if got, ok := m["e2ee_enabled"].(bool); !ok || got != c.want {
			t.Errorf("%s: e2ee_enabled = %v, want %v", c.name, m["e2ee_enabled"], c.want)
		}
	}
}
