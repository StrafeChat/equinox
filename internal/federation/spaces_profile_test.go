package federation

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// Space profiles across instances: the member.profile op (mirror → origin), and the member
// wire that carries profiles back out (origin → mirrors).

func strp(s string) *string { return &s }

// profileTestUsers is the slice of auth.UserRepository these paths read.
type profileTestUsers struct {
	auth.UserRepository
	byID map[int64]*auth.User
}

func (u *profileTestUsers) GetByID(_ context.Context, id int64) (*auth.User, error) {
	return u.byID[id], nil
}

func (u *profileTestUsers) GetByRemote(_ context.Context, domain string, remoteID int64) (*auth.User, error) {
	for _, x := range u.byID {
		if x.IsRemote() && x.HomeDomain == domain && *x.RemoteID == remoteID {
			return x, nil
		}
	}
	return nil, nil
}

func shadowUser(localID, remoteID int64, domain string) *auth.User {
	return &auth.User{ID: localID, Username: "shadow", HomeDomain: domain, RemoteID: &remoteID}
}

func TestManageMemberProfileJSON(t *testing.T) {
	in := &spaces.MemberProfilePatch{Nick: strp(""), Bio: strp("about me"), Avatar: strp("https://b.example/cdn/v1/a.png")}
	raw, err := json.Marshal(memberProfileParams("@40:b.example", in))
	if err != nil {
		t.Fatal(err)
	}
	// nil stays out (unchanged); "" goes as "" (clear) - the difference is the whole point.
	if got, want := string(raw), `{"user":"@40:b.example","nick":"","avatar":"https://b.example/cdn/v1/a.png","bio":"about me"}`; got != want {
		t.Errorf("wire %s, want %s", got, want)
	}
	var back manageMemberProfile
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	out, err := back.patch()
	if err != nil {
		t.Fatal(err)
	}
	if out.Nick == nil || *out.Nick != "" {
		t.Errorf("nick %v, want a clear", out.Nick)
	}
	if out.Bio == nil || *out.Bio != "about me" || out.Avatar == nil || *out.Avatar != *in.Avatar {
		t.Errorf("bio %v avatar %v", out.Bio, out.Avatar)
	}
	if out.Banner != nil || out.AboutMe != nil || out.Pronouns != nil {
		t.Errorf("absent fields arrived set: %+v", out)
	}
}

func TestManageMemberProfileImages(t *testing.T) {
	long := "https://b.example/" + strings.Repeat("x", spaces.MaxMemberImageURLRunes)
	cases := []struct {
		name string
		v    *string
		ok   bool
	}{
		{"absent", nil, true},
		{"cleared", strp(""), true},
		{"cleared with blanks", strp("  "), true},
		{"https", strp("https://b.example/v1/spaces/members/1/2/avatar-0123456789abcdef.png"), true},
		{"http", strp("http://b.local:4100/v1/x.png"), true},
		{"javascript", strp("javascript:alert(1)"), false},
		{"data", strp("data:image/png;base64,AAAA"), false},
		{"ftp", strp("ftp://b.example/x.png"), false},
		{"no scheme", strp("b.example/x.png"), false},
		{"too long", strp(long), false},
	}
	for _, tc := range cases {
		for _, kind := range []string{"avatar", "banner"} {
			p := manageMemberProfile{User: "@40:b.example"}
			if kind == "avatar" {
				p.Avatar = tc.v
			} else {
				p.Banner = tc.v
			}
			_, err := p.patch()
			if tc.ok && err != nil {
				t.Errorf("%s %s: %v", kind, tc.name, err)
			}
			if !tc.ok && !errors.Is(err, errBadParams) {
				t.Errorf("%s %s: got %v, want errBadParams", kind, tc.name, err)
			}
		}
	}
}

