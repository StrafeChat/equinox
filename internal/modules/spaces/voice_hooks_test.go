package spaces

import (
	"context"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// The hooks the voice module hangs off: a room's call has to end while the room still loads,
// and a mirror's removal of a member cannot depend on the relay voice sends separately.

const hookRoom = 900

// hookRepo is visRepo with the rows that go when a member or a space does.
type hookRepo struct{ *visRepo }

func (r *hookRepo) RemoveMember(_ context.Context, spaceID, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.members[spaceID], userID)
	return nil
}

func (r *hookRepo) DeleteSpace(context.Context, int64) error { return nil }

// hookWorld records the hook calls and whether the room was still there for each.
type hookWorld struct {
	*visWorld
	going    []int64
	loadable []bool
	removed  []int64
}

func newHookWorld() *hookWorld {
	w := &hookWorld{visWorld: newVisWorld()}
	w.svc = NewService(&hookRepo{w.repo}, w.rooms, w.users, w.svc.redis, w.svc.cfg)
	w.svc.SetFederator(w.fed)
	w.rooms.add(rooms.Room{ID: hookRoom, Type: rooms.TypeSpaceVoice})
	w.svc.SetRoomRemovedHook(func(_ context.Context, spaceID, roomID int64) {
		w.going = append(w.going, roomID)
		_, ok := w.rooms.byID[roomID]
		w.loadable = append(w.loadable, ok && spaceID == rfSpace)
	})
	w.svc.SetMemberRemovedHook(func(_ context.Context, _, userID int64) {
		w.removed = append(w.removed, userID)
	})
	return w
}

func (w *hookWorld) sawRoomLoad(t *testing.T) {
	t.Helper()
	if len(w.going) != 1 || w.going[0] != hookRoom {
		t.Fatalf("hook saw %v", w.going)
	}
	if !w.loadable[0] {
		t.Fatal("the hook ran after the room was gone: voice could not say who to tell")
	}
	if _, ok := w.rooms.byID[hookRoom]; ok {
		t.Fatal("the room is still there")
	}
}

func TestDeleteRoomTellsTheHookFirst(t *testing.T) {
	w := newHookWorld()
	if err := w.svc.DeleteRoom(context.Background(), rfOwnerID, rfSpace, hookRoom); err != nil {
		t.Fatal(err)
	}
	w.sawRoomLoad(t)
}

func TestMirrorDeleteTellsTheHookFirst(t *testing.T) {
	w := newHookWorld()
	w.mirror()
	if err := w.svc.ApplyMirrorRoom(context.Background(), rfSpace, MirrorRoom{Room: &rooms.Room{ID: hookRoom}}, true); err != nil {
		t.Fatal(err)
	}
	w.sawRoomLoad(t)
}

func TestSpaceCascadeTellsTheHookWhileMembersCanStillBeRead(t *testing.T) {
	ctx := context.Background()
	w := newHookWorld()
	sp, err := w.repo.GetByID(ctx, rfSpace)
	if err != nil || sp == nil {
		t.Fatalf("space %v: %v", sp, err)
	}
	members := -1
	w.svc.SetRoomRemovedHook(func(ctx context.Context, _, roomID int64) {
		w.going = append(w.going, roomID)
		list, _ := w.repo.ListMembers(ctx, rfSpace)
		members = len(list)
	})
	want, _ := w.repo.ListMembers(ctx, rfSpace)
	if err := w.svc.deleteSpaceCascade(ctx, 0, sp); err != nil {
		t.Fatal(err)
	}
	if len(w.going) != 1 || w.going[0] != hookRoom {
		t.Fatalf("hook saw %v", w.going)
	}
	if members != len(want) {
		t.Fatalf("hook saw %d members of %d: a private room's last left would reach nobody", members, len(want))
	}
}

func TestRemovingAMirrorMemberRunsTheVoiceHook(t *testing.T) {
	ctx := context.Background()
	w := newHookWorld()
	w.mirror()
	if err := w.svc.RemoveMirrorMember(ctx, rfSpace, rfPlainID); err != nil {
		t.Fatal(err)
	}
	if !eq64(w.removed, []int64{rfPlainID}) {
		t.Fatalf("hook saw %v", w.removed)
	}
	// A repeat has nothing left to remove, and says so to nobody.
	if err := w.svc.RemoveMirrorMember(ctx, rfSpace, rfPlainID); err != nil {
		t.Fatal(err)
	}
	if len(w.removed) != 1 {
		t.Fatalf("hook saw %v", w.removed)
	}
}

func eq64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
