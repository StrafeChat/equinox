package spaces

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// Who hears about which room. Everything here goes through the one view rule; the tests pin
// what it decides and, on a mirror, that nothing is announced until it can be decided on what
// was just written.

const (
	roomSection = 1000 // a section
	roomChild   = 1001 // a channel in it
	roomPublic  = 1002 // a channel at the top level
	roomStaff   = 1003 // a channel at the top level, private in the tests that say so
	roomThread  = 1004 // a thread
	roomSecret  = 1005 // another thread
)

func textRoom(roomID int64, name string) rooms.Room {
	off := false
	return rooms.Room{ID: roomID, Type: rooms.TypeSpaceText, Name: name, Position: int(roomID % 100), E2EEEnabled: &off, PermissionsSynced: &off}
}

func sectionRoom(roomID int64) rooms.Room {
	return rooms.Room{ID: roomID, Type: rooms.TypeRoomSection, Name: "section", Position: int(roomID % 100)}
}

func threadRoom(roomID, parent int64, private bool) rooms.Room {
	off, priv := false, private
	return rooms.Room{ID: roomID, Type: rooms.TypeThread, Name: "thread", ParentID: &parent, ThreadPrivate: &priv, E2EEEnabled: &off, ThreadAutoArchiveMin: 60}
}

func child(r rooms.Room, parent int64, synced bool) rooms.Room {
	r.ParentID = &parent
	r.PermissionsSynced = &synced
	return r
}

// mirrored is a room as the origin would report it, overrides in the wire's shape.
func mirrored(r rooms.Room, ov ...SpaceRoomRoleOverride) MirrorRoom {
	cp := r
	return MirrorRoom{Room: &cp, Overrides: RoomOverrides{Roles: ov}}
}

func denyView(roleID int64) SpaceRoomRoleOverride {
	return SpaceRoomRoleOverride{RoleID: roleID, Deny: permissions.PermViewRoom}
}

func allowView(roleID int64) SpaceRoomRoleOverride {
	return SpaceRoomRoleOverride{RoleID: roleID, Allow: permissions.PermViewRoom}
}

// The members and who can see a room denied to @everyone and allowed to the member role: the
// owner, the Administrator and the role's holder.
var (
	privateViewers = i64s(rfOwnerID, rfAdminID, rfMemberID)
	privateOthers  = i64s(rfModID, rfPlainID, rfMod2ID)
)

// privateWorld stores: a section @everyone cannot see, a channel synced to it, a public
// channel, one private to the member role, a thread in that one, and a private thread in the
// public channel that the plain member joined.
func privateWorld(t *testing.T) *visWorld {
	t.Helper()
	w := newVisWorld()
	w.rooms.add(sectionRoom(roomSection))
	w.rooms.add(child(textRoom(roomChild, "child"), roomSection, true))
	w.rooms.add(textRoom(roomPublic, "public"))
	w.rooms.add(textRoom(roomStaff, "staff"))
	w.rooms.add(threadRoom(roomThread, roomStaff, false))
	w.rooms.add(threadRoom(roomSecret, roomPublic, true))
	w.rooms.members[roomSecret] = i64s(rfPlainID)
	w.deny(roomSection, rfEveryone)
	w.allow(roomSection, rfMember)
	w.deny(roomStaff, rfEveryone)
	w.allow(roomStaff, rfMember)
	w.settle(t)
	return w
}

func TestViewRule(t *testing.T) {
	w := privateWorld(t)
	w.repo.addRole(rfSpace, SpaceRole{ID: rfMod, Name: "mod", Position: 2, Permissions: rfModPerms | permissions.PermManageThreads})
	w.settle(t)
	ctx := context.Background()
	snap, err := w.svc.Snapshot(ctx, rfSpace)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		room int64
		user int64
		want bool
	}{
		{"owner in a private room", roomStaff, rfOwnerID, true},
		{"Administrator in a private room", roomStaff, rfAdminID, true},
		{"role allowed", roomStaff, rfMemberID, true},
		{"no role", roomStaff, rfPlainID, false},
		{"anyone in a public room", roomPublic, rfPlainID, true},
		{"channel synced to a private section, no role", roomChild, rfPlainID, false},
		{"channel synced to a private section, role allowed there", roomChild, rfMemberID, true},
		{"public thread of a private channel, no role", roomThread, rfPlainID, false},
		{"public thread of a private channel, role allowed", roomThread, rfMemberID, true},
		{"private thread, joined", roomSecret, rfPlainID, true},
		{"private thread, not joined", roomSecret, rfMemberID, false},
		{"private thread, Manage Threads", roomSecret, rfModID, true},
		{"private thread, owner", roomSecret, rfOwnerID, true},
		{"private thread, Administrator", roomSecret, rfAdminID, true},
	}
	for _, tc := range cases {
		roles, _, _ := w.repo.MemberRoleIDs(ctx, rfSpace, tc.user)
		if got := snap.CanViewRoom(tc.room, tc.user, roles, w.svc.threadRoster(ctx, tc.room)); got != tc.want {
			t.Errorf("%s: CanViewRoom = %v, want %v", tc.name, got, tc.want)
		}
	}
	if (*Snapshot)(nil).CanViewRoom(roomPublic, rfOwnerID, nil, nil) {
		t.Error("a nil snapshot lets someone in")
	}
}

// Every place that answers "who can see this room" gives one answer: READY's list, the
// permission check behind the gateway authorizer, ViewersOf and republishRoomVisibility.
func TestVisibilityAnswersAgree(t *testing.T) {
	w := privateWorld(t)
	ctx := context.Background()
	members, _ := w.repo.ListMembers(ctx, rfSpace)
	for _, room := range []int64{roomSection, roomChild, roomPublic, roomStaff, roomThread, roomSecret} {
		viewers, everyone := w.svc.ViewersOf(ctx, rfSpace, room)
		w.svc.republishRoomVisibility(ctx, rfSpace, []int64{room})
		created, _ := w.sentTo("SPACE_ROOM_CREATE", room)
		for _, m := range members {
			list, _, err := w.svc.VisibleSpaceRooms(ctx, m.UserID, rfSpace)
			if err != nil {
				t.Fatal(err)
			}
			listed := slices.ContainsFunc(list, func(r *rooms.Room) bool { return r.ID == room })
			perms, err := w.svc.EffectiveChannelPermissions(ctx, m.UserID, rfSpace, room)
			if err != nil {
				t.Fatal(err)
			}
			if can := permissions.Has(perms, permissions.PermViewRoom); can != listed {
				t.Errorf("room %d user %d: permission check %v, READY %v", room, m.UserID, can, listed)
			}
			if in := everyone || slices.Contains(viewers, m.UserID); in != listed {
				t.Errorf("room %d user %d: ViewersOf %v, READY %v", room, m.UserID, in, listed)
			}
			if told := slices.Contains(created, m.UserID); told != listed {
				t.Errorf("room %d user %d: republish told %v, READY %v", room, m.UserID, told, listed)
			}
		}
	}
}

