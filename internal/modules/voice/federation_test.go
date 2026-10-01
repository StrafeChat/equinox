package voice

import (
	"context"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// fakeFed resolves "@<id>:<domain>" to a fixed local id and answers nothing else.
type fakeFed struct{ local map[string]int64 }

func (f *fakeFed) IsLocalServer(domain string) bool { return domain == "a.local" }
func (f *fakeFed) FIDOf(u *auth.User) string {
	if u.IsRemote() {
		return "@" + itoa(*u.RemoteID) + ":" + u.HomeDomain
	}
	return "@" + itoa(u.ID) + ":a.local"
}
func (f *fakeFed) ResolveLocalID(_ context.Context, fid string) (int64, error) {
	if id, ok := f.local[fid]; ok {
		return id, nil
	}
	return 0, ErrNotInVoice
}
func (f *fakeFed) JoinRemoteVoice(context.Context, string, *rooms.RoomWithParticipants, *auth.User, JoinInput) (*RemoteJoin, error) {
	return nil, ErrOriginUnavailable
}
func (f *fakeFed) LeaveRemoteVoice(context.Context, string, *rooms.RoomWithParticipants, *auth.User) {}
func (f *fakeFed) UpdateRemoteVoiceSelf(context.Context, string, *rooms.RoomWithParticipants, *auth.User, SelfInput) {
}
func (f *fakeFed) RingRemoteCall(context.Context, string, *rooms.RoomWithParticipants, *auth.User, []int64) {
}
func (f *fakeFed) DeclineRemoteCall(context.Context, string, *rooms.RoomWithParticipants, *auth.User) {}
func (f *fakeFed) AfterVoiceStateChanged(context.Context, *rooms.RoomWithParticipants, *State, bool) {}
func (f *fakeFed) AfterCallChanged(context.Context, *rooms.RoomWithParticipants, string, *Call, *auth.User, bool) {
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// A federated identity has dots inside the id (the domain), so the session is whatever
// follows the LAST dot, and the id resolves to the local row - a shadow for a remote
// user. The local form keeps parsing as before.
func TestUserOfIdentityHandlesBothForms(t *testing.T) {
	remote := int64(20)
	s := &Service{fed: &fakeFed{local: map[string]int64{"@20:b.local": 2, "@1:a.local": 1}}}
	ctx := context.Background()

	uid, session, ok := s.userOfIdentity(ctx, "@20:b.local.abc123")
	if !ok || uid != 2 || session != "abc123" {
		t.Fatalf("remote identity: uid=%d session=%q ok=%v", uid, session, ok)
	}
	uid, session, ok = s.userOfIdentity(ctx, "1.deadbeef")
	if !ok || uid != 1 || session != "deadbeef" {
		t.Fatalf("local identity: uid=%d session=%q ok=%v", uid, session, ok)
	}
	if _, _, ok := s.userOfIdentity(ctx, "@99:c.local.zzz"); ok {
		t.Fatal("an unknown federated id must not resolve")
	}
	if _, _, ok := (&Service{}).userOfIdentity(ctx, "@20:b.local.abc123"); ok {
		t.Fatal("without federation a federated identity must not resolve")
	}

	// identityFor matches what userOfIdentity parses, in both forms.
	fed := s.fed
	bob := &auth.User{ID: 2, HomeDomain: "b.local", RemoteID: &remote}
	federated := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 7}}
	federated.Federation = &rooms.Federation{OriginDomain: "a.local", OriginID: "7"}
	if got := s.identityFor(federated, bob, "abc123"); got != "@20:b.local.abc123" {
		t.Fatalf("identityFor(federated, remote user) = %q", got)
	}
	local := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 8}}
	if got := s.identityFor(local, &auth.User{ID: 1}, "x"); got != "1.x" {
		t.Fatalf("identityFor(local room) = %q", got)
	}
	_ = fed
}

// remoteOrigin / hosting: who runs the call.
func TestRemoteOriginAndHosting(t *testing.T) {
	s := &Service{fed: &fakeFed{}}
	mirror := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 1}}
	mirror.Federation = &rooms.Federation{OriginDomain: "b.local", OriginID: "9"}
	if s.remoteOrigin(mirror) != "b.local" || s.hosting(mirror) {
		t.Fatal("a room originated elsewhere is hosted there")
	}
	own := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 2}}
	own.Federation = &rooms.Federation{OriginDomain: "a.local", OriginID: "2"}
	if s.remoteOrigin(own) != "" || !s.hosting(own) {
		t.Fatal("our own federated room is hosted here")
	}
	plain := &rooms.RoomWithParticipants{Room: rooms.Room{ID: 3}}
	if s.remoteOrigin(plain) != "" || s.hosting(plain) {
		t.Fatal("a never-federated room is neither remote nor relayed")
	}
	if (&Service{}).remoteOrigin(mirror) != "" {
		t.Fatal("without federation every room is local")
	}
}
