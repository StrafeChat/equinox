package spaces

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// Space profiles on a mirror: the origin's member state (roles and space profile) is applied
// whole and only what differs is written. Relayed state never releases an image (it is not
// ordered, so it may be older than the row); what this instance's own forwarded write replaced
// is released once the origin has accepted it (ForwardMemberProfile), and removal releases
// the rest.

// mirrorRepo is profileRepo plus the member writes a mirror makes.
type mirrorRepo struct {
	*profileRepo
	adds        []MirrorMember // AddMember calls, as what they wrote
	roleWrites  int
	removed     []int64
	userSetAdds int
}

func (r *mirrorRepo) AddMember(_ context.Context, spaceID, userID int64, roleIDs []int64, p MemberProfile) error {
	m := &SpaceMember{SpaceID: spaceID, UserID: userID, RoleIDs: append([]int64(nil), roleIDs...), JoinedAt: time.Now().UTC()}
	setProfileOn(m, p)
	r.members[userID] = m
	r.adds = append(r.adds, MirrorMember{UserID: userID, RoleIDs: roleIDs, Nickname: p.Nick, Avatar: p.Avatar, Banner: p.Banner, AboutMe: p.AboutMe, Bio: p.Bio, Pronouns: p.Pronouns})
	return nil
}

func (r *mirrorRepo) AddSpaceToUserSet(context.Context, int64, int64) error {
	r.userSetAdds++
	return nil
}

func (r *mirrorRepo) SetMemberRoleIDs(_ context.Context, _, userID int64, roleIDs []int64) error {
	r.roleWrites++
	if m := r.members[userID]; m != nil {
		m.RoleIDs = append([]int64(nil), roleIDs...)
	}
	return nil
}

func (r *mirrorRepo) RemoveMember(_ context.Context, _, userID int64) error {
	delete(r.members, userID)
	r.removed = append(r.removed, userID)
	return nil
}

func mirrorFixture() *mirrorRepo { return &mirrorRepo{profileRepo: profileFixture()} }

func mirrorService(r *mirrorRepo) *Service {
	return NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, nil)
}

// mirrorOf is the MirrorMember the origin would send for the stored row as it is now.
func mirrorOf(m *SpaceMember) MirrorMember {
	p := m.Profile()
	return MirrorMember{
		UserID: m.UserID, RoleIDs: append([]int64(nil), m.RoleIDs...), JoinedAt: m.JoinedAt,
		Nickname: p.Nick, Avatar: p.Avatar, Banner: p.Banner, AboutMe: p.AboutMe, Bio: p.Bio, Pronouns: p.Pronouns,
	}
}

func TestMirrorMemberProfile(t *testing.T) {
	m := MirrorMember{Nickname: "n", Avatar: "a", Banner: "b", AboutMe: "am", Bio: "bi", Pronouns: "p"}
	if got, want := m.profile(), (MemberProfile{Nick: "n", Avatar: "a", Banner: "b", AboutMe: "am", Bio: "bi", Pronouns: "p"}); got != want {
		t.Errorf("profile() = %+v, want %+v", got, want)
	}
}