// A snapshot cached a moment before a room existed knows nothing of it. Read as a room with no
// overrides, a new private thread would be listed for everyone until the snapshot expired.
func TestVisibleSpaceRoomsWaitsForTheSnapshotToCatchUp(t *testing.T) {
	w := privateWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Snapshot(ctx, rfSpace); err != nil { // the stale copy, cached
		t.Fatal(err)
	}
	const fresh = 1099
	w.rooms.add(threadRoom(fresh, roomPublic, true))
	for _, user := range []int64{rfPlainID, rfOwnerID} {
		list, _, err := w.svc.VisibleSpaceRooms(ctx, user, rfSpace)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(list, func(r *rooms.Room) bool { return r.ID == fresh }) {
			t.Errorf("user %d was shown a room the snapshot has not seen", user)
		}
	}
	w.svc.invalidateSnapshot(ctx, rfSpace)
	list, _, err := w.svc.VisibleSpaceRooms(ctx, rfOwnerID, rfSpace)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(list, func(r *rooms.Room) bool { return r.ID == fresh }) {
		t.Error("the room stayed hidden after the snapshot was rebuilt")
	}
}

func TestOpenToEveryone(t *testing.T) {
	const (
		plain   = 1
		denied  = 2
		oneRole = 3
		oneUser = 4
		allowed = 5
		synced  = 6
		section = 7
		thread  = 8
		secret  = 9
		inner   = 10
	)
	snap := &Snapshot{
		OwnerID: 99, EveryoneRoleID: rfEveryone,
		Roles: []SpaceRole{{ID: rfEveryone, Permissions: permissions.DefaultEveryone}, {ID: rfMember}},
		Overrides: map[int64]RoomOverrides{
			denied:  {Roles: []SpaceRoomRoleOverride{denyView(rfEveryone)}},
			oneRole: {Roles: []SpaceRoomRoleOverride{denyView(rfMember)}},
			oneUser: {Users: []SpaceRoomUserOverride{{UserID: 5, Deny: permissions.PermViewRoom}}},
			allowed: {Roles: []SpaceRoomRoleOverride{allowView(rfMember), {RoleID: rfEveryone, Deny: permissions.PermSendMessages}}},
			section: {Roles: []SpaceRoomRoleOverride{denyView(rfEveryone)}},
		},
		Meta: map[int64]RoomMeta{
			plain: {}, denied: {}, oneRole: {}, oneUser: {}, allowed: {}, section: {},
			synced: {ParentID: section, Synced: true},
			thread: {ParentID: plain, Thread: true},
			secret: {ParentID: plain, Thread: true, Private: true},
			inner:  {ParentID: denied, Thread: true},
		},
	}
	const unknown = 11 // created after this snapshot was built
	want := map[int64]bool{
		plain: true, denied: false, oneRole: false, oneUser: false, allowed: true,
		synced: false, thread: true, secret: false, inner: false, unknown: false,
	}
	for room, open := range want {
		if got := snap.openToEveryone(room); got != open {
			t.Errorf("room %d: openToEveryone = %v, want %v", room, got, open)
		}
	}
}

func TestFailsClosedWithoutASnapshot(t *testing.T) {
	w := privateWorld(t)
	ctx := context.Background()
	w.svc.invalidateSnapshot(ctx, rfSpace)
	w.repo.failList = rfSpace
	if w.svc.everyoneCanView(ctx, rfSpace, roomPublic) {
		t.Error("everyoneCanView said yes without a snapshot")
	}
	if ids, everyone := w.svc.ViewersOf(ctx, rfSpace, roomPublic); everyone || ids != nil {
		t.Errorf("ViewersOf = (%v, %v) without a snapshot", ids, everyone)
	}
}

// A deny on one role keeps the room off the space channel: a member holding that role is on
// that channel too. The room is still listed for everyone else.
func TestViewersOfHonoursARoleLevelDeny(t *testing.T) {
	w := newVisWorld()
	w.rooms.add(textRoom(roomPublic, "public"))
	w.deny(roomPublic, rfMember)
	w.settle(t)
	viewers, everyone := w.svc.ViewersOf(context.Background(), rfSpace, roomPublic)
	if everyone {
		t.Fatal("a room denied to one role went space-wide")
	}
	if slices.Contains(viewers, rfMemberID) || !slices.Contains(viewers, rfPlainID) {
		t.Errorf("viewers %v", viewers)
	}
}

// ---- applying one room --------------------------------------------------------------------

func TestApplyMirrorRoomNewRoom(t *testing.T) {
	ctx := context.Background()

	t.Run("public goes space-wide, with its global identity", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.settle(t)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(textRoom(roomPublic, "public")), false); err != nil {
			t.Fatal(err)
		}
		users, spaceWide := w.sentTo("SPACE_ROOM_CREATE", roomPublic)
		if !spaceWide || users != nil {
			t.Fatalf("users %v, spaceWide %v", users, spaceWide)
		}
		if w.log.of("SPACE_ROOM_CREATE")[0].Data["federation"] == nil {
			t.Error("no federation block")
		}
	})

	// The cached snapshot predates the room. Announcing from it would read the room as public.
	t.Run("private goes to its viewers alone", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.settle(t)
		mr := mirrored(textRoom(roomStaff, "staff"), denyView(rfEveryone), allowView(rfMember))
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mr, false); err != nil {
			t.Fatal(err)
		}
		users, spaceWide := w.sentTo("SPACE_ROOM_CREATE", roomStaff)
		if spaceWide || !reflect.DeepEqual(users, privateViewers) {
			t.Fatalf("CREATE to %v, spaceWide %v, want %v", users, spaceWide, privateViewers)
		}
		if lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomStaff); !reflect.DeepEqual(lost, privateOthers) {
			t.Errorf("DELETE to %v, want %v", lost, privateOthers)
		}
		e := w.log.of("SPACE_ROOM_CREATE")[0]
		if e.Data["federation"] == nil {
			t.Error("a client on a mirror cannot key an E2EE channel without its global identity")
		}
		if ov, _ := e.Data["permission_overrides"].([]any); len(ov) != 2 {
			t.Errorf("overrides %v", e.Data["permission_overrides"])
		}
	})

	t.Run("denied to one role stays off the space channel", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.settle(t)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(textRoom(roomStaff, "staff"), denyView(rfMember)), false); err != nil {
			t.Fatal(err)
		}
		users, spaceWide := w.sentTo("SPACE_ROOM_CREATE", roomStaff)
		if spaceWide || slices.Contains(users, rfMemberID) {
			t.Fatalf("CREATE to %v, spaceWide %v", users, spaceWide)
		}
	})

	t.Run("private thread reaches its members and Manage Threads holders", func(t *testing.T) {
		w := privateWorld(t).mirror()
		w.settle(t)
		const fresh = 1099
		w.rooms.members[fresh] = i64s(rfPlainID)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(threadRoom(fresh, roomPublic, true)), false); err != nil {
			t.Fatal(err)
		}
		users, spaceWide := w.sentTo("SPACE_ROOM_CREATE", fresh)
		if want := i64s(rfOwnerID, rfAdminID, rfPlainID); spaceWide || !reflect.DeepEqual(users, want) {
			t.Fatalf("CREATE to %v, spaceWide %v, want %v", users, spaceWide, want)
		}
	})

	t.Run("public thread of a private channel stays with the channel's viewers", func(t *testing.T) {
		w := privateWorld(t).mirror()
		w.settle(t)
		const fresh = 1099
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(threadRoom(fresh, roomStaff, false)), false); err != nil {
			t.Fatal(err)
		}
		users, spaceWide := w.sentTo("SPACE_ROOM_CREATE", fresh)
		if spaceWide || !reflect.DeepEqual(users, privateViewers) {
			t.Fatalf("CREATE to %v, spaceWide %v", users, spaceWide)
		}
	})

	t.Run("archived thread is announced to nobody", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.rooms.add(textRoom(roomPublic, "public"))
		w.settle(t)
		th := threadRoom(roomThread, roomPublic, false)
		th.ThreadArchived = boolp(true)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(th), false); err != nil {
			t.Fatal(err)
		}
		if n := len(w.log.of("SPACE_ROOM_CREATE")); n != 0 {
			t.Errorf("%d CREATE events", n)
		}
	})
}

