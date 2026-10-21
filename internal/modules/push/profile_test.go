package push

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// Space profiles in pushes: inside a space a member's nickname and space avatar stand in
// for their global name and avatar, in the author and in mention previews; a PM, a group
// PM and a call always show the global profile.

var _ Directory = (*memDir)(nil)

const (
	globalAvatar = "https://cdn.example/v1/avatars/1/a.webp"
	spaceAvatar  = "https://cdn.example/v1/spaces/members/100/1/avatar-0123456789abcdef.png"
)

// countingDir counts space-profile reads, and can make them fail.
type countingDir struct {
	*memDir
	err error

	mu    sync.Mutex
	reads map[[2]int64]int
}

func (d *countingDir) MemberProfile(ctx context.Context, spaceID, userID int64) (*MemberProfile, error) {
	d.mu.Lock()
	d.reads[[2]int64{spaceID, userID}]++
	d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	return d.memDir.MemberProfile(ctx, spaceID, userID)
}

func (d *countingDir) total() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.reads {
		n += c
	}
	return n
}

// withCountingDir swaps the world's service for one over a countingDir.
func withCountingDir(t *testing.T, w *world, err error) *countingDir {
	t.Helper()
	cd := &countingDir{memDir: w.dir, err: err, reads: map[[2]int64]int{}}
	w.svc = NewService(w.repo, cd, nil, testPushConfig(), testVAPID(t))
	trustServer(t, w.svc.sender, w.f.srv)
	return cd
}

func TestSpaceMessageAuthorUsesSpaceProfile(t *testing.T) {
	for _, room := range []struct {
		name string
		id   int64
		typ  int
	}{{"channel", channelID, roomTypeSpaceText}, {"thread", threadID, roomTypeThread}} {
		t.Run(room.name, func(t *testing.T) {
			w := newWorld(t)
			w.dir.profiles[[2]int64{spaceID, alice}] = &MemberProfile{Nick: "Turtle", Avatar: spaceAvatar}
			w.svc.process(context.Background(), msgJob(room.id, room.typ, "hi <@2>", false, []int64{bob}, []int64{bob}))
			got := w.received(t)[bob]
			if len(got) != 1 {
				t.Fatalf("bob got %d pushes", len(got))
			}
			if a := got[0].Author; a == nil || a.ID != "1" || a.Name != "Turtle" || a.Avatar != spaceAvatar {
				t.Errorf("author: %+v", a)
			}
		})
	}
}

// Each override applies on its own; whatever is unset (or not a web URL) stays global.
func TestSpaceMessageAuthorFallsBackPerField(t *testing.T) {
	cases := []struct {
		name               string
		profile            *MemberProfile
		wantName, wantAvat string
	}{
		{"nick only", &MemberProfile{Nick: "Turtle"}, "Turtle", globalAvatar},
		{"avatar only", &MemberProfile{Avatar: spaceAvatar}, "Alice", spaceAvatar},
		{"avatar not a web URL", &MemberProfile{Nick: "Turtle", Avatar: "javascript:alert(1)"}, "Turtle", globalAvatar},
		{"relative avatar", &MemberProfile{Avatar: "/v1/spaces/members/100/1/avatar-0123456789abcdef.png"}, "Alice", globalAvatar},
		{"empty profile", &MemberProfile{}, "Alice", globalAvatar},
		{"not a member", nil, "Alice", globalAvatar},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			if tc.profile != nil {
				w.dir.profiles[[2]int64{spaceID, alice}] = tc.profile
			}
			w.svc.process(context.Background(), msgJob(channelID, roomTypeSpaceText, "hi", false, []int64{bob}, []int64{bob}))
			got := w.received(t)[bob]
			if len(got) != 1 {
				t.Fatalf("bob got %d pushes", len(got))
			}
			if a := got[0].Author; a == nil || a.Name != tc.wantName || a.Avatar != tc.wantAvat {
				t.Errorf("author: %+v, want %q / %q", a, tc.wantName, tc.wantAvat)
			}
		})
	}
}

