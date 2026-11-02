package voice

import (
	"context"
	"testing"

	"github.com/StrafeChat/equinox/internal/id"
)

// A voice state names its space and room on every channel it takes: the space's (an open room),
// the room's own and the user's (a private one).
func TestVoiceStatesNameTheirSpaceAndRoom(t *testing.T) {
	for _, roomID := range []int64{wLounge, wSecret} {
		w := newWorld(t)
		w.svc.publishState(context.Background(), w.room(roomID), w.state(roomID, wAlice, "s1"), false)
		got := w.voiceEvents(roomID)
		if len(got) == 0 {
			t.Fatalf("room %d: no events", roomID)
		}
		space, room := id.Format(wSpace), id.Format(roomID)
		for _, e := range got {
			if !e.Scoped || e.ScopeSpaceID != space || e.ScopeRoomID != room {
				t.Errorf("room %d: %+v scoped to %q / %q", roomID, e, e.ScopeSpaceID, e.ScopeRoomID)
			}
			if e.Data["space_id"] != space {
				t.Errorf("room %d: payload space_id %v", roomID, e.Data["space_id"])
			}
		}
	}
}