// A resync re-applies every room every 30 minutes. A room that did not change must cost
// nothing and tell nobody anything: re-sending names is how private ones leaked.
func TestApplyMirrorRoomNothingChanged(t *testing.T) {
	w := privateWorld(t).mirror()
	w.settle(t)
	ctx := context.Background()
	touched := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	w.rooms.byID[roomSecret].ThreadLastActiveAt = &touched
	th := threadRoom(roomSecret, roomPublic, true)
	th.ThreadLastActiveAt = &touched
	th.ThreadArchived, th.ThreadLocked, th.ThreadInvitable = boolp(false), boolp(false), boolp(true)
	for _, mr := range []MirrorRoom{
		mirrored(textRoom(roomStaff, "staff"), denyView(rfEveryone), allowView(rfMember)),
		mirrored(child(textRoom(roomChild, "child"), roomSection, true)),
		mirrored(sectionRoom(roomSection), denyView(rfEveryone), allowView(rfMember)),
		mirrored(th),
	} {
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mr, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.log.events) != 0 {
		t.Errorf("events on a no-op: %+v", w.log.events)
	}
	if len(w.rooms.writes) != 0 || w.repo.overrideWrites != 0 {
		t.Errorf("writes on a no-op: rooms %v, %d overrides", w.rooms.writes, w.repo.overrideWrites)
	}
}

func TestApplyMirrorRoomMetadataOnly(t *testing.T) {
	ctx := context.Background()

	t.Run("a private room's new name goes to its viewers", func(t *testing.T) {
		w := privateWorld(t).mirror()
		mr := mirrored(textRoom(roomStaff, "renamed"), denyView(rfEveryone), allowView(rfMember))
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mr, false); err != nil {
			t.Fatal(err)
		}
		users, spaceWide := w.sentTo("SPACE_ROOM_UPDATE", roomStaff)
		if spaceWide || !reflect.DeepEqual(users, privateViewers) {
			t.Fatalf("UPDATE to %v, spaceWide %v", users, spaceWide)
		}
		e := w.log.of("SPACE_ROOM_UPDATE")[0]
		if e.Data["name"] != "renamed" || e.Data["space_id"] != id.Format(rfSpace) {
			t.Errorf("payload %v", e.Data)
		}
		if synced, ok := e.Data["permissions_synced"].(bool); !ok || synced {
			t.Errorf("permissions_synced %v", e.Data["permissions_synced"])
		}
		if created, _ := w.sentTo("SPACE_ROOM_CREATE", 0); created != nil {
			t.Errorf("a rename republished visibility: %v", created)
		}
	})

	t.Run("a public room's goes to the space", func(t *testing.T) {
		w := privateWorld(t).mirror()
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(textRoom(roomPublic, "renamed")), false); err != nil {
			t.Fatal(err)
		}
		if _, spaceWide := w.sentTo("SPACE_ROOM_UPDATE", roomPublic); !spaceWide {
			t.Error("not space-wide")
		}
	})
}

func TestApplyMirrorRoomVisibilityChange(t *testing.T) {
	ctx := context.Background()

	// The case a member noticed: the channel goes private while they are subscribed to it. The
	// SPACE_ROOM_DELETE on their own channel is what ends the subscription.
	t.Run("a channel going private is taken from those who lost it, with its threads", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.rooms.add(textRoom(roomPublic, "public"))
		w.rooms.add(threadRoom(roomThread, roomPublic, false))
		w.settle(t)
		mr := mirrored(textRoom(roomPublic, "public"), denyView(rfEveryone), allowView(rfMember))
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mr, false); err != nil {
			t.Fatal(err)
		}
		for _, room := range []int64{roomPublic, roomThread} {
			if lost, _ := w.sentTo("SPACE_ROOM_DELETE", room); !reflect.DeepEqual(lost, privateOthers) {
				t.Errorf("room %d: DELETE to %v, want %v", room, lost, privateOthers)
			}
			if kept, spaceWide := w.sentTo("SPACE_ROOM_CREATE", room); spaceWide || !reflect.DeepEqual(kept, privateViewers) {
				t.Errorf("room %d: CREATE to %v, spaceWide %v", room, kept, spaceWide)
			}
		}
		if e := w.log.of("SPACE_ROOM_CREATE")[0]; e.Data["federation"] == nil {
			t.Error("republish lost the room's global identity")
		}
		if _, spaceWide := w.sentTo("SPACE_ROOM_OVERRIDE_UPDATE", roomPublic); !spaceWide {
			t.Error("override events stay on the space channel, as on the origin")
		}
		if n := len(w.log.of("SPACE_ROOM_UPDATE")); n != 0 {
			t.Errorf("%d updates for a change of overrides alone", n)
		}
	})

	t.Run("a section reaches its synced channels and their threads, not the others", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.rooms.add(sectionRoom(roomSection))
		w.rooms.add(child(textRoom(roomChild, "child"), roomSection, true))
		w.rooms.add(child(textRoom(roomStaff, "own"), roomSection, false))
		w.rooms.add(threadRoom(roomThread, roomChild, false))
		w.settle(t)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(sectionRoom(roomSection), denyView(rfEveryone)), false); err != nil {
			t.Fatal(err)
		}
		for _, room := range []int64{roomSection, roomChild, roomThread} {
			if lost, _ := w.sentTo("SPACE_ROOM_DELETE", room); !slices.Contains(lost, rfPlainID) {
				t.Errorf("room %d kept: DELETE to %v", room, lost)
			}
		}
		if lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomStaff); lost != nil {
			t.Errorf("an unsynced channel was taken away from %v", lost)
		}
	})

	t.Run("a member let in hears of the room", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.rooms.add(textRoom(roomStaff, "staff"))
		w.deny(roomStaff, rfEveryone)
		w.settle(t)
		mr := mirrored(textRoom(roomStaff, "staff"), denyView(rfEveryone), allowView(rfMember))
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mr, false); err != nil {
			t.Fatal(err)
		}
		if got, _ := w.sentTo("SPACE_ROOM_CREATE", roomStaff); !slices.Contains(got, rfMemberID) {
			t.Errorf("CREATE to %v", got)
		}
	})
}