func TestMirrorMemberProfileClamps(t *testing.T) {
	w := SpaceMemberWire{
		Nickname: "  a\nb‮c" + strings.Repeat("n", 40) + "  ",
		Avatar:   "javascript:alert(1)",
		Banner:   "https://a.example/v1/spaces/members/1/2/banner-0123456789abcdef.png",
		AboutMe:  strings.Repeat("a", 200),
		Bio:      strings.Repeat("é", 200), // runes, not bytes
		Pronouns: strings.Repeat("p", 50),
	}
	var m spaces.MirrorMember
	mirrorMemberProfile(&m, w)
	if want := "abc" + strings.Repeat("n", 29); m.Nickname != want {
		t.Errorf("nickname %q, want %q (controls stripped, then 32 runes)", m.Nickname, want)
	}
	if m.Avatar != "" {
		t.Errorf("non-http avatar kept: %q", m.Avatar)
	}
	if m.Banner != w.Banner {
		t.Errorf("https banner dropped: %q", m.Banner)
	}
	if n := len([]rune(m.AboutMe)); n != spaces.MaxMemberAboutMeRunes {
		t.Errorf("about_me %d runes", n)
	}
	if n := len([]rune(m.Bio)); n != spaces.MaxMemberBioRunes {
		t.Errorf("bio %d runes", n)
	}
	if n := len([]rune(m.Pronouns)); n != spaces.MaxMemberPronounsRunes {
		t.Errorf("pronouns %d runes", n)
	}

	var long spaces.MirrorMember
	mirrorMemberProfile(&long, SpaceMemberWire{Avatar: "https://a.example/" + strings.Repeat("x", 300)})
	if long.Avatar != "" {
		t.Errorf("an image URL past the local limit was kept (%d runes)", len(long.Avatar))
	}

	// An old origin sends no profile fields: the member has none.
	var none spaces.MirrorMember
	mirrorMemberProfile(&none, SpaceMemberWire{})
	if !reflect.DeepEqual(none, spaces.MirrorMember{}) {
		t.Errorf("empty wire gave %+v", none)
	}
}

// The origin's member wire carries the space profile, and a mirror reads it back to the
// same member - through JSON, as it travels.
func TestMemberWireRoundTrip(t *testing.T) {
	ctx := context.Background()
	origin := &Service{fcfg: config.FederationConfig{Domain: "a.example"}}
	// On the origin, a member from b.example is a shadow; on b.example they are local user 40.
	shadow := shadowUser(4, 40, "b.example")
	joined := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	row := &spaces.SpaceMember{
		SpaceID: 100, UserID: 4, RoleIDs: []int64{10, 12}, JoinedAt: joined,
		Nickname: "Turtle", Avatar: "https://b.example/v1/spaces/members/9/40/avatar-0123456789abcdef.png", Bio: "hi",
	}
	raw, err := json.Marshal(origin.memberWire(row, shadow))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"nickname", "avatar", "bio"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("wire lacks %q: %s", k, raw)
		}
	}
	// Empty fields stay off the wire; absent reads as "no override" on the other side.
	for _, k := range []string{"banner", "about_me", "pronouns"} {
		if _, ok := keys[k]; ok {
			t.Errorf("empty %q sent: %s", k, raw)
		}
	}

	var w SpaceMemberWire
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	local := &auth.User{ID: 40, Username: "turtle"}
	mirror := &Service{fcfg: config.FederationConfig{Domain: "b.example"}, users: &profileTestUsers{byID: map[int64]*auth.User{40: local}}}
	m, err := mirror.mirrorMember(ctx, "a.example", w)
	if err != nil {
		t.Fatal(err)
	}
	want := spaces.MirrorMember{UserID: 40, RoleIDs: []int64{10, 12}, JoinedAt: joined, Nickname: "Turtle", Avatar: row.Avatar, Bio: "hi"}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("mirror member %+v, want %+v", m, want)
	}
}

// ---- the origin side of member.profile -------------------------------------------------

// originSpaceRepo is the slice of spaces.Repository a space-profile write reads and writes,
// in memory. Anything else panics through the nil embedded interface.
type originSpaceRepo struct {
	spaces.Repository
	space   *spaces.Space
	roles   []spaces.SpaceRole
	members map[int64]*spaces.SpaceMember
	audits  []spaces.SpaceAuditEntry
	writes  int
}

func (r *originSpaceRepo) GetByID(_ context.Context, id int64) (*spaces.Space, error) {
	if r.space == nil || r.space.ID != id {
		return nil, nil
	}
	cp := *r.space
	return &cp, nil
}

func (r *originSpaceRepo) ListSpaceRoles(context.Context, int64) ([]spaces.SpaceRole, error) {
	return append([]spaces.SpaceRole(nil), r.roles...), nil
}

