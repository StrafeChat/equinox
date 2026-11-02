package spaces

import (
	"context"
	"testing"

	"github.com/StrafeChat/equinox/internal/id"
)

// A room's create names its space and itself, whether it goes to the space's own channel (a
// room everyone may see) or to its viewers one by one; a space-wide event names the space alone.
func TestSpaceEventsNameTheirSpaceAndRoom(t *testing.T) {
	ctx := context.Background()
	space := id.Format(rfSpace)
	check := func(t *testing.T, e sentEvent, room string) {
		t.Helper()
		if !e.Scoped || e.ScopeSpaceID != space || e.ScopeRoomID != room {
			t.Errorf("%s scoped %v to space %q room %q, want %q / %q", e.Type, e.Scoped, e.ScopeSpaceID, e.ScopeRoomID, space, room)
		}
		if e.Data["space_id"] != space {
			t.Errorf("%s payload space_id %v", e.Type, e.Data["space_id"])
		}
		if got, ok := e.Data["room_id"]; (room == "" && ok) || (room != "" && got != room) {
			t.Errorf("%s payload room_id %v, want %q", e.Type, got, room)
		}
	}

	t.Run("public room, on the space channel", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.settle(t)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(textRoom(roomPublic, "public")), false); err != nil {
			t.Fatal(err)
		}
		creates := w.log.of("SPACE_ROOM_CREATE")
		if len(creates) != 1 || creates[0].SpaceID != space || creates[0].UserID != "" {
			t.Fatalf("creates %+v", creates)
		}
		check(t, creates[0], id.Format(roomPublic))
	})

	t.Run("private room, to its viewers", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.settle(t)
		mr := mirrored(textRoom(roomStaff, "staff"), denyView(rfEveryone), allowView(rfMember))
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mr, false); err != nil {
			t.Fatal(err)
		}
		creates := w.log.of("SPACE_ROOM_CREATE")
		if len(creates) != len(privateViewers) {
			t.Fatalf("creates %+v", creates)
		}
		for _, e := range creates {
			if e.SpaceID != "" || e.UserID == "" {
				t.Errorf("went out on space %q / user %q", e.SpaceID, e.UserID)
			}
			check(t, e, id.Format(roomStaff))
		}
		// The map went to several users: each copy is the same, none leaks another's ids.
		for _, e := range w.log.of("SPACE_ROOM_DELETE") {
			check(t, e, id.Format(roomStaff))
		}
	})

	t.Run("space-wide", func(t *testing.T) {
		w := newVisWorld()
		w.svc.publishSpaceEvent(ctx, rfSpace, "SPACE_UPDATE", map[string]interface{}{"id": space})
		updates := w.log.of("SPACE_UPDATE")
		if len(updates) != 1 || updates[0].SpaceID != space {
			t.Fatalf("updates %+v", updates)
		}
		check(t, updates[0], "")
	})
}