func TestApplyMirrorRoomSyncedFlag(t *testing.T) {
	ctx := context.Background()

	// Without this the wire fix heals storage and leaves connected clients listing the room.
	t.Run("the flag reaches clients before the override deletes", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.rooms.add(sectionRoom(roomSection))
		w.deny(roomSection, rfEveryone)
		w.rooms.add(child(textRoom(roomChild, "child"), roomSection, false))
		w.allow(roomChild, rfMember)
		w.settle(t)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(child(textRoom(roomChild, "child"), roomSection, true)), false); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(w.rooms.writes, "synced:"+id.Format(roomChild)) {
			t.Fatalf("writes %v", w.rooms.writes)
		}
		update, del := -1, -1
		for i, e := range w.log.events {
			if e.Type == "SPACE_ROOM_UPDATE" && update < 0 {
				update = i
				if synced, _ := e.Data["permissions_synced"].(bool); !synced {
					t.Errorf("update carries %v", e.Data["permissions_synced"])
				}
			}
			if e.Type == "SPACE_ROOM_OVERRIDE_DELETE" && del < 0 {
				del = i
			}
		}
		if update < 0 || del < 0 || update > del {
			t.Errorf("update at %d, override delete at %d: the room would flash visible", update, del)
		}
		if lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomChild); !slices.Contains(lost, rfPlainID) {
			t.Errorf("DELETE to %v", lost)
		}
	})

	t.Run("an older origin says nothing and clears nothing", func(t *testing.T) {
		w := newVisWorld().mirror()
		w.rooms.add(sectionRoom(roomSection))
		w.rooms.add(child(textRoom(roomChild, "child"), roomSection, true))
		w.settle(t)
		r := child(textRoom(roomChild, "child"), roomSection, true)
		r.PermissionsSynced = nil
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(r), false); err != nil {
			t.Fatal(err)
		}
		if got := w.rooms.byID[roomChild].PermissionsSynced; got == nil || !*got {
			t.Errorf("synced became %v", got)
		}
		if len(w.log.events) != 0 {
			t.Errorf("events %+v", w.log.events)
		}
	})

	t.Run("never set and false are the same thing", func(t *testing.T) {
		w := newVisWorld().mirror()
		r := textRoom(roomPublic, "public")
		r.PermissionsSynced = nil
		w.rooms.add(r)
		w.settle(t)
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(textRoom(roomPublic, "public")), false); err != nil {
			t.Fatal(err)
		}
		if len(w.rooms.writes) != 0 || len(w.log.events) != 0 {
			t.Errorf("writes %v, events %+v", w.rooms.writes, w.log.events)
		}
	})
}

func TestApplyMirrorRoomE2EERotation(t *testing.T) {
	ctx := context.Background()
	on := textRoom(roomPublic, "public")
	on.E2EEEnabled = boolp(true)

	w := newVisWorld().mirror()
	w.rooms.add(textRoom(roomPublic, "public"))
	w.settle(t)
	if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(on), false); err != nil {
		t.Fatal(err)
	}
	if _, spaceWide := w.sentTo("SESSION_ROTATE", roomPublic); !spaceWide {
		t.Error("turning E2EE on rotated nothing")
	}

	w.log.events = nil
	if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, mirrored(textRoom(roomPublic, "public")), false); err != nil {
		t.Fatal(err)
	}
	if n := len(w.log.of("SESSION_ROTATE")); n != 0 {
		t.Errorf("turning E2EE off rotated (%d)", n)
	}
}

func TestApplyMirrorRoomDeleteUsesTheRoomsOwnDelete(t *testing.T) {
	w := privateWorld(t).mirror()
	ctx := context.Background()
	for _, room := range []int64{roomSecret, roomPublic} {
		if err := w.svc.ApplyMirrorRoom(ctx, rfSpace, MirrorRoom{Room: &rooms.Room{ID: room}}, true); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"delete-thread:" + id.Format(roomSecret), "delete-room:" + id.Format(roomPublic)}
	if !reflect.DeepEqual(w.rooms.writes, want) {
		t.Errorf("writes %v, want %v", w.rooms.writes, want)
	}
}

func TestApplyMirrorReorder(t *testing.T) {
	w := privateWorld(t).mirror()
	ctx := context.Background()
	section := int64(roomSection)
	order := []RoomOrder{
		{RoomID: roomPublic, Position: w.rooms.byID[roomPublic].Position}, // unchanged
		{RoomID: roomStaff, ParentID: &section, Position: 50},
	}
	if err := w.svc.ApplyMirrorReorder(ctx, rfSpace, order); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.sentTo("SPACE_ROOM_UPDATE", roomPublic); got != nil {
		t.Errorf("an unmoved room was announced to %v", got)
	}
	users, spaceWide := w.sentTo("SPACE_ROOM_UPDATE", roomStaff)
	if spaceWide || !reflect.DeepEqual(users, privateViewers) {
		t.Errorf("UPDATE to %v, spaceWide %v", users, spaceWide)
	}
	snap, err := w.svc.Snapshot(ctx, rfSpace)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Meta[roomStaff].ParentID != roomSection {
		t.Errorf("the snapshot kept the old parent: %+v", snap.Meta[roomStaff])
	}
}

// ---- reconciling a whole mirror ------------------------------------------------------------

// spec is the origin's description of the fixture's space as it stands, with these rooms.
func (w *visWorld) spec(t *testing.T, list ...MirrorRoom) *MirrorSpec {
	t.Helper()
	ctx := context.Background()
	roles, err := w.repo.ListSpaceRoles(ctx, rfSpace)
	if err != nil {
		t.Fatal(err)
	}
	members, err := w.repo.ListMembers(ctx, rfSpace)
	if err != nil {
		t.Fatal(err)
	}
	spec := &MirrorSpec{
		Space: &Space{ID: rfSpace, OwnerID: rfOwnerID, EveryoneRoleID: rfEveryone, Name: "Roles"},
		Roles: roles, Rooms: list, MembersComplete: true,
	}
	for i := range members {
		spec.Members = append(spec.Members, mirrorOf(&members[i]))
	}
	return spec
}

// privateRooms is what the origin says of privateWorld's rooms, as they are stored.
func privateRooms() []MirrorRoom {
	secret := threadRoom(roomSecret, roomPublic, true)
	return []MirrorRoom{
		mirrored(sectionRoom(roomSection), denyView(rfEveryone), allowView(rfMember)),
		mirrored(child(textRoom(roomChild, "child"), roomSection, true)),
		mirrored(textRoom(roomPublic, "public")),
		mirrored(textRoom(roomStaff, "staff"), denyView(rfEveryone), allowView(rfMember)),
		mirrored(threadRoom(roomThread, roomStaff, false)),
		mirrored(secret),
	}
}

func roomEvents(w *visWorld) []sentEvent {
	var out []sentEvent
	for _, e := range w.log.events {
		switch e.Type {
		case "SPACE_ROOM_CREATE", "SPACE_ROOM_UPDATE", "SPACE_ROOM_DELETE", "SPACE_ROOM_OVERRIDE_UPDATE", "SPACE_ROOM_OVERRIDE_DELETE", "SPACE_ROOM_USER_OVERRIDE_UPDATE", "SPACE_ROOM_USER_OVERRIDE_DELETE":
			out = append(out, e)
		}
	}
	return out
}

func TestReconcileMirrorNothingChanged(t *testing.T) {
	w := privateWorld(t).mirror()
	if _, err := w.svc.ReconcileMirror(context.Background(), w.spec(t, privateRooms()...)); err != nil {
		t.Fatal(err)
	}
	if ev := roomEvents(w); len(ev) != 0 {
		t.Errorf("room events on a no-op pass: %+v", ev)
	}
	if len(w.rooms.writes) != 0 || w.repo.overrideWrites != 0 {
		t.Errorf("writes on a no-op pass: rooms %v, %d overrides", w.rooms.writes, w.repo.overrideWrites)
	}
}

