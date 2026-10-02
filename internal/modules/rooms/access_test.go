package rooms

import (
	"context"
	"errors"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

// fakeSpaceChecker lets canAccessLoaded be tested without a space service: it returns a
// per-room permission bitmask (and optionally an error) for the subject.
type fakeSpaceChecker struct {
	perms map[int64]int64
	err   error
}

func (f *fakeSpaceChecker) IsMember(context.Context, int64, int64) (bool, error) {
	// The vulnerability was that this alone granted access; the gate must no longer use it.
	return true, nil
}

func (f *fakeSpaceChecker) EffectiveChannelPermissions(_ context.Context, _, _, roomID int64) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.perms[roomID], nil
}

// A space member may reach a room only when they can actually view it. Being a member of
// the space (IsMember true) must NOT by itself grant access to a private channel - that was
// the realtime-subscribe leak.
func TestCanAccessLoaded_SpaceRoomRequiresViewPermission(t *testing.T) {
	spaceID := int64(10)
	viewable := &Room{ID: 1, SpaceID: &spaceID}
	private := &Room{ID: 2, SpaceID: &spaceID}

	s := &Service{spaceChecker: &fakeSpaceChecker{perms: map[int64]int64{
		1: permissions.PermViewRoom | permissions.PermSendMessages,
		2: permissions.PermSendMessages, // a member, but PermViewRoom is denied
	}}}
	ctx := context.Background()

	if !s.canAccessLoaded(ctx, 99, viewable, nil) {
		t.Error("member with PermViewRoom must be granted access to the room")
	}
	if s.canAccessLoaded(ctx, 99, private, nil) {
		t.Error("member WITHOUT PermViewRoom must be DENIED a private space room (the vulnerability)")
	}
}

// Permission resolution failing must deny, never fall back to granting.
func TestCanAccessLoaded_SpaceRoomFailsClosed(t *testing.T) {
	spaceID := int64(10)
	room := &Room{ID: 1, SpaceID: &spaceID}
	s := &Service{spaceChecker: &fakeSpaceChecker{err: errors.New("snapshot unavailable")}}
	if s.canAccessLoaded(context.Background(), 99, room, nil) {
		t.Error("access must fail closed when permission resolution errors")
	}
}

// With no space service wired, a space room is never accessible.
func TestCanAccessLoaded_SpaceRoomNoCheckerDenies(t *testing.T) {
	spaceID := int64(10)
	room := &Room{ID: 1, SpaceID: &spaceID}
	s := &Service{}
	if s.canAccessLoaded(context.Background(), 99, room, nil) {
		t.Error("a space room must be denied when no space checker is configured")
	}
}

// Direct messages and group DMs are unchanged: access is participation, independent of the
// space checker.
func TestCanAccessLoaded_DirectMessageByParticipation(t *testing.T) {
	s := &Service{spaceChecker: &fakeSpaceChecker{}}
	pm := &Room{ID: 3} // no SpaceID
	if !s.canAccessLoaded(context.Background(), 99, pm, []int64{7, 99, 42}) {
		t.Error("a participant must have access to their PM")
	}
	if s.canAccessLoaded(context.Background(), 99, pm, []int64{7, 42}) {
		t.Error("a non-participant must not access a PM")
	}
}