func (r *originSpaceRepo) IsMember(_ context.Context, _, userID int64) (bool, error) {
	return r.members[userID] != nil, nil
}

func (r *originSpaceRepo) MemberRoleIDs(_ context.Context, _, userID int64) ([]int64, bool, error) {
	m := r.members[userID]
	if m == nil {
		return nil, false, nil
	}
	return append([]int64(nil), m.RoleIDs...), true, nil
}

func (r *originSpaceRepo) GetMember(_ context.Context, _, userID int64) (*spaces.SpaceMember, error) {
	m := r.members[userID]
	if m == nil {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

func (r *originSpaceRepo) UpdateMemberProfile(_ context.Context, _, userID int64, prev *spaces.SpaceMember, p spaces.MemberProfilePatch) (bool, error) {
	m := r.members[userID]
	if m == nil || !m.JoinedAt.Equal(prev.JoinedAt) {
		return false, nil
	}
	next := p.Apply(m.Profile())
	m.Nickname, m.Avatar, m.Banner, m.AboutMe, m.Bio, m.Pronouns = next.Nick, next.Avatar, next.Banner, next.AboutMe, next.Bio, next.Pronouns
	r.writes++
	return true, nil
}

func (r *originSpaceRepo) InsertAuditEntry(_ context.Context, e *spaces.SpaceAuditEntry) error {
	r.audits = append(r.audits, *e)
	return nil
}

const (
	opSpace    = 100
	opOwner    = 1 // local
	opMod      = 3 // local, Manage Nicknames
	opMember   = 4 // b.example's user 40
	opOther    = 5 // b.example's user 50
	opEveryone = 10
	opModRole  = 12
)

// originFixture is a space hosted on a.example with two members from b.example.
func originFixture() (*Service, *originSpaceRepo, *profileTestUsers) {
	joined := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mk := func(uid int64, roles ...int64) *spaces.SpaceMember {
		return &spaces.SpaceMember{SpaceID: opSpace, UserID: uid, RoleIDs: append([]int64{opEveryone}, roles...), JoinedAt: joined}
	}
	repo := &originSpaceRepo{
		space: &spaces.Space{ID: opSpace, OwnerID: opOwner, EveryoneRoleID: opEveryone},
		roles: []spaces.SpaceRole{
			{SpaceID: opSpace, ID: opEveryone, Name: spaces.EveryoneRoleName, Position: 0, Permissions: permissions.DefaultEveryone},
			{SpaceID: opSpace, ID: opModRole, Name: "mod", Position: 2, Permissions: permissions.PermManageNicknames},
		},
		members: map[int64]*spaces.SpaceMember{
			opOwner: mk(opOwner), opMod: mk(opMod, opModRole), opMember: mk(opMember), opOther: mk(opOther),
		},
	}
	users := &profileTestUsers{byID: map[int64]*auth.User{
		opOwner:  {ID: opOwner, Username: "owner"},
		opMod:    {ID: opMod, Username: "mod"},
		opMember: shadowUser(opMember, 40, "b.example"),
		opOther:  shadowUser(opOther, 50, "b.example"),
	}}
	s := &Service{
		fcfg:     config.FederationConfig{Domain: "a.example"},
		users:    users,
		spaceSvc: spaces.NewService(repo, nil, users, nil, nil),
	}
	return s, repo, users
}

func manageProfile(t *testing.T, s *Service, actor *auth.User, p manageMemberProfile) error {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.manageAt(context.Background(), &spaces.Space{ID: opSpace}, actor, "member.profile", raw)
	if res != nil {
		t.Errorf("member.profile answered %v; the relay is the answer", res)
	}
	return err
}

func TestManageAtMemberProfile(t *testing.T) {
	avatar := "https://b.example/v1/spaces/members/9/40/avatar-0123456789abcdef.png"

	t.Run("a member from the asking instance edits their own profile", func(t *testing.T) {
		s, repo, users := originFixture()
		err := manageProfile(t, s, users.byID[opMember], manageMemberProfile{
			User: "@40:b.example", Nick: strp(" Turtle "), Avatar: strp(avatar), Pronouns: strp("they/them"),
		})
		if err != nil {
			t.Fatal(err)
		}
		m := repo.members[opMember]
		if m.Nickname != "Turtle" || m.Avatar != avatar || m.Pronouns != "they/them" {
			t.Errorf("row %+v", m.Profile())
		}
		if len(repo.audits) != 0 {
			t.Errorf("a self edit was audited: %+v", repo.audits)
		}
	})

	t.Run("a moderator renames and resets a remote member, audited", func(t *testing.T) {
		s, repo, users := originFixture()
		repo.members[opMember].Nickname, repo.members[opMember].Bio, repo.members[opMember].Avatar = "Old", "rude", avatar
		err := manageProfile(t, s, users.byID[opMod], manageMemberProfile{
			User: "@40:b.example", Nick: strp("Fixed"), Bio: strp(""), Avatar: strp(""),
		})
		if err != nil {
			t.Fatal(err)
		}
		m := repo.members[opMember]
		if m.Nickname != "Fixed" || m.Bio != "" || m.Avatar != "" {
			t.Errorf("row %+v", m.Profile())
		}
		actions := map[string]string{}
		for _, a := range repo.audits {
			actions[a.ActionType] = a.Changes
			if a.UserID != opMod || a.TargetID != "4" {
				t.Errorf("audit %+v", a)
			}
		}
		if actions[spaces.AuditMemberNickUpdate] != `{"nick":{"old":"Old","new":"Fixed"}}` {
			t.Errorf("nick audit %q", actions[spaces.AuditMemberNickUpdate])
		}
		if actions[spaces.AuditMemberProfileReset] != `{"fields":["avatar","bio"]}` {
			t.Errorf("reset audit %q", actions[spaces.AuditMemberProfileReset])
		}
	})

	t.Run("refusals", func(t *testing.T) {
		cases := []struct {
			name  string
			actor int64
			p     manageMemberProfile
			want  error
		}{
			{"a moderator setting someone's text", opMod, manageMemberProfile{User: "@40:b.example", Pronouns: strp("x")}, spaces.ErrNotOwnProfile},
			{"a moderator setting someone's image", opMod, manageMemberProfile{User: "@40:b.example", Avatar: strp(avatar)}, spaces.ErrNotOwnProfile},
			{"renaming another member without the bit", opOther, manageMemberProfile{User: "@40:b.example", Nick: strp("x")}, spaces.ErrMissingPerm},
			{"renaming the owner", opMod, manageMemberProfile{User: "@1:a.example", Nick: strp("x")}, spaces.ErrCannotChangeOwnerNickname},
			{"resetting the owner's profile", opMod, manageMemberProfile{User: "@1:a.example", Bio: strp("")}, spaces.ErrCannotResetOwnerProfile},
			{"renaming the moderator above you", opMember, manageMemberProfile{User: "@3:a.example", Nick: strp("x")}, spaces.ErrMissingPerm},
			{"a nickname on two lines", opMember, manageMemberProfile{User: "@40:b.example", Nick: strp("a\nb")}, spaces.ErrInvalidNickname},
			{"a script for an avatar", opMember, manageMemberProfile{User: "@40:b.example", Avatar: strp("javascript:alert(1)")}, errBadParams},
			{"nothing to change", opMember, manageMemberProfile{User: "@40:b.example"}, spaces.ErrNothingToPatch},
			{"a user nobody here knows", opMod, manageMemberProfile{User: "@999:b.example", Nick: strp("x")}, ErrRemoteUserNotFound},
			{"not a federated id", opMod, manageMemberProfile{User: "nobody", Nick: strp("x")}, ErrInvalidFID},
		}
		for _, tc := range cases {
			s, repo, users := originFixture()
			if err := manageProfile(t, s, users.byID[tc.actor], tc.p); !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
			}
			if repo.writes != 0 || len(repo.audits) != 0 {
				t.Errorf("%s: a refused op wrote (%d writes, %d audits)", tc.name, repo.writes, len(repo.audits))
			}
		}
	})

	t.Run("no params", func(t *testing.T) {
		s, _, users := originFixture()
		if _, err := s.manageAt(context.Background(), &spaces.Space{ID: opSpace}, users.byID[opMember], "member.profile", nil); !errors.Is(err, errBadParams) {
			t.Errorf("got %v", err)
		}
	})
}

// ---- the mirror side -------------------------------------------------------------------

// What RemoteUpdateMemberProfile settles before it asks the origin anything. The Service has
// no client or repository: reaching the network would panic.
func TestRemoteUpdateMemberProfileRefusesLocally(t *testing.T) {
	ctx := context.Background()
	s := &Service{fcfg: config.FederationConfig{Domain: "b.example"}, users: &profileTestUsers{byID: map[int64]*auth.User{}}}
	mod := &auth.User{ID: 3}

	if err := s.RemoteUpdateMemberProfile(ctx, "a.example", 9, mod, 3, &spaces.MemberProfilePatch{}); !errors.Is(err, spaces.ErrNothingToPatch) {
		t.Errorf("empty patch: %v", err)
	}
	// Giving another member's profile a value is never allowed, so it is refused here with the
	// sentinel the REST handler answers with its code.
	for _, p := range []spaces.MemberProfilePatch{
		{Bio: strp("my words now")}, {Avatar: strp("https://b.example/v1/x.png")}, {Nick: strp("ok"), Pronouns: strp("x")},
	} {
		p := p
		if err := s.RemoteUpdateMemberProfile(ctx, "a.example", 9, mod, 4, &p); !errors.Is(err, spaces.ErrNotOwnProfile) {
			t.Errorf("%+v: %v", p, err)
		}
	}
	// A target this instance has never heard of is the target's problem, not the caller's.
	if err := s.RemoteUpdateMemberProfile(ctx, "a.example", 9, mod, 4, &spaces.MemberProfilePatch{Nick: strp("x")}); !errors.Is(err, spaces.ErrMemberNotFound) {
		t.Errorf("unknown target: %v", err)
	}
}

// ---- joining a space this instance already mirrors -------------------------------------

// joinTestRepo is the slice of the federation Repository applyJoinSnapshot reads and writes
// for a space already mirrored here.
type joinTestRepo struct {
	Repository
	mapping *SpaceMapping
}

func (r *joinTestRepo) GetSpaceByOrigin(_ context.Context, domain string, originSpaceID int64) (*SpaceMapping, error) {
	if r.mapping != nil && r.mapping.OriginDomain == domain && r.mapping.OriginSpaceID == originSpaceID {
		cp := *r.mapping
		return &cp, nil
	}
	return nil, nil
}

func (r *joinTestRepo) DeleteSpacePeers(context.Context, int64) error     { return nil }
func (r *joinTestRepo) AddSpacePeer(context.Context, int64, string) error { return nil }

// joinMirrorRepo is originSpaceRepo plus the member writes a mirror makes, recorded.
type joinMirrorRepo struct {
	*originSpaceRepo
	adds       []spaces.MirrorMember
	roleWrites int
}

func (r *joinMirrorRepo) AddMember(_ context.Context, spaceID, userID int64, roleIDs []int64, p spaces.MemberProfile) error {
	r.adds = append(r.adds, spaces.MirrorMember{UserID: userID, RoleIDs: roleIDs, Nickname: p.Nick, Avatar: p.Avatar, Banner: p.Banner})
	r.members[userID] = &spaces.SpaceMember{
		SpaceID: spaceID, UserID: userID, RoleIDs: append([]int64(nil), roleIDs...), JoinedAt: time.Now().UTC(),
		Nickname: p.Nick, Avatar: p.Avatar, Banner: p.Banner, AboutMe: p.AboutMe, Bio: p.Bio, Pronouns: p.Pronouns,
	}
	return nil
}

func (r *joinMirrorRepo) AddSpaceToUserSet(context.Context, int64, int64) error { return nil }

func (r *joinMirrorRepo) SetMemberRoleIDs(_ context.Context, _, userID int64, roleIDs []int64) error {
	r.roleWrites++
	r.members[userID].RoleIDs = append([]int64(nil), roleIDs...)
	return nil
}

const (
	jnLocalSpace  = 9   // b.example's id for the mirror
	jnOriginSpace = 900 // a.example's id for the space
	jnUser        = 40  // local to b.example
	jnEveryone    = 10
	jnRole        = 12
)

// joinFixture is b.example mirroring a.example's space 900, with user 40 (local) in it
// when member is true: two roles, a nickname and a space avatar stored on b.example's CDN.
func joinFixture(member bool) (*Service, *joinMirrorRepo, *auth.User) {
	user := &auth.User{ID: jnUser, Username: "turtle"}
	repo := &joinMirrorRepo{originSpaceRepo: &originSpaceRepo{
		space:   &spaces.Space{ID: jnLocalSpace, OwnerID: 1, EveryoneRoleID: jnEveryone},
		members: map[int64]*spaces.SpaceMember{},
	}}
	if member {
		repo.members[jnUser] = &spaces.SpaceMember{
			SpaceID: jnLocalSpace, UserID: jnUser, RoleIDs: []int64{jnEveryone, jnRole},
			JoinedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			Nickname: "Keeps", Avatar: "https://b.example/v1/spaces/members/9/40/avatar-0123456789abcdef.png",
		}
	}
	s := &Service{
		fcfg:  config.FederationConfig{Domain: "b.example"},
		users: &profileTestUsers{byID: map[int64]*auth.User{jnUser: user}},
		repo:  &joinTestRepo{mapping: &SpaceMapping{SpaceID: jnLocalSpace, OriginDomain: "a.example", OriginSpaceID: jnOriginSpace}},
		// The spaces service knows no users: AddMirrorMember then skips the read-state work
		// (which needs rooms) and the gateway events (which need Redis).
		spaceSvc: spaces.NewService(repo, nil, &profileTestUsers{byID: map[int64]*auth.User{}}, nil, nil),
	}
	return s, repo, user
}

// joinSnapshot is the origin's reply to a join: the first page of members only.
func joinSnapshot(members ...SpaceMemberWire) *SpaceSnapshot {
	return &SpaceSnapshot{
		Space:   SpaceRef{OriginDomain: "a.example", OriginSpaceID: "900"},
		Info:    SpaceInfo{EveryoneRole: "10"},
		Members: members,
	}
}

func TestApplyJoinSnapshotToAnExistingMirror(t *testing.T) {
	ctx := context.Background()
	someoneElse := SpaceMemberWire{User: Profile{FID: "@7:a.example"}, Roles: []string{"10"}}

	t.Run("already a member, beyond the snapshot's first page: nothing is overwritten", func(t *testing.T) {
		s, repo, user := joinFixture(true)
		before := *repo.members[jnUser]
		sp, err := s.applyJoinSnapshot(ctx, "a.example", user, joinSnapshot(someoneElse))
		if err != nil {
			t.Fatal(err)
		}
		if sp == nil || sp.ID != jnLocalSpace {
			t.Fatalf("space %+v", sp)
		}
		if len(repo.adds) != 0 || repo.writes != 0 || repo.roleWrites != 0 {
			t.Errorf("the stand-in was written (adds %+v, %d profile, %d role writes)", repo.adds, repo.writes, repo.roleWrites)
		}
		if got := repo.members[jnUser]; !reflect.DeepEqual(*got, before) {
			t.Errorf("row %+v, want it untouched %+v", *got, before)
		}
	})

	t.Run("already a member, in the snapshot: their row is applied as an update", func(t *testing.T) {
		s, repo, user := joinFixture(true)
		avatar := repo.members[jnUser].Avatar
		me := SpaceMemberWire{User: Profile{FID: "@40:b.example"}, Roles: []string{"10", "12"}, Nickname: "Fresh", Avatar: avatar}
		if _, err := s.applyJoinSnapshot(ctx, "a.example", user, joinSnapshot(someoneElse, me)); err != nil {
			t.Fatal(err)
		}
		if len(repo.adds) != 0 || repo.roleWrites != 0 {
			t.Errorf("rewritten whole (adds %+v, %d role writes)", repo.adds, repo.roleWrites)
		}
		if got := repo.members[jnUser]; got.Nickname != "Fresh" || got.Avatar != avatar || !reflect.DeepEqual(got.RoleIDs, []int64{jnEveryone, jnRole}) {
			t.Errorf("row %+v", got)
		}
	})

	t.Run("not a member yet: added, with the stand-in when the snapshot lacks them", func(t *testing.T) {
		s, repo, user := joinFixture(false)
		if _, err := s.applyJoinSnapshot(ctx, "a.example", user, joinSnapshot(someoneElse)); err != nil {
			t.Fatal(err)
		}
		if len(repo.adds) != 1 || repo.adds[0].UserID != jnUser || !reflect.DeepEqual(repo.adds[0].RoleIDs, []int64{jnEveryone}) {
			t.Fatalf("adds %+v", repo.adds)
		}
	})
}