// A space avatar the author set for themselves without a global one still shows.
func TestSpaceAvatarWithoutGlobalAvatar(t *testing.T) {
	w := newWorld(t)
	w.dir.users[alice].Avatar = ""
	w.dir.profiles[[2]int64{spaceID, alice}] = &MemberProfile{Avatar: spaceAvatar}
	w.svc.process(context.Background(), msgJob(channelID, roomTypeSpaceText, "hi", false, []int64{bob}, []int64{bob}))
	if got := w.received(t)[bob]; len(got) != 1 || got[0].Author == nil || got[0].Author.Avatar != spaceAvatar {
		t.Fatalf("got %+v", got)
	}
}

// A space profile belongs to that space: a PM, a group PM's name and a call show the
// global profile, and a profile in another space never leaks into this one.
func TestSpaceProfileStaysInItsSpace(t *testing.T) {
	w := newWorld(t)
	w.dir.profiles[[2]int64{spaceID, alice}] = &MemberProfile{Nick: "Turtle", Avatar: spaceAvatar}
	w.dir.profiles[[2]int64{spaceID, carol}] = &MemberProfile{Nick: "Caz"}
	cd := withCountingDir(t, w, nil)

	w.svc.process(context.Background(), msgJob(pmID, roomTypePM, "", true, []int64{bob}, nil))
	w.svc.process(context.Background(), msgJob(groupID, roomTypeGroupPM, "hi <@3>", false, []int64{bob}, nil))
	w.svc.process(context.Background(), &job{T: TypeCall, Users: []int64{bob}, Room: groupID, RoomType: roomTypeGroupPM, TS: 5, Caller: alice})
	got := w.received(t)[bob]
	if len(got) != 3 {
		t.Fatalf("bob got %d pushes", len(got))
	}
	for _, p := range got {
		if p.Author == nil || p.Author.Name != "Alice" || p.Author.Avatar != globalAvatar {
			t.Errorf("%s author: %+v", p.T, p.Author)
		}
	}
	if gp := got[1]; gp.RoomName != "Alice, Carol, dave" || gp.Body != "hi @Carol" {
		t.Errorf("group PM name %q, preview %q", gp.RoomName, gp.Body)
	}
	if n := cd.total(); n != 0 {
		t.Errorf("%d space-profile reads for rooms outside any space", n)
	}

	// A nickname in another space does not name the author here.
	w = newWorld(t)
	w.dir.profiles[[2]int64{999, alice}] = &MemberProfile{Nick: "Elsewhere", Avatar: spaceAvatar}
	w.svc.process(context.Background(), msgJob(channelID, roomTypeSpaceText, "hi", false, []int64{bob}, []int64{bob}))
	if g := w.received(t)[bob]; len(g) != 1 || g[0].Author.Name != "Alice" || g[0].Author.Avatar != globalAvatar {
		t.Fatalf("got %+v", g)
	}
}

func TestSpaceMentionPreviewUsesNick(t *testing.T) {
	w := newWorld(t)
	w.dir.profiles[[2]int64{spaceID, bob}] = &MemberProfile{Nick: "Bobby"}
	w.dir.profiles[[2]int64{spaceID, carol}] = &MemberProfile{Avatar: spaceAvatar} // no nick
	w.dir.profiles[[2]int64{999, dave}] = &MemberProfile{Nick: "Elsewhere"}
	w.repo.update(carol, carol*10, func(s *Subscription) { s.PrefSpace = ModeAll })

	// The shared preview (rendered once) and a per-recipient one (it links a room).
	w.svc.process(context.Background(), msgJob(channelID, roomTypeSpaceText, "hi <@2>, <@3> and <@4>", false, []int64{bob}, []int64{bob}))
	w.svc.process(context.Background(), msgJob(channelID, roomTypeSpaceText, "<@2> see <#103>", false, []int64{carol}, nil))
	got := w.received(t)
	if b := got[bob]; len(b) != 1 || b[0].Body != "hi @Bobby, @Carol and @dave" {
		t.Errorf("bob's preview: %+v", b)
	}
	if c := got[carol]; len(c) != 1 || c[0].Body != "@Bobby see #secret-plans" {
		t.Errorf("carol's preview: %+v", c)
	}

	// The same mention in a plaintext group PM reads as the global name.
	w = newWorld(t)
	w.dir.profiles[[2]int64{spaceID, carol}] = &MemberProfile{Nick: "Caz"}
	w.svc.process(context.Background(), msgJob(groupID, roomTypeGroupPM, "hi <@3>", false, []int64{bob}, nil))
	if b := w.received(t)[bob]; len(b) != 1 || b[0].Body != "hi @Carol" {
		t.Errorf("group PM preview: %+v", b)
	}
}