// The first pass after an upgrade: the origin now says synced, and every member holding the
// wrong room hears about it, which is what ends their subscription to it.
func TestReconcileMirrorHealsTheSyncedFlag(t *testing.T) {
	w := privateWorld(t).mirror()
	w.rooms.byID[roomChild].PermissionsSynced = boolp(false)
	w.settle(t)
	if _, err := w.svc.ReconcileMirror(context.Background(), w.spec(t, privateRooms()...)); err != nil {
		t.Fatal(err)
	}
	if lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomChild); !reflect.DeepEqual(lost, privateOthers) {
		t.Errorf("DELETE to %v, want %v", lost, privateOthers)
	}
	if users, spaceWide := w.sentTo("SPACE_ROOM_UPDATE", roomChild); spaceWide || !reflect.DeepEqual(users, privateViewers) {
		t.Errorf("UPDATE to %v, spaceWide %v", users, spaceWide)
	}
	for _, room := range []int64{roomPublic, roomStaff, roomThread, roomSecret} {
		if got, _ := w.sentTo("SPACE_ROOM_DELETE", room); got != nil {
			t.Errorf("room %d, unchanged, sent DELETE to %v", room, got)
		}
	}
}

// Rooms arrive in the origin's order, children possibly first. Room by room, this channel
// would be judged before its private section exists here and go out to everyone. A gateway
// check caching the snapshot midway, before either room is there, must not change the answer.
func TestReconcileMirrorAnnouncesAfterEveryRoomIsWritten(t *testing.T) {
	w := newVisWorld().mirror()
	w.settle(t)
	spec := w.spec(t,
		mirrored(child(textRoom(roomChild, "child"), roomSection, true)),
		mirrored(sectionRoom(roomSection), denyView(rfEveryone), allowView(rfMember)),
	)
	ctx := context.Background()
	w.rooms.beforeCreate = func() {
		if _, err := w.svc.Snapshot(ctx, rfSpace); err != nil {
			t.Error(err)
		}
	}
	if _, err := w.svc.ReconcileMirror(ctx, spec); err != nil {
		t.Fatal(err)
	}
	for _, room := range []int64{roomChild, roomSection} {
		users, spaceWide := w.sentTo("SPACE_ROOM_CREATE", room)
		if spaceWide || !reflect.DeepEqual(users, privateViewers) {
			t.Errorf("room %d: CREATE to %v, spaceWide %v, want %v", room, users, spaceWide, privateViewers)
		}
	}
}

// One call, however many rooms changed: each member hears of each room once.
func TestReconcileMirrorRepublishesEachRoomOnce(t *testing.T) {
	w := newVisWorld().mirror()
	w.rooms.add(sectionRoom(roomSection))
	w.rooms.add(child(textRoom(roomChild, "child"), roomSection, true))
	w.settle(t)
	spec := w.spec(t,
		mirrored(sectionRoom(roomSection), denyView(rfEveryone)),
		mirrored(child(textRoom(roomChild, "renamed"), roomSection, true), denyView(rfEveryone)),
	)
	if _, err := w.svc.ReconcileMirror(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomChild)
	if len(lost) == 0 || len(lost) != len(slices.Compact(slices.Clone(lost))) {
		t.Errorf("DELETE to %v", lost)
	}
}

// A pass that fails halfway has still written what it wrote, and the next pass will find no
// difference to announce: the announcement cannot wait for a pass that succeeds.
func TestReconcileMirrorAnnouncesWhatItWroteBeforeFailing(t *testing.T) {
	w := privateWorld(t).mirror()
	w.rooms.failOn = roomThread
	spec := w.spec(t, mirrored(textRoom(roomPublic, "public"), denyView(rfEveryone)), mirrored(threadRoom(roomThread, roomStaff, false)))
	if _, err := w.svc.ReconcileMirror(context.Background(), spec); !errors.Is(err, errRoomRead) {
		t.Fatalf("err = %v", err)
	}
	if lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomPublic); !slices.Contains(lost, rfPlainID) {
		t.Errorf("DELETE to %v", lost)
	}
	snap, err := w.svc.Snapshot(context.Background(), rfSpace)
	if err != nil {
		t.Fatal(err)
	}
	if snap.openToEveryone(roomPublic) {
		t.Error("the snapshot was left stale")
	}
}

func TestReconcileMirrorRoleView(t *testing.T) {
	ctx := context.Background()

	t.Run("a role re-applied as it was republishes nothing", func(t *testing.T) {
		w := privateWorld(t).mirror()
		if err := w.svc.ApplyMirrorRole(ctx, rfSpace, &SpaceRole{ID: rfMember, Name: "renamed", Position: 1}, false); err != nil {
			t.Fatal(err)
		}
		if n := len(w.log.of("SPACE_ROOM_CREATE")); n != 0 {
			t.Errorf("%d CREATE events", n)
		}
	})

	t.Run("ApplyMirrorRole takes a room from a role that lost Administrator", func(t *testing.T) {
		w := privateWorld(t).mirror()
		if err := w.svc.ApplyMirrorRole(ctx, rfSpace, &SpaceRole{ID: rfAdmin, Name: "admin", Position: 3}, false); err != nil {
			t.Fatal(err)
		}
		if lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomStaff); !slices.Contains(lost, rfAdminID) {
			t.Errorf("DELETE to %v", lost)
		}
	})

	t.Run("a pass announces it once, after the rooms", func(t *testing.T) {
		w := privateWorld(t).mirror()
		spec := w.spec(t, privateRooms()...)
		for i := range spec.Roles {
			if spec.Roles[i].ID == rfAdmin {
				spec.Roles[i].Permissions = 0
			}
		}
		if _, err := w.svc.ReconcileMirror(ctx, spec); err != nil {
			t.Fatal(err)
		}
		lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomStaff)
		n := 0
		for _, uid := range lost {
			if uid == rfAdminID {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the Administrator lost the room %d times (%v)", n, lost)
		}
	})

	t.Run("deleting a role republishes", func(t *testing.T) {
		w := privateWorld(t).mirror()
		if err := w.svc.ApplyMirrorRole(ctx, rfSpace, &SpaceRole{ID: rfAdmin}, true); err != nil {
			t.Fatal(err)
		}
		if lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomStaff); !slices.Contains(lost, rfAdminID) {
			t.Errorf("DELETE to %v", lost)
		}
	})
}

// A role change tells each member only what changed for them. Telling everyone about every
// room sent a SPACE_ROOM_DELETE for each room a member never could see, and each one has the
// gateway re-check every subscription they hold.
func TestARoleChangeTellsOnlyWhoseViewChanged(t *testing.T) {
	w := privateWorld(t)
	perms := int64(permissions.PermAdministrator)
	if _, err := w.svc.UpdateSpaceRole(context.Background(), rfOwnerID, rfSpace, rfMod, &UpdateSpaceRoleInput{Permissions: &perms}); err != nil {
		t.Fatal(err)
	}
	if got := w.log.of("SPACE_ROOM_DELETE"); len(got) != 0 {
		t.Errorf("nobody lost a room, %d DELETEs sent", len(got))
	}
	creates := w.log.of("SPACE_ROOM_CREATE")
	if len(creates) == 0 {
		t.Fatal("nobody was told about the rooms the role opened")
	}
	for _, e := range creates {
		if e.SpaceID != "" || (e.UserID != id.Format(rfModID) && e.UserID != id.Format(rfMod2ID)) {
			t.Errorf("CREATE to %+v, want only the role's holders", e)
		}
	}
	if got, _ := w.sentTo("SPACE_ROOM_CREATE", roomPublic); got != nil {
		t.Errorf("a room they could already see: %v", got)
	}
}

