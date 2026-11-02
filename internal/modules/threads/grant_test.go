package threads

import (
	"slices"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// THREAD_CREATE and joining a thread give the recipients the thread's channel; other thread
// events, and leaving, give nothing.
func TestThreadGrants(t *testing.T) {
	space, parent := int64(500), int64(600)
	room := &rooms.Room{ID: 700, Type: rooms.TypeThread, SpaceID: &space, ParentID: &parent}
	for _, tc := range []struct {
		event   string
		payload map[string]interface{}
		opens   bool
	}{
		{"THREAD_CREATE", map[string]interface{}{}, true},
		{"THREAD_MEMBER_UPDATE", map[string]interface{}{"joined": true}, true},
		{"THREAD_MEMBER_UPDATE", map[string]interface{}{"joined": false}, false},
		{"THREAD_UPDATE", map[string]interface{}{}, false},
		{"THREAD_MEMBERS_UPDATE", map[string]interface{}{"member_count": 2}, false},
	} {
		sc := threadScope(room, tc.event, tc.payload)
		if sc.SpaceID != space || sc.RoomID != room.ID {
			t.Errorf("%s %v scoped to %d/%d", tc.event, tc.payload, sc.SpaceID, sc.RoomID)
		}
		want := []int64(nil)
		if tc.opens {
			want = []int64{room.ID}
		}
		if !slices.Equal(sc.Subscribe, want) {
			t.Errorf("%s %v subscribes %v, want %v", tc.event, tc.payload, sc.Subscribe, want)
		}
	}
}