// A mention of a member whose account could not be loaded still reads as their nickname.
func TestSpaceMentionNickWithoutUserRecord(t *testing.T) {
	w := newWorld(t)
	w.dir.profiles[[2]int64{spaceID, 77}] = &MemberProfile{Nick: "Ghostly"}
	w.svc.process(context.Background(), msgJob(channelID, roomTypeSpaceText, "ping <@77> <@78>", false, []int64{bob}, []int64{bob}))
	if b := w.received(t)[bob]; len(b) != 1 || b[0].Body != "ping @Ghostly" {
		t.Errorf("preview: %+v", b)
	}
}

// payloadsFor builds one job's message payloads for each recipient in turn - what
// deliverTo does, without its parallelism, so the job's caches can be counted exactly.
func payloadsFor(t *testing.T, svc *Service, j *job, recipients ...int64) []Payload {
	t.Helper()
	ctx := context.Background()
	jc := newJobCtx(svc, j)
	if !jc.loadRoom(ctx) {
		t.Fatal("room not found")
	}
	out := make([]Payload, 0, len(recipients))
	for _, uid := range recipients {
		b, err := jc.messagePayload(ctx, uid, true, jc.mentions(uid))
		if err != nil {
			t.Fatal(err)
		}
		var p Payload
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// One read per person per job, however many recipients and mentions; a failing read falls
// back to the global profile and is not retried for every recipient.
func TestSpaceProfileReadOncePerJob(t *testing.T) {
	w := newWorld(t)
	w.dir.profiles[[2]int64{spaceID, alice}] = &MemberProfile{Nick: "Turtle"}
	w.dir.profiles[[2]int64{spaceID, bob}] = &MemberProfile{Nick: "Bobby"}
	cd := withCountingDir(t, w, nil)
	// The room link makes the preview per recipient, so every recipient renders the mentions.
	j := msgJob(channelID, roomTypeSpaceText, "<@2> <@2> <@3> see <#101>", false, []int64{bob, carol, dave}, []int64{bob, carol})
	for _, p := range payloadsFor(t, w.svc, j, bob, carol, dave) {
		if p.Author == nil || p.Author.Name != "Turtle" || p.Body != "@Bobby @Bobby @Carol see #general" {
			t.Errorf("payload for %s: author %+v, preview %q", p.UID, p.Author, p.Body)
		}
	}
	cd.mu.Lock()
	for k, n := range cd.reads {
		if n != 1 {
			t.Errorf("space profile %v read %d times", k, n)
		}
	}
	if len(cd.reads) != 3 { // the author, Bob and Carol
		t.Errorf("profiles read: %v", cd.reads)
	}
	cd.mu.Unlock()

	w = newWorld(t)
	w.dir.profiles[[2]int64{spaceID, alice}] = &MemberProfile{Nick: "Turtle"}
	cd = withCountingDir(t, w, errors.New("space_members: undefined column"))
	j = msgJob(channelID, roomTypeSpaceText, "hi <@2>", false, []int64{bob, carol, dave}, nil)
	for _, p := range payloadsFor(t, w.svc, j, bob, carol, dave) {
		if p.Author == nil || p.Author.Name != "Alice" || p.Author.Avatar != globalAvatar || p.Body != "hi @bob" {
			t.Errorf("payload for %s with a failing profile read: author %+v, preview %q", p.UID, p.Author, p.Body)
		}
	}
	if n := cd.total(); n != 2 { // the author and Bob, once each
		t.Errorf("%d profile reads with the store failing, want 2", n)
	}
}