// Working that out reads every member, so it runs after the request that changed the role.
func TestARoleChangeAnnouncesAfterTheRequest(t *testing.T) {
	w := privateWorld(t)
	var queued []func()
	w.svc.runLater = func(f func()) { queued = append(queued, f) }
	perms := int64(permissions.PermAdministrator)
	if _, err := w.svc.UpdateSpaceRole(context.Background(), rfOwnerID, rfSpace, rfMod, &UpdateSpaceRoleInput{Permissions: &perms}); err != nil {
		t.Fatal(err)
	}
	if n := len(w.log.of("SPACE_ROOM_CREATE")); n != 0 || len(queued) != 1 {
		t.Fatalf("%d CREATEs before returning, %d queued", n, len(queued))
	}
	queued[0]()
	if got, _ := w.sentTo("SPACE_ROOM_CREATE", roomStaff); !slices.Contains(got, rfModID) {
		t.Errorf("CREATE to %v", got)
	}
}

// Losing View of a room ends a call in it, as on Discord: the voice module hears who lost it.
func TestLosingViewIsReported(t *testing.T) {
	ctx := context.Background()
	lostBy := func(w *visWorld) map[int64][]int64 {
		got := map[int64][]int64{}
		w.svc.SetRoomAccessLostHook(func(_ context.Context, _, roomID int64, users []int64) {
			got[roomID] = append(got[roomID], users...)
		})
		return got
	}
	t.Run("a role taken away", func(t *testing.T) {
		w := privateWorld(t)
		if err := w.svc.SetMemberRoles(ctx, rfOwnerID, rfSpace, rfPlainID, []int64{rfMember}); err != nil {
			t.Fatal(err)
		}
		got := lostBy(w)
		if err := w.svc.SetMemberRoles(ctx, rfOwnerID, rfSpace, rfPlainID, nil); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(got[roomStaff], rfPlainID) {
			t.Errorf("lost %v", got)
		}
		if _, ok := got[roomPublic]; ok {
			t.Errorf("a room they can still see: %v", got)
		}
	})
	t.Run("a role's Administrator taken away", func(t *testing.T) {
		w := privateWorld(t)
		got := lostBy(w)
		if err := w.svc.ApplyMirrorRole(ctx, rfSpace, &SpaceRole{ID: rfAdmin, Name: "admin", Position: 3}, false); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(got[roomStaff], rfAdminID) {
			t.Errorf("lost %v", got)
		}
	})
}

// A room's caller saw it open, but a View deny landed before the announcement: the space-wide
// grant must not stand for members who can no longer view it.
func TestAnOpenRoomGrantIsWithdrawnIfTheRoomClosedMeanwhile(t *testing.T) {
	w := privateWorld(t)
	w.svc.publishOpenRoomCreate(context.Background(), rfSpace, roomStaff, map[string]interface{}{"id": id.Format(roomStaff)})
	if lost, _ := w.sentTo("SPACE_ROOM_DELETE", roomStaff); !slices.Contains(lost, rfPlainID) {
		t.Errorf("DELETE to %v, want the member who cannot view it", lost)
	}
}

func TestReconcileMirrorAdoptsTheOriginsJoinDate(t *testing.T) {
	w := privateWorld(t).mirror()
	spec := w.spec(t, privateRooms()...)
	then := time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := range spec.Members {
		if spec.Members[i].UserID == rfPlainID {
			spec.Members[i].JoinedAt = then
		}
	}
	if _, err := w.svc.ReconcileMirror(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := w.repo.members[rfSpace][rfPlainID].JoinedAt; !got.Equal(then) {
		t.Errorf("joined_at %v", got)
	}
	if _, ok := w.repo.joinDates[rfMemberID]; ok {
		t.Error("a member whose date already matched was rewritten")
	}
	if n := len(w.log.of("SPACE_MEMBER_UPDATE")); n != 0 {
		t.Errorf("%d member updates for a date nobody is told about", n)
	}
}

func TestReconcileMirrorDropsInvitesStoredHere(t *testing.T) {
	w := privateWorld(t).mirror()
	w.repo.invites = []SpaceInvite{{Code: "abc", SpaceID: rfSpace}}
	w.repo.spaces[rfSpace].WidgetInviteCode = "abc"
	if _, err := w.svc.ReconcileMirror(context.Background(), w.spec(t, privateRooms()...)); err != nil {
		t.Fatal(err)
	}
	if len(w.repo.invites) != 0 || w.repo.spaces[rfSpace].WidgetInviteCode != "" {
		t.Errorf("invites %v, widget code %q", w.repo.invites, w.repo.spaces[rfSpace].WidgetInviteCode)
	}
}

// ---- a member's roles ----------------------------------------------------------------------

// The gateway authorizes a subscription once, so a role taken away only ends the stream through
// the SPACE_ROOM_DELETE sent here, and one given only shows the room through the CREATE.
func TestRoleChangesMoveRoomsInAndOut(t *testing.T) {
	ctx := context.Background()
	changes := []struct {
		name         string
		give, remove func(w *visWorld) error
	}{
		{
			"SetMemberRoles",
			func(w *visWorld) error {
				return w.svc.SetMemberRoles(ctx, rfOwnerID, rfSpace, rfPlainID, []int64{rfMember})
			},
			func(w *visWorld) error { return w.svc.SetMemberRoles(ctx, rfOwnerID, rfSpace, rfPlainID, nil) },
		},
		{
			"AddMemberRole / RemoveMemberRole",
			func(w *visWorld) error { return w.svc.AddMemberRole(ctx, rfOwnerID, rfSpace, rfPlainID, rfMember) },
			func(w *visWorld) error {
				return w.svc.RemoveMemberRole(ctx, rfOwnerID, rfSpace, rfPlainID, rfMember)
			},
		},
		{
			"UpdateMirrorMember",
			func(w *visWorld) error {
				m := mirrorOf(w.repo.members[rfSpace][rfPlainID])
				m.RoleIDs = append(m.RoleIDs, rfMember)
				return w.svc.UpdateMirrorMember(ctx, rfSpace, m)
			},
			func(w *visWorld) error {
				m := mirrorOf(w.repo.members[rfSpace][rfPlainID])
				m.RoleIDs = []int64{rfEveryone}
				return w.svc.UpdateMirrorMember(ctx, rfSpace, m)
			},
		},
	}
	gained := i64s(roomSection, roomChild, roomStaff, roomThread)
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			w := privateWorld(t)
			if err := tc.give(w); err != nil {
				t.Fatal(err)
			}
			for _, room := range gained {
				if users, spaceWide := w.sentTo("SPACE_ROOM_CREATE", room); spaceWide || !reflect.DeepEqual(users, i64s(rfPlainID)) {
					t.Errorf("room %d: CREATE to %v, spaceWide %v", room, users, spaceWide)
				}
			}
			if got, _ := w.sentTo("SPACE_ROOM_CREATE", roomPublic); got != nil {
				t.Errorf("a room they could already see: %v", got)
			}
			if e := w.log.of("SPACE_ROOM_CREATE")[0]; e.Data["federation"] == nil || e.Data["permission_overrides"] == nil {
				t.Errorf("payload %v", e.Data)
			}
			w.log.events = nil
			if err := tc.remove(w); err != nil {
				t.Fatal(err)
			}
			for _, room := range gained {
				if users, _ := w.sentTo("SPACE_ROOM_DELETE", room); !reflect.DeepEqual(users, i64s(rfPlainID)) {
					t.Errorf("room %d: DELETE to %v", room, users)
				}
			}
			if got, _ := w.sentTo("SPACE_ROOM_DELETE", roomPublic); got != nil {
				t.Errorf("a room they can still see: %v", got)
			}
		})
	}
}