func TestMirrorMemberDiffers(t *testing.T) {
	base := SpaceMember{UserID: 4, RoleIDs: []int64{10, 11}, Nickname: "Turtle", Bio: "hi"}
	same := mirrorOf(&base)

	cases := []struct {
		name string
		edit func(m *MirrorMember)
		want bool
	}{
		{"neither", func(*MirrorMember) {}, false},
		{"roles in another order", func(m *MirrorMember) { m.RoleIDs = []int64{11, 10} }, false},
		{"roles only", func(m *MirrorMember) { m.RoleIDs = []int64{10} }, true},
		{"a duplicated role", func(m *MirrorMember) { m.RoleIDs = []int64{10, 11, 11} }, true},
		{"nickname only", func(m *MirrorMember) { m.Nickname = "Tortoise" }, true},
		{"nickname cleared", func(m *MirrorMember) { m.Nickname = "" }, true},
		{"avatar only", func(m *MirrorMember) { m.Avatar = "https://cdn.test/v1/x.png" }, true},
		{"banner only", func(m *MirrorMember) { m.Banner = "https://cdn.test/v1/y.png" }, true},
		{"about me only", func(m *MirrorMember) { m.AboutMe = "about" }, true},
		{"bio only", func(m *MirrorMember) { m.Bio = "" }, true},
		{"pronouns only", func(m *MirrorMember) { m.Pronouns = "they/them" }, true},
	}
	for _, tc := range cases {
		m := same
		m.RoleIDs = append([]int64(nil), same.RoleIDs...)
		tc.edit(&m)
		if got := mirrorMemberDiffers(base, m); got != tc.want {
			t.Errorf("%s: mirrorMemberDiffers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStripNicknameControls(t *testing.T) {
	cases := map[string]string{
		"Turtle":           "Turtle",
		"a\nb\tc\r":        "abc",
		"evil‮gnp.exe":     "evilgnp.exe",
		"⁦isolated⁩":       "isolated",
		"‪embed‬":          "embed",
		"👩‍👩‍👧 family":     "👩‍👩‍👧 family", // the zero-width joiners in an emoji stay
		"عربي":             "عربي",
		"null\x00byte\x7f": "nullbyte",
		"line\u0085next":   "linenext",
		"keeps‎joiner‍ok":  "keeps‎joiner‍ok",
		"top\u2028bottom":  "topbottom", // LINE SEPARATOR breaks the line like a newline
		"top\u2029bottom":  "topbottom", // and PARAGRAPH SEPARATOR
	}
	for in, want := range cases {
		if got := StripNicknameControls(in); got != want {
			t.Errorf("StripNicknameControls(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPutMirrorMemberWritesTheWholeRow(t *testing.T) {
	ctx := context.Background()
	r := mirrorFixture()
	// A row left over from a membership whose end the mirror never heard about.
	r.members[pfPlain].Nickname, r.members[pfPlain].Bio = "Stale", "stale bio"
	s := mirrorService(r)

	m := MirrorMember{UserID: pfPlain, RoleIDs: []int64{pfEveryone}, Nickname: "Fresh", Pronouns: "she/her"}
	if err := s.putMirrorMember(ctx, pfSpace, m); err != nil {
		t.Fatal(err)
	}
	if len(r.adds) != 1 || !reflect.DeepEqual(r.adds[0].profile(), m.profile()) {
		t.Fatalf("AddMember calls %+v, want one with %+v", r.adds, m.profile())
	}
	if r.writes != 0 {
		t.Errorf("a second profile write after the INSERT (%d)", r.writes)
	}
	if r.userSetAdds != 1 {
		t.Errorf("AddSpaceToUserSet calls %d", r.userSetAdds)
	}
	got := r.members[pfPlain]
	if got.Nickname != "Fresh" || got.Bio != "" || got.Pronouns != "she/her" {
		t.Errorf("row after put: %+v", got)
	}
}

func TestUpdateMirrorMember(t *testing.T) {
	ctx := context.Background()

	t.Run("profile change writes only what differs and releases nothing", func(t *testing.T) {
		cfg, deleted := profileNebulaStub(t)
		r := mirrorFixture()
		_, oldURL := profileBlob(pfPlain, "avatar-1111111111111111.png")
		_, newURL := profileBlob(pfPlain, "avatar-2222222222222222.png")
		row := r.members[pfPlain]
		row.Avatar, row.Banner, row.Nickname = oldURL, "https://cdn.elsewhere.org/v1/banner.png", "Old"
		s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)

		want := mirrorOf(row)
		want.Avatar, want.Banner, want.Nickname, want.Bio = newURL, "", "New", "bio"
		if err := s.UpdateMirrorMember(ctx, pfSpace, want); err != nil {
			t.Fatal(err)
		}
		got := r.members[pfPlain]
		if got.Profile() != want.profile() {
			t.Errorf("row %+v, want %+v", got.Profile(), want.profile())
		}
		if r.writes != 1 || r.roleWrites != 0 {
			t.Errorf("profile writes %d, role writes %d (roles did not change)", r.writes, r.roleWrites)
		}
		// Not even our own replaced avatar: relayed state may be older than the row.
		expectBlobDeletes(t, deleted)
	})

	t.Run("an older state arriving after a newer one deletes nothing", func(t *testing.T) {
		// The member uploaded b here; the captured reply set the row to b. A relay sent
		// before that (a role change carrying a0) arrives late through the outbox. The row
		// goes back to a0 until the next relay or resync, but b - which the origin and every
		// other mirror show - must survive.
		cfg, deleted := profileNebulaStub(t)
		r := mirrorFixture()
		_, a0URL := profileBlob(pfPlain, "avatar-aaaaaaaaaaaaaaaa.png")
		_, bURL := profileBlob(pfPlain, "avatar-bbbbbbbbbbbbbbbb.png")
		r.members[pfPlain].Avatar = bURL
		s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)
		stale := mirrorOf(r.members[pfPlain])
		stale.Avatar = a0URL
		stale.RoleIDs = []int64{pfEveryone, 12}
		if err := s.UpdateMirrorMember(ctx, pfSpace, stale); err != nil {
			t.Fatal(err)
		}
		expectBlobDeletes(t, deleted)
	})

	t.Run("roles only", func(t *testing.T) {
		r := mirrorFixture()
		r.members[pfPlain].Nickname = "Keeps"
		s := mirrorService(r)
		want := mirrorOf(r.members[pfPlain])
		want.RoleIDs = []int64{pfEveryone, 12}
		if err := s.UpdateMirrorMember(ctx, pfSpace, want); err != nil {
			t.Fatal(err)
		}
		if r.roleWrites != 1 || r.writes != 0 {
			t.Errorf("role writes %d, profile writes %d", r.roleWrites, r.writes)
		}
		if got := r.members[pfPlain]; !reflect.DeepEqual(got.RoleIDs, want.RoleIDs) || got.Nickname != "Keeps" {
			t.Errorf("row %+v", got)
		}
	})

	t.Run("nothing differs", func(t *testing.T) {
		r := mirrorFixture()
		r.members[pfPlain].Bio = "same"
		s := mirrorService(r)
		want := mirrorOf(r.members[pfPlain])
		want.RoleIDs = []int64{pfEveryone} // the same multiset
		if err := s.UpdateMirrorMember(ctx, pfSpace, want); err != nil {
			t.Fatal(err)
		}
		if r.roleWrites != 0 || r.writes != 0 || len(r.adds) != 0 {
			t.Errorf("an unchanged member was written (%d role, %d profile, %d adds)", r.roleWrites, r.writes, len(r.adds))
		}
	})

	t.Run("an update for someone not here is dropped, not turned into an add", func(t *testing.T) {
		// The member's own profile change at the origin, then a kick: the kick's remove
		// relay overtook the captured reply to the profile change. Re-adding them would undo
		// the kick; a member the mirror really lacks comes back with the next resync.
		r := mirrorFixture()
		s := mirrorService(r)
		m := MirrorMember{UserID: 77, RoleIDs: []int64{pfEveryone}, Nickname: "New Here", AboutMe: "hello"}
		if err := s.UpdateMirrorMember(ctx, pfSpace, m); err != nil {
			t.Fatal(err)
		}
		if _, ok := r.members[77]; ok || len(r.adds) != 0 || r.writes != 0 || r.roleWrites != 0 {
			t.Fatalf("a removed member came back (adds %+v, %d profile, %d role writes)", r.adds, r.writes, r.roleWrites)
		}
	})

	t.Run("lost compare-and-set retries from a fresh read", func(t *testing.T) {
		cfg, deleted := profileNebulaStub(t)
		r := mirrorFixture()
		_, oldURL := profileBlob(pfPlain, "avatar-1111111111111111.png")
		_, midURL := profileBlob(pfPlain, "avatar-2222222222222222.png")
		_, newURL := profileBlob(pfPlain, "avatar-3333333333333333.png")
		r.members[pfPlain].Avatar = oldURL
		// Another relay lands between the read and the write: it replaced old with mid.
		r.beforeWrite = func() { r.members[pfPlain].Avatar = midURL }
		s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)
		want := mirrorOf(r.members[pfPlain])
		want.Avatar = newURL
		if err := s.UpdateMirrorMember(ctx, pfSpace, want); err != nil {
			t.Fatal(err)
		}
		if got := r.members[pfPlain].Avatar; got != newURL || r.writes != 1 {
			t.Errorf("avatar %q, writes %d", got, r.writes)
		}
		// Neither relay releases what it overwrote.
		expectBlobDeletes(t, deleted)
	})

	t.Run("removed underneath: the stale update is dropped", func(t *testing.T) {
		r := mirrorFixture()
		r.beforeWrite = func() { delete(r.members, pfPlain) }
		s := mirrorService(r)
		want := mirrorOf(r.members[pfPlain])
		want.Nickname = "Ghost"
		if err := s.UpdateMirrorMember(ctx, pfSpace, want); err != nil {
			t.Fatal(err)
		}
		if _, ok := r.members[pfPlain]; ok || len(r.adds) != 0 {
			t.Errorf("the removed member came back (%d adds)", len(r.adds))
		}
	})

	t.Run("keeps losing", func(t *testing.T) {
		r := mirrorFixture()
		s := mirrorService(r)
		want := mirrorOf(r.members[pfPlain])
		want.Avatar = "https://cdn.test/v1/new.png"
		n := 0
		var bump func()
		bump = func() {
			n++
			r.members[pfPlain].Avatar = "https://cdn.test/v1/race" + string(rune('a'+n)) + ".png"
			r.beforeWrite = bump
		}
		r.beforeWrite = bump
		if err := s.UpdateMirrorMember(ctx, pfSpace, want); !errors.Is(err, errMirrorProfileRace) {
			t.Fatalf("got %v, want errMirrorProfileRace", err)
		}
		// Not an HTTPError: the relay is answered 500, which the origin's outbox retries.
		if _, _, ok := HTTPError(errMirrorProfileRace); ok {
			t.Error("errMirrorProfileRace maps to a client status; a 4xx would drop the relay")
		}
	})
}

func TestAddMirrorMemberOverwritesButReleasesNothing(t *testing.T) {
	ctx := context.Background()
	cfg, deleted := profileNebulaStub(t)
	r := mirrorFixture()
	_, bannerURL := profileBlob(pfPlain, "banner-0123456789abcdef.webp")
	_, avatarURL := profileBlob(pfPlain, "avatar-0123456789abcdef.png")
	r.members[pfPlain].Banner, r.members[pfPlain].Avatar = bannerURL, avatarURL
	s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)

	// An "add" for a row already here (the origin saw a rejoin the mirror missed the leave of,
	// or an older "add" arriving late): the row is the origin's state, but the banner it drops
	// is not deleted - a late "add" may be older than the row, and the banner still shown.
	m := MirrorMember{UserID: pfPlain, RoleIDs: []int64{pfEveryone}, Avatar: avatarURL}
	if err := s.AddMirrorMember(ctx, pfSpace, m); err != nil {
		t.Fatal(err)
	}
	if got := r.members[pfPlain]; got.Banner != "" || got.Avatar != avatarURL {
		t.Errorf("row %+v", got.Profile())
	}
	expectBlobDeletes(t, deleted)
}

func TestForwardMemberProfile(t *testing.T) {
	ctx := context.Background()
	foreign := "https://cdn.elsewhere.org/v1/spaces/members/100/5/banner-0123456789abcdef.png"

	t.Run("an accepted upload releases the image it replaced", func(t *testing.T) {
		cfg, deleted := profileNebulaStub(t)
		r := mirrorFixture()
		oldKey, oldURL := profileBlob(pfPlain, "avatar-1111111111111111.png")
		_, newURL := profileBlob(pfPlain, "avatar-2222222222222222.png")
		r.members[pfPlain].Avatar, r.members[pfPlain].Banner = oldURL, foreign
		s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)
		calls := 0
		err := s.ForwardMemberProfile(ctx, pfSpace, pfPlain, &MemberProfilePatch{Avatar: profileStr(newURL)}, func() error {
			calls++
			// The origin's relay, captured into the reply, lands while the op runs.
			r.members[pfPlain].Avatar = newURL
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("err %v, forward calls %d", err, calls)
		}
		expectBlobDeletes(t, deleted, oldKey)
	})

	t.Run("an accepted reset releases ours, never another instance's", func(t *testing.T) {
		cfg, deleted := profileNebulaStub(t)
		r := mirrorFixture()
		avatarKey, avatarURL := profileBlob(pfMember, "avatar-0123456789abcdef.gif")
		r.members[pfMember].Avatar, r.members[pfMember].Banner = avatarURL, foreign
		s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)
		in := &MemberProfilePatch{Avatar: profileStr(""), Banner: profileStr(""), Bio: profileStr("")}
		if err := s.ForwardMemberProfile(ctx, pfSpace, pfMember, in, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		expectBlobDeletes(t, deleted, avatarKey)
	})

	t.Run("a nickname change releases nothing", func(t *testing.T) {
		cfg, deleted := profileNebulaStub(t)
		r := mirrorFixture()
		_, avatarURL := profileBlob(pfPlain, "avatar-1111111111111111.png")
		r.members[pfPlain].Avatar = avatarURL
		s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)
		if err := s.ForwardMemberProfile(ctx, pfSpace, pfPlain, &MemberProfilePatch{Nick: profileStr("Turtle")}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		expectBlobDeletes(t, deleted)
	})

	t.Run("a failed forward releases nothing", func(t *testing.T) {
		// Refused, or the origin unreachable after it may already have written: either way
		// the old image may still be in use.
		cfg, deleted := profileNebulaStub(t)
		r := mirrorFixture()
		_, oldURL := profileBlob(pfPlain, "avatar-1111111111111111.png")
		_, newURL := profileBlob(pfPlain, "avatar-2222222222222222.png")
		r.members[pfPlain].Avatar = oldURL
		s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)
		err := s.ForwardMemberProfile(ctx, pfSpace, pfPlain, &MemberProfilePatch{Avatar: profileStr(newURL)}, func() error {
			return ErrOriginUnavailable
		})
		if !errors.Is(err, ErrOriginUnavailable) {
			t.Fatalf("got %v, want the forward's error", err)
		}
		expectBlobDeletes(t, deleted)
	})
}

func TestRemoveMirrorMember(t *testing.T) {
	ctx := context.Background()
	cfg, deleted := profileNebulaStub(t)
	r := mirrorFixture()
	avatarKey, avatarURL := profileBlob(pfPlain, "avatar-0123456789abcdef.gif")
	r.members[pfPlain].Avatar = avatarURL
	r.members[pfPlain].Banner = "https://cdn.elsewhere.org/v1/spaces/members/100/5/banner-0123456789abcdef.png"
	s := NewService(r, nil, &profileUsers{users: map[int64]*auth.User{}}, nil, cfg)

	if err := s.RemoveMirrorMember(ctx, pfSpace, pfPlain); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.members[pfPlain]; ok || !reflect.DeepEqual(r.removed, []int64{pfPlain}) {
		t.Errorf("removed %v", r.removed)
	}
	// The space profile goes with the row: our image is released, the foreign one is not ours.
	expectBlobDeletes(t, deleted, avatarKey)

	// Someone who is not a member is nothing to remove.
	if err := s.RemoveMirrorMember(ctx, pfSpace, 99); err != nil {
		t.Fatal(err)
	}
	if len(r.removed) != 1 {
		t.Errorf("removed %v", r.removed)
	}
}
