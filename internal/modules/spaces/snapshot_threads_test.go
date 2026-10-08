package spaces

import (
	"reflect"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

// A thread has no overrides of its own: it resolves to its channel's, and when that channel
// is synced to a section, to the section's - Discord's rule. A thread's own row in the
// overrides map (there should never be one) must not be read.
func TestEffectiveRoomOverrides_ThreadInheritsChannelThenSection(t *testing.T) {
	const (
		section = int64(10)
		channel = int64(20)
		thread  = int64(30)
		roleA   = int64(2)
	)
	sectionOv := RoomOverrides{Roles: []SpaceRoomRoleOverride{{RoleID: roleA, Deny: permissions.PermSendMessagesInThreads}}}
	channelOv := RoomOverrides{Roles: []SpaceRoomRoleOverride{{RoleID: roleA, Allow: permissions.PermManageThreads}}}
	strayOv := RoomOverrides{Roles: []SpaceRoomRoleOverride{{RoleID: roleA, Deny: permissions.PermViewRoom}}}

	snap := &Snapshot{
		Overrides: map[int64]RoomOverrides{section: sectionOv, channel: channelOv, thread: strayOv},
		Meta: map[int64]RoomMeta{
			channel: {ParentID: section},
			thread:  {ParentID: channel, Thread: true},
		},
	}
	if got := snap.EffectiveRoomOverrides(thread); !reflect.DeepEqual(got, channelOv) {
		t.Fatalf("thread under an unsynced channel: got %+v, want the channel's %+v", got, channelOv)
	}

	snap.Meta[channel] = RoomMeta{ParentID: section, Synced: true}
	if got := snap.EffectiveRoomOverrides(thread); !reflect.DeepEqual(got, sectionOv) {
		t.Fatalf("thread under a synced channel: got %+v, want the section's %+v", got, sectionOv)
	}
}

// The thread bits are part of AllRoom (so a room override can carry them) and the public
// pair is granted by default, the private/manage pair is not.
func TestThreadPermissionBits(t *testing.T) {
	for _, bit := range []int64{permissions.PermCreatePublicThreads, permissions.PermCreatePrivateThreads, permissions.PermSendMessagesInThreads, permissions.PermManageThreads} {
		if !permissions.Has(permissions.AllRoom, bit) {
			t.Errorf("bit %d missing from AllRoom", bit)
		}
	}
	if !permissions.Has(permissions.DefaultEveryone, permissions.PermCreatePublicThreads) || !permissions.Has(permissions.DefaultEveryone, permissions.PermSendMessagesInThreads) {
		t.Error("DefaultEveryone should allow public threads and sending in threads")
	}
	if permissions.Has(permissions.DefaultEveryone, permissions.PermCreatePrivateThreads) || permissions.Has(permissions.DefaultEveryone, permissions.PermManageThreads) {
		t.Error("DefaultEveryone should not grant private or manage threads")
	}
}