func TestUpdateMirrorMemberProfileAloneMovesNoRooms(t *testing.T) {
	w := privateWorld(t).mirror()
	m := mirrorOf(w.repo.members[rfSpace][rfPlainID])
	if err := w.svc.UpdateMirrorMember(context.Background(), rfSpace, m); err != nil {
		t.Fatal(err)
	}
	if ev := roomEvents(w); len(ev) != 0 {
		t.Errorf("events %+v", ev)
	}
}

func TestRoleEditsRepublishWhenTheyTouchView(t *testing.T) {
	ctx := context.Background()

	t.Run("giving a role Administrator", func(t *testing.T) {
		w := privateWorld(t)
		perms := int64(permissions.PermAdministrator)
		if _, err := w.svc.UpdateSpaceRole(ctx, rfOwnerID, rfSpace, rfMod, &UpdateSpaceRoleInput{Permissions: &perms}); err != nil {
			t.Fatal(err)
		}
		if got, _ := w.sentTo("SPACE_ROOM_CREATE", roomStaff); !slices.Contains(got, rfModID) {
			t.Errorf("CREATE to %v", got)
		}
	})

	t.Run("a Manage Threads holder sees private threads", func(t *testing.T) {
		w := privateWorld(t)
		perms := int64(rfModPerms | permissions.PermManageThreads)
		if _, err := w.svc.UpdateSpaceRole(ctx, rfOwnerID, rfSpace, rfMod, &UpdateSpaceRoleInput{Permissions: &perms}); err != nil {
			t.Fatal(err)
		}
		if got, _ := w.sentTo("SPACE_ROOM_CREATE", roomSecret); !slices.Contains(got, rfModID) {
			t.Errorf("CREATE to %v", got)
		}
	})

	t.Run("anything else does not", func(t *testing.T) {
		w := privateWorld(t)
		name := "moderators"
		if _, err := w.svc.UpdateSpaceRole(ctx, rfOwnerID, rfSpace, rfMod, &UpdateSpaceRoleInput{Name: &name}); err != nil {
			t.Fatal(err)
		}
		if ev := roomEvents(w); len(ev) != 0 {
			t.Errorf("events %+v", ev)
		}
	})

	t.Run("deleting a role", func(t *testing.T) {
		w := privateWorld(t)
		if err := w.svc.DeleteSpaceRole(ctx, rfOwnerID, rfSpace, rfAdmin); err != nil {
			t.Fatal(err)
		}
		if got, _ := w.sentTo("SPACE_ROOM_DELETE", roomStaff); !slices.Contains(got, rfAdminID) {
			t.Errorf("DELETE to %v", got)
		}
	})
}

// A thread's block in SPACE_ROOM_CREATE is taken as the whole thread: a republish that sends
// a bare one resets every viewer's joined flag and counts until they reload.
func TestRepublishKeepsThreadState(t *testing.T) {
	w := privateWorld(t)
	w.rooms.members[roomThread] = i64s(rfMemberID)
	w.svc.republishRoomVisibility(context.Background(), rfSpace, []int64{roomThread})
	for _, e := range w.log.of("SPACE_ROOM_CREATE") {
		block, _ := e.Data["thread"].(map[string]any)
		joined, _ := block["joined"].(bool)
		if want := e.UserID == id.Format(rfMemberID); joined != want {
			t.Errorf("user %s: joined %v", e.UserID, joined)
		}
		if block["message_count"] != float64(7) || block["member_count"] != float64(1) {
			t.Errorf("user %s: block %v", e.UserID, block)
		}
	}
	if n := len(w.log.of("SPACE_ROOM_CREATE")); n != len(privateViewers) {
		t.Errorf("%d events", n)
	}
}

func TestRepublishLeavesArchivedThreadsToBeFetched(t *testing.T) {
	w := privateWorld(t)
	w.rooms.byID[roomThread].ThreadArchived = boolp(true)
	w.svc.republishRoomVisibility(context.Background(), rfSpace, []int64{roomThread})
	if got, _ := w.sentTo("SPACE_ROOM_CREATE", roomThread); got != nil {
		t.Errorf("CREATE to %v", got)
	}
	if got, _ := w.sentTo("SPACE_ROOM_DELETE", roomThread); !reflect.DeepEqual(got, privateOthers) {
		t.Errorf("DELETE to %v: a subscription outlives the archive", got)
	}
}

// ---- the origin ----------------------------------------------------------------------------

func TestUpdateRoomKeepsAPrivateRoomsNameFromOthers(t *testing.T) {
	w := privateWorld(t)
	name := "renamed"
	if err := w.svc.UpdateRoom(context.Background(), rfOwnerID, rfSpace, roomStaff, &UpdateRoomInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	users, spaceWide := w.sentTo("SPACE_ROOM_UPDATE", roomStaff)
	if spaceWide || !reflect.DeepEqual(users, privateViewers) {
		t.Errorf("UPDATE to %v, spaceWide %v", users, spaceWide)
	}
}

func TestCreateRoomUnderARoleDenyStaysOffTheSpaceChannel(t *testing.T) {
	w := newVisWorld()
	w.rooms.add(sectionRoom(roomSection))
	w.deny(roomSection, rfMember)
	w.settle(t)
	section := int64(roomSection)
	room, err := w.svc.CreateRoom(context.Background(), rfOwnerID, rfSpace, &CreateRoomInput{Name: "new", Type: rooms.TypeSpaceText, ParentID: &section})
	if err != nil {
		t.Fatal(err)
	}
	users, spaceWide := w.sentTo("SPACE_ROOM_CREATE", room.ID)
	if spaceWide || slices.Contains(users, rfMemberID) || !slices.Contains(users, rfPlainID) {
		t.Errorf("CREATE to %v, spaceWide %v", users, spaceWide)
	}
}

func TestMoveSpaceChannelRelaysTheRoomBeforeTheReorder(t *testing.T) {
	w := privateWorld(t)
	ctx := context.Background()
	if err := w.svc.MoveSpaceChannel(ctx, rfOwnerID, rfSpace, roomChild, nil, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"upsert:" + id.Format(roomChild), "reorder"}; !reflect.DeepEqual(w.fed.roomRelays, want) {
		t.Errorf("relays %v, want %v", w.fed.roomRelays, want)
	}
	if got := w.fed.synced[roomChild]; got == nil || *got {
		t.Errorf("relayed synced %v", got)
	}
	snap, err := w.svc.Snapshot(ctx, rfSpace)
	if err != nil {
		t.Fatal(err)
	}
	if m := snap.Meta[roomChild]; m.Synced || m.ParentID != 0 {
		t.Errorf("stale snapshot meta %+v", m)
	}
	if len(snap.Overrides[roomChild].Roles) != 2 {
		t.Errorf("stale snapshot overrides %+v", snap.Overrides[roomChild])
	}
	if users, spaceWide := w.sentTo("SPACE_ROOM_UPDATE", roomChild); spaceWide || !reflect.DeepEqual(users, privateViewers) {
		t.Errorf("UPDATE to %v, spaceWide %v", users, spaceWide)
	}
}

func TestDeleteRoomRelaysWhatHappensToItsChildren(t *testing.T) {
	ctx := context.Background()

	t.Run("a section's channels, before the section", func(t *testing.T) {
		w := privateWorld(t)
		if err := w.svc.DeleteRoom(ctx, rfOwnerID, rfSpace, roomSection); err != nil {
			t.Fatal(err)
		}
		if want := []string{"upsert:" + id.Format(roomChild), "delete:" + id.Format(roomSection)}; !reflect.DeepEqual(w.fed.roomRelays, want) {
			t.Fatalf("relays %v, want %v", w.fed.roomRelays, want)
		}
		if len(w.repo.roleOv[roomChild]) != 2 {
			t.Errorf("overrides not copied down: %v", w.repo.roleOv[roomChild])
		}
		if users, spaceWide := w.sentTo("SPACE_ROOM_UPDATE", roomChild); spaceWide || !reflect.DeepEqual(users, privateViewers) {
			t.Errorf("UPDATE to %v, spaceWide %v", users, spaceWide)
		}
	})

	t.Run("a channel's threads, before the channel", func(t *testing.T) {
		w := privateWorld(t)
		if err := w.svc.DeleteRoom(ctx, rfOwnerID, rfSpace, roomStaff); err != nil {
			t.Fatal(err)
		}
		if want := []string{"delete:" + id.Format(roomThread), "delete:" + id.Format(roomStaff)}; !reflect.DeepEqual(w.fed.roomRelays, want) {
			t.Fatalf("relays %v, want %v", w.fed.roomRelays, want)
		}
	})
}

// ---- the rest of a mirror ------------------------------------------------------------------

func TestApplyMirrorSpaceKeepsItsEveryoneRole(t *testing.T) {
	w := privateWorld(t).mirror()
	if err := w.svc.ApplyMirrorSpace(context.Background(), &Space{ID: rfSpace, OwnerID: rfOwnerID}); err != nil {
		t.Fatal(err)
	}
	if got := w.repo.spaces[rfSpace].EveryoneRoleID; got != rfEveryone {
		t.Errorf("everyone_role_id %d", got)
	}
}

func TestBootstrapEveryoneRoleRefusesAMirror(t *testing.T) {
	ctx := context.Background()
	w := privateWorld(t).mirror()
	w.repo.spaces[rfSpace].EveryoneRoleID = 0
	w.svc.invalidateSnapshot(ctx, rfSpace)
	if _, err := w.svc.Snapshot(ctx, rfSpace); !errors.Is(err, ErrRemoteSpace) {
		t.Fatalf("err = %v", err)
	}
	if n := len(w.repo.roleInserts); n != 0 {
		t.Errorf("%d roles minted", n)
	}

	local := privateWorld(t)
	local.repo.spaces[rfSpace].EveryoneRoleID = 0
	rid, err := local.svc.bootstrapEveryoneRole(ctx, rfSpace)
	if err != nil || rid == 0 {
		t.Errorf("a space hosted here: role %d, err %v", rid, err)
	}
}

func TestWidgetMintsNoInviteOnAMirror(t *testing.T) {
	ctx := context.Background()
	room := int64(roomPublic)

	w := privateWorld(t).mirror()
	w.repo.spaces[rfSpace].WidgetEnabled = true
	w.repo.spaces[rfSpace].WidgetRoomID = &room
	info, err := w.svc.Widget(ctx, rfSpace)
	if err != nil {
		t.Fatal(err)
	}
	if info.InviteCode != "" || len(w.repo.invites) != 0 {
		t.Errorf("code %q, invites %v", info.InviteCode, w.repo.invites)
	}
	if info.RoomName != "public" {
		t.Errorf("room name %q", info.RoomName)
	}
}

func TestInviteStoredForAMirrorLeadsNowhere(t *testing.T) {
	ctx := context.Background()
	w := privateWorld(t).mirror()
	w.repo.invites = []SpaceInvite{{Code: "abc", SpaceID: rfSpace, InviterID: rfOwnerID}}
	if _, err := w.svc.JoinByInvite(ctx, 42, "abc"); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("join: %v", err)
	}
	if _, err := w.svc.GetInvitePreview(ctx, "abc"); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("preview: %v", err)
	}
}

func TestPutMirrorMemberStoresTheOriginsJoinDate(t *testing.T) {
	ctx := context.Background()
	w := newVisWorld()
	then := time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := w.svc.putMirrorMember(ctx, rfSpace, MirrorMember{UserID: 77, RoleIDs: i64s(rfEveryone), JoinedAt: then}); err != nil {
		t.Fatal(err)
	}
	if got := w.repo.joinDates[77]; !got.Equal(then) {
		t.Errorf("joined_at %v", got)
	}
	if err := w.svc.putMirrorMember(ctx, rfSpace, MirrorMember{UserID: 78, RoleIDs: i64s(rfEveryone)}); err != nil {
		t.Fatal(err)
	}
	if w.repo.joinDates[78].IsZero() {
		t.Error("a member with no date from the origin got none")
	}
}

func TestBirthdayGreeting(t *testing.T) {
	ctx := context.Background()
	birthdayWorld := func(t *testing.T) (*visWorld, *auth.User, *[]string) {
		t.Helper()
		w := privateWorld(t)
		room := int64(roomPublic)
		w.repo.spaces[rfSpace].BirthdayChannelID = &room
		var posted []string
		w.svc.SetSystemMessenger(func(_ context.Context, _ int64, _ []int64, eventType, _ string) (int64, error) {
			posted = append(posted, eventType)
			return 1, nil
		})
		u, _ := w.users.GetByID(ctx, rfPlainID)
		return w, u, &posted
	}

	t.Run("hosted here, it is posted here", func(t *testing.T) {
		w, u, posted := birthdayWorld(t)
		w.svc.announceBirthday(ctx, rfSpace, u, "2026-11-01")
		if len(*posted) != 1 || (*posted)[0] != SystemBirthday {
			t.Errorf("posted %v", *posted)
		}
	})

	// A greeting written into the mirror's copy would reach only this instance, and the next
	// history page (served by the origin) would not have it.
	t.Run("on a mirror the origin is asked, once", func(t *testing.T) {
		w, u, posted := birthdayWorld(t)
		w.mirror()
		w.svc.announceBirthday(ctx, rfSpace, u, "2026-11-01")
		w.svc.announceBirthday(ctx, rfSpace, u, "2026-11-01")
		if len(*posted) != 0 {
			t.Errorf("posted locally: %v", *posted)
		}
		if !reflect.DeepEqual(w.fed.greetings, i64s(rfPlainID)) {
			t.Errorf("greetings %v", w.fed.greetings)
		}
	})

	t.Run("the origin greets a member once a day", func(t *testing.T) {
		w, _, posted := birthdayWorld(t)
		for range 2 {
			if err := w.svc.GreetBirthday(ctx, rfSpace, rfPlainID); err != nil {
				t.Fatal(err)
			}
		}
		if len(*posted) != 1 {
			t.Errorf("posted %v", *posted)
		}
		if err := w.svc.GreetBirthday(ctx, rfSpace, 999); !errors.Is(err, ErrNotMember) {
			t.Errorf("a stranger: %v", err)
		}
	})
}
