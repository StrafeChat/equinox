package spaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

func profileStr(s string) *string { return &s }

func TestNicknamePermission(t *testing.T) {
	const owner, actor, target = 1, 2, 3
	change, manage, admin := permissions.PermChangeNickname, permissions.PermManageNicknames, permissions.PermAdministrator
	cases := []struct {
		name                        string
		actorID, targetID           int64
		base                        int64
		actorHighest, targetHighest int
		want                        error
	}{
		{"owner on self", owner, owner, permissions.AllSpace, -1, -1, nil},
		{"owner on another", owner, target, 0, -1, 5, nil},
		{"owner with no bits on self", owner, owner, 0, -1, -1, nil},
		{"admin on a lower member", actor, target, admin, 3, 1, nil},
		{"admin on the owner", actor, owner, admin, 3, -1, ErrCannotChangeOwnerNickname},
		{"admin at equal rank", actor, target, admin, 3, 3, ErrRoleHierarchy},
		{"self with Change Nickname", actor, actor, change, 1, 1, nil},
		{"self without it", actor, actor, 0, 1, 1, ErrCannotChangeNickname},
		{"self with only Manage Nicknames", actor, actor, manage, 1, 1, nil},
		{"self as admin", actor, actor, admin, -1, -1, nil},
		{"Manage Nicknames on a lower member", actor, target, manage, 2, 1, nil},
		{"Manage Nicknames at equal rank", actor, target, manage, 2, 2, ErrRoleHierarchy},
		{"Manage Nicknames on a higher member", actor, target, manage, 2, 3, ErrRoleHierarchy},
		{"Change Nickname is not enough for others", actor, target, change, 2, 1, ErrMissingPerm},
		{"no bit for others", actor, target, 0, 2, 1, ErrMissingPerm},
		{"no roles on either side", actor, target, manage, -1, -1, ErrRoleHierarchy},
		{"Manage Nicknames on the owner", actor, owner, manage, 9, -1, ErrCannotChangeOwnerNickname},
	}
	for _, tc := range cases {
		got := nicknamePermission(tc.actorID, tc.targetID, owner, tc.base, tc.actorHighest, tc.targetHighest)
		if !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeMemberProfilePatch(t *testing.T) {
	type tc struct {
		name string
		in   MemberProfilePatch
		want error
	}
	cases := []tc{
		{"empty nick is a clear", MemberProfilePatch{Nick: profileStr("")}, nil},
		{"32 emoji", MemberProfilePatch{Nick: profileStr(strings.Repeat("🐢", 32))}, nil},
		{"33 emoji", MemberProfilePatch{Nick: profileStr(strings.Repeat("🐢", 33))}, ErrInvalidNickname},
		{"32 Arabic letters", MemberProfilePatch{Nick: profileStr(strings.Repeat("ب", 32))}, nil},
		{"33 Arabic letters", MemberProfilePatch{Nick: profileStr(strings.Repeat("ب", 33))}, ErrInvalidNickname},
		{"newline", MemberProfilePatch{Nick: profileStr("a\nb")}, ErrInvalidNickname},
		{"tab", MemberProfilePatch{Nick: profileStr("a\tb")}, ErrInvalidNickname},
		{"line separator", MemberProfilePatch{Nick: profileStr("a b")}, ErrInvalidNickname},
		{"paragraph separator", MemberProfilePatch{Nick: profileStr("a b")}, ErrInvalidNickname},
		{"only a variation selector", MemberProfilePatch{Nick: profileStr("️")}, ErrInvalidNickname},
		{"only combining marks", MemberProfilePatch{Nick: profileStr("͏᠋឴")}, ErrInvalidNickname},
		{"only an enclosing mark", MemberProfilePatch{Nick: profileStr("⃣")}, ErrInvalidNickname},
		{"keycap emoji", MemberProfilePatch{Nick: profileStr("1️⃣")}, nil},
		{"heart with variation selector", MemberProfilePatch{Nick: profileStr("❤️")}, nil},
		{"Devanagari with vowel signs", MemberProfilePatch{Nick: profileStr("प्रिया")}, nil},
		{"decomposed accent", MemberProfilePatch{Nick: profileStr("Zoé")}, nil},
		{"bidi override", MemberProfilePatch{Nick: profileStr("abc\u202Edef")}, ErrInvalidNickname},
		{"bidi isolate", MemberProfilePatch{Nick: profileStr("\u2066abc")}, ErrInvalidNickname},
		{"only zero-width spaces", MemberProfilePatch{Nick: profileStr("\u200B\u200B")}, ErrInvalidNickname},
		{"only BOM and word joiner", MemberProfilePatch{Nick: profileStr("\uFEFF\u2060")}, ErrInvalidNickname},
		{"only a Hangul filler", MemberProfilePatch{Nick: profileStr("\u3164")}, ErrInvalidNickname},
		{"emoji joined by ZWJ", MemberProfilePatch{Nick: profileStr("👨\u200D👩\u200D👧")}, nil},
		{"RTL mark in an Arabic name", MemberProfilePatch{Nick: profileStr("\u200Fسلمى")}, nil},
		{"about_me 190", MemberProfilePatch{AboutMe: profileStr(strings.Repeat("x", 190))}, nil},
		{"about_me 191", MemberProfilePatch{AboutMe: profileStr(strings.Repeat("x", 191))}, ErrInvalidMemberAboutMe},
		{"bio 190", MemberProfilePatch{Bio: profileStr(strings.Repeat("é", 190))}, nil},
		{"bio 191", MemberProfilePatch{Bio: profileStr(strings.Repeat("é", 191))}, ErrInvalidMemberBio},
		{"pronouns 40", MemberProfilePatch{Pronouns: profileStr(strings.Repeat("y", 40))}, nil},
		{"pronouns 41", MemberProfilePatch{Pronouns: profileStr(strings.Repeat("y", 41))}, ErrInvalidMemberPronouns},
		{"image cleared", MemberProfilePatch{Avatar: profileStr(""), Banner: profileStr("")}, nil},
		{"https image", MemberProfilePatch{Avatar: profileStr("https://cdn.example/v1/a.png")}, nil},
		{"http image", MemberProfilePatch{Banner: profileStr("http://cdn.example/v1/a.png")}, nil},
		{"256-rune image", MemberProfilePatch{Avatar: profileStr("https://c.io/" + strings.Repeat("a", 256-13))}, nil},
		{"257-rune image", MemberProfilePatch{Avatar: profileStr("https://c.io/" + strings.Repeat("a", 257-13))}, ErrInvalidProfileImage},
		{"javascript: image", MemberProfilePatch{Avatar: profileStr("javascript:alert(1)")}, ErrInvalidProfileImage},
		{"ftp image", MemberProfilePatch{Avatar: profileStr("ftp://cdn.example/a.png")}, ErrInvalidProfileImage},
		{"no scheme", MemberProfilePatch{Banner: profileStr("cdn.example/a.png")}, ErrInvalidProfileImage},
		{"no host", MemberProfilePatch{Banner: profileStr("https:///a.png")}, ErrInvalidProfileImage},
	}
	for _, c := range cases {
		p := c.in
		if got := normalizeMemberProfilePatch(&p); !errors.Is(got, c.want) || (c.want == nil && got != nil) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	p := MemberProfilePatch{Nick: profileStr("  Turtle  "), Bio: profileStr("   "), Pronouns: profileStr("\tthey/them\n")}
	if err := normalizeMemberProfilePatch(&p); err != nil {
		t.Fatal(err)
	}
	if *p.Nick != "Turtle" || *p.Bio != "" || *p.Pronouns != "they/them" {
		t.Errorf("trim: nick %q bio %q pronouns %q", *p.Nick, *p.Bio, *p.Pronouns)
	}
	if p.Avatar != nil || p.AboutMe != nil {
		t.Error("normalize set a field the patch did not name")
	}
	ws := MemberProfilePatch{Nick: profileStr(" \t ")}
	if err := normalizeMemberProfilePatch(&ws); err != nil || *ws.Nick != "" {
		t.Errorf("whitespace-only nick: err %v, value %q (want a clear)", err, *ws.Nick)
	}
}

func TestParseMemberProfileBody(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    MemberProfilePatch
		wantErr string
	}{
		{"set and ignore unknown", `{"nick":"Turtle","foo":1}`, MemberProfilePatch{Nick: profileStr("Turtle")}, ""},
		{"null clears", `{"nick":null,"bio":null}`, MemberProfilePatch{Nick: profileStr(""), Bio: profileStr("")}, ""},
		{"empty string clears", `{"pronouns":""}`, MemberProfilePatch{Pronouns: profileStr("")}, ""},
		{"images cleared", `{"avatar":null,"banner":""}`, MemberProfilePatch{Avatar: profileStr(""), Banner: profileStr("")}, ""},
		{"whitespace image is a clear", `{"avatar":"  "}`, MemberProfilePatch{Avatar: profileStr("  ")}, ""},
		{"every key", `{"nick":"n","avatar":null,"banner":null,"about_me":"a","bio":"b","pronouns":"p"}`,
			MemberProfilePatch{Nick: profileStr("n"), Avatar: profileStr(""), Banner: profileStr(""), AboutMe: profileStr("a"), Bio: profileStr("b"), Pronouns: profileStr("p")}, ""},
		{"number", `{"nick":5}`, MemberProfilePatch{}, "nick must be a string or null"},
		{"object", `{"bio":{"a":1}}`, MemberProfilePatch{}, "bio must be a string or null"},
		{"bool", `{"about_me":true}`, MemberProfilePatch{}, "about_me must be a string or null"},
		{"avatar URL", `{"avatar":"https://cdn.example/v1/a.png"}`, MemberProfilePatch{}, ErrInvalidProfileImage.Error()},
		{"banner URL", `{"banner":"x"}`, MemberProfilePatch{}, ErrInvalidProfileImage.Error()},
		{"nothing known", `{"foo":"bar"}`, MemberProfilePatch{}, ErrNothingToPatch.Error()},
		{"empty object", `{}`, MemberProfilePatch{}, ErrNothingToPatch.Error()},
		{"null body", `null`, MemberProfilePatch{}, ErrNothingToPatch.Error()},
		{"invalid json", `{"nick":`, MemberProfilePatch{}, "invalid json"},
		{"array", `["nick"]`, MemberProfilePatch{}, "invalid json"},
	}
	for _, tc := range cases {
		got, err := parseMemberProfileBody([]byte(tc.body))
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("%s: err %v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("%s: got %s, want %s", tc.name, profilePatchString(*got), profilePatchString(tc.want))
		}
	}
}

func profilePatchString(p MemberProfilePatch) string {
	f := func(s *string) string {
		if s == nil {
			return "<nil>"
		}
		return "\"" + *s + "\""
	}
	return "{nick " + f(p.Nick) + " avatar " + f(p.Avatar) + " banner " + f(p.Banner) + " about_me " + f(p.AboutMe) + " bio " + f(p.Bio) + " pronouns " + f(p.Pronouns) + "}"
}

func TestMemberProfilePatchHelpers(t *testing.T) {
	if !(&MemberProfilePatch{}).Empty() {
		t.Error("zero patch not Empty")
	}
	if (&MemberProfilePatch{Bio: profileStr("")}).Empty() {
		t.Error("a clear is not empty")
	}
	if !(&MemberProfilePatch{Nick: profileStr("")}).OnlyNick() {
		t.Error("nick-only clear not OnlyNick")
	}
	if (&MemberProfilePatch{}).OnlyNick() || (&MemberProfilePatch{Nick: profileStr("a"), Avatar: profileStr("")}).OnlyNick() {
		t.Error("OnlyNick true without just the nick")
	}
	if (&MemberProfilePatch{Nick: profileStr("x"), Avatar: profileStr(""), Bio: profileStr("")}).setsNonNickValue() {
		t.Error("clears and a nick reported as setting a profile value")
	}
	if !(&MemberProfilePatch{Pronouns: profileStr("she/her")}).setsNonNickValue() {
		t.Error("a pronouns value not reported")
	}

	old := MemberProfile{Nick: "a", Avatar: "https://x/v1/1", Bio: "bio"}
	next := MemberProfilePatch{Nick: profileStr("b"), Avatar: profileStr(""), AboutMe: profileStr("hi")}.Apply(old)
	want := MemberProfile{Nick: "b", Avatar: "", AboutMe: "hi", Bio: "bio"}
	if next != want {
		t.Errorf("Apply: %+v, want %+v", next, want)
	}
	if (MemberProfilePatch{}).Apply(old) != old {
		t.Error("an empty patch changed the profile")
	}

	d := profileDiff(old, next)
	if d.Nick == nil || *d.Nick != "b" || d.Avatar == nil || *d.Avatar != "" || d.AboutMe == nil || *d.AboutMe != "hi" {
		t.Errorf("profileDiff missed a change: %s", profilePatchString(d))
	}
	if d.Banner != nil || d.Bio != nil || d.Pronouns != nil {
		t.Errorf("profileDiff named an unchanged field: %s", profilePatchString(d))
	}
	if same := profileDiff(old, old); !same.Empty() {
		t.Error("profileDiff of equal profiles is not empty")
	}

	got := resetFields(MemberProfile{Nick: "n", Avatar: "a", Banner: "b", Bio: "x", Pronouns: "p"},
		MemberProfile{Nick: "", Avatar: "", Banner: "b2", Bio: "", Pronouns: "p"})
	if !reflect.DeepEqual(got, []string{"avatar", "bio"}) {
		t.Errorf("resetFields = %v, want [avatar bio]", got)
	}
}

func TestMemberWireBuilders(t *testing.T) {
	m := &SpaceMember{
		SpaceID: 100, UserID: 4, RoleIDs: []int64{10, 11}, Nickname: "Turtle", Avatar: " ",
		Bio: "space bio", JoinedAt: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC),
	}
	keys := []string{"nick", "space_about_me", "space_avatar", "space_banner", "space_bio", "space_pronouns"}

	fields := memberProfileFields(m)
	if got := profileKeysOf(fields); !reflect.DeepEqual(got, keys) {
		t.Errorf("memberProfileFields keys %v, want %v", got, keys)
	}
	if fields["nick"] != "Turtle" || fields["space_bio"] != "space bio" {
		t.Errorf("values: %v", fields)
	}
	for _, k := range []string{"space_avatar", "space_banner", "space_about_me", "space_pronouns"} {
		if fields[k] != nil {
			t.Errorf("%s = %#v, want nil for an empty (or blank) value", k, fields[k])
		}
	}

	upd := memberUpdatePayload(m)
	if got, want := profileKeysOf(upd), sortedProfileKeys(append([]string{"role_ids", "user_id"}, keys...)); !reflect.DeepEqual(got, want) {
		t.Errorf("memberUpdatePayload keys %v, want %v", got, want)
	}
	if upd["user_id"] != "4" || !reflect.DeepEqual(upd["role_ids"], []string{"10", "11"}) {
		t.Errorf("memberUpdatePayload ids: %v", upd)
	}
	// No roles still sends role_ids, as [] - older clients read a missing key as "no roles".
	raw, _ := json.Marshal(memberUpdatePayload(&SpaceMember{UserID: 4}))
	if !strings.Contains(string(raw), `"role_ids":[]`) || !strings.Contains(string(raw), `"nick":null`) {
		t.Errorf("empty member payload %s", raw)
	}

	s := NewService(nil, nil, nil, nil, nil)
	u := &auth.User{ID: 4, Username: "turtle", DisplayName: "Global Name", Avatar: "https://cdn/v1/global.png", Bio: "global bio"}
	j := s.memberJSON(m, u, nil)
	for _, k := range append([]string{"roles", "joined_at", "id", "username", "display_name", "avatar", "bio"}, keys...) {
		if _, ok := j[k]; !ok {
			t.Errorf("memberJSON lacks %q", k)
		}
	}
	if j["avatar"] != u.Avatar || j["bio"] != u.Bio || j["display_name"] != u.DisplayName {
		t.Error("memberJSON's global keys must stay the global profile")
	}
	if j["nick"] != "Turtle" || j["space_bio"] != "space bio" || !reflect.DeepEqual(j["roles"], []string{"10", "11"}) {
		t.Errorf("memberJSON space values: %v", j)
	}
	if j["joined_at"] != m.JoinedAt {
		t.Errorf("joined_at = %v", j["joined_at"])
	}
}

func profileKeysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedProfileKeys(s []string) []string {
	sort.Strings(s)
	return s
}

func TestOwnsMemberBlobURL(t *testing.T) {
	bases := []string{"https://cdn.example.com/", "http://nebula:4300"}
	own := "spaces/members/100/4/avatar-0123456789abcdef.png"
	cases := []struct {
		name string
		url  string
		key  string
	}{
		{"public base", "https://cdn.example.com/v1/" + own, own},
		{"internal base", "http://nebula:4300/v1/" + own, own},
		{"banner jpeg", "https://cdn.example.com/v1/spaces/members/100/4/banner-fedcba9876543210.jpeg", "spaces/members/100/4/banner-fedcba9876543210.jpeg"},
		{"another user's", "https://cdn.example.com/v1/spaces/members/100/5/avatar-0123456789abcdef.png", ""},
		{"another space's", "https://cdn.example.com/v1/spaces/members/101/4/avatar-0123456789abcdef.png", ""},
		{"user id prefix of another", "https://cdn.example.com/v1/spaces/members/100/45/avatar-0123456789abcdef.png", ""},
		{"foreign CDN", "https://cdn.other.org/v1/" + own, ""},
		{"lookalike host", "https://cdn.example.com.evil.org/v1/" + own, ""},
		{"empty", "", ""},
		{"global avatar", "https://cdn.example.com/v1/avatars/4/0123456789abcdef.png", ""},
		{"traversal", "https://cdn.example.com/v1/spaces/members/100/4/../5/avatar-0123456789abcdef.png", ""},
		{"traversal in name", "https://cdn.example.com/v1/spaces/members/100/4/avatar-0123456789abcdef.png/../../x", ""},
		{"query string", "https://cdn.example.com/v1/" + own + "?x=1", ""},
		{"fragment", "https://cdn.example.com/v1/" + own + "#x", ""},
		{"encoded dots", "https://cdn.example.com/v1/spaces/members/100/4/%2e%2e/avatar-0123456789abcdef.png", ""},
		{"encoded slash", "https://cdn.example.com/v1/spaces/members/100/4/avatar-0123456789abcdef%2fpng", ""},
		{"wrong extension", "https://cdn.example.com/v1/spaces/members/100/4/avatar-0123456789abcdef.svg", ""},
		{"short random", "https://cdn.example.com/v1/spaces/members/100/4/avatar-0123.png", ""},
		{"uppercase hex", "https://cdn.example.com/v1/spaces/members/100/4/avatar-0123456789ABCDEF.png", ""},
		{"double slash", "https://cdn.example.com//v1/" + own, ""},
	}
	for _, tc := range cases {
		key, ok := ownsMemberBlobURL(bases, 100, 4, tc.url)
		if ok != (tc.key != "") || key != tc.key {
			t.Errorf("%s: got (%q, %v), want %q", tc.name, key, ok, tc.key)
		}
	}
	if _, ok := ownsMemberBlobURL([]string{"", "  "}, 100, 4, "/v1/"+own); ok {
		t.Error("an empty base matched")
	}
	if got := memberBlobPrefix(100, 4); got != "spaces/members/100/4/" {
		t.Errorf("memberBlobPrefix = %q", got)
	}
}

func TestLiveMembers(t *testing.T) {
	joined := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []SpaceMember{
		{UserID: 1, JoinedAt: joined},
		{UserID: 2, Nickname: "ghost"}, // profile cells outliving a removal: no joined_at
		{UserID: 3, JoinedAt: joined, RoleIDs: []int64{9}},
		{UserID: 4},
	}
	got := liveMembers(rows)
	if len(got) != 2 || got[0].UserID != 1 || got[1].UserID != 3 {
		t.Errorf("liveMembers kept %+v", got)
	}
	if liveMember(nil) || liveMember(&SpaceMember{UserID: 5, RoleIDs: []int64{1}}) || !liveMember(&SpaceMember{JoinedAt: joined}) {
		t.Error("liveMember")
	}
}

func TestHTTPErrorProfileSentinels(t *testing.T) {
	cases := []struct {
		err    error
		status int
		msg    string
	}{
		{ErrMemberNotFound, 404, "member not found"},
		{ErrInvalidNickname, 400, "nickname must be at most 32 characters, on one line"},
		{ErrInvalidMemberAboutMe, 400, "about_me must be at most 190 characters"},
		{ErrInvalidMemberBio, 400, "bio must be at most 190 characters"},
		{ErrInvalidMemberPronouns, 400, "pronouns must be at most 40 characters"},
		{ErrInvalidProfileImage, 400, "avatar and banner must be uploaded, or cleared with null"},
		{ErrCannotChangeNickname, 403, "missing permission to change your nickname in this space"},
		{ErrCannotChangeOwnerNickname, 403, "only the space owner can change the owner's nickname"},
		{ErrNotOwnProfile, 403, "only the member can change their own space avatar, banner, pronouns, about me or bio"},
		{ErrCannotResetOwnerProfile, 403, "only the space owner can change the owner's space profile"},
		{ErrMemberProfileChanged, 409, ErrMemberProfileChanged.Error()},
	}
	for _, tc := range cases {
		status, msg, ok := HTTPError(tc.err)
		if !ok || status != tc.status || msg != tc.msg {
			t.Errorf("%v: got (%d, %q, %v), want (%d, %q)", tc.err, status, msg, ok, tc.status, tc.msg)
		}
	}
}

// ---- the service, on an in-memory repository ---------------------------------------------

// profileRepo is the slice of Repository the space-profile and backfill paths touch, in memory.
// Any other method panics through the nil embedded interface - an unexpected read is a bug.
type profileRepo struct {
	Repository
	space   *Space
	roles   []SpaceRole
	members map[int64]*SpaceMember
	audits  []SpaceAuditEntry
	writes  int
	// beforeWrite runs once, right before the next conditional profile write is checked: the
	// moment another request could change the row underneath.
	beforeWrite func()
	claimed     map[string]bool
	permWrites  map[int64]int64
}

func (r *profileRepo) GetByID(_ context.Context, id int64) (*Space, error) {
	if r.space == nil || r.space.ID != id {
		return nil, nil
	}
	cp := *r.space
	return &cp, nil
}

func (r *profileRepo) ListSpaceRoles(context.Context, int64) ([]SpaceRole, error) {
	return append([]SpaceRole(nil), r.roles...), nil
}

func (r *profileRepo) IsMember(_ context.Context, _, userID int64) (bool, error) {
	return liveMember(r.members[userID]), nil
}

func (r *profileRepo) MemberRoleIDs(_ context.Context, _, userID int64) ([]int64, bool, error) {
	m := r.members[userID]
	if !liveMember(m) {
		return nil, false, nil
	}
	return append([]int64(nil), m.RoleIDs...), true, nil
}

func (r *profileRepo) GetMember(_ context.Context, _, userID int64) (*SpaceMember, error) {
	m := r.members[userID]
	if !liveMember(m) {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

func (r *profileRepo) UpdateMemberProfile(_ context.Context, _, userID int64, prev *SpaceMember, p MemberProfilePatch) (bool, error) {
	if f := r.beforeWrite; f != nil {
		r.beforeWrite = nil
		f()
	}
	m := r.members[userID]
	if !liveMember(m) || !m.JoinedAt.Equal(prev.JoinedAt) {
		return false, nil
	}
	if (p.Avatar != nil && m.Avatar != prev.Avatar) || (p.Banner != nil && m.Banner != prev.Banner) {
		return false, nil
	}
	setProfileOn(m, p.Apply(m.Profile()))
	r.writes++
	return true, nil
}

func (r *profileRepo) InsertAuditEntry(_ context.Context, e *SpaceAuditEntry) error {
	r.audits = append(r.audits, *e)
	return nil
}

func setProfileOn(m *SpaceMember, p MemberProfile) {
	m.Nickname, m.Avatar, m.Banner, m.AboutMe, m.Bio, m.Pronouns = p.Nick, p.Avatar, p.Banner, p.AboutMe, p.Bio, p.Pronouns
}

const (
	pfSpace    = 100
	pfOwner    = 1
	pfAdmin    = 2
	pfMod      = 3
	pfMember   = 4
	pfPlain    = 5
	pfEveryone = 10
)

// profileFixture: @everyone (Change Nickname via the defaults) < helper(1) < mod(2, Manage
// Nicknames) < admin(3, Administrator); owner 1, admin 2, mod 3, member 4 (helper), plain 5.
func profileFixture() *profileRepo {
	joined := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mk := func(uid int64, roles ...int64) *SpaceMember {
		return &SpaceMember{SpaceID: pfSpace, UserID: uid, RoleIDs: append([]int64{pfEveryone}, roles...), JoinedAt: joined}
	}
	return &profileRepo{
		space: &Space{ID: pfSpace, OwnerID: pfOwner, EveryoneRoleID: pfEveryone},
		roles: []SpaceRole{
			{SpaceID: pfSpace, ID: pfEveryone, Name: EveryoneRoleName, Position: 0, Permissions: permissions.DefaultEveryone},
			{SpaceID: pfSpace, ID: 11, Name: "helper", Position: 1},
			{SpaceID: pfSpace, ID: 12, Name: "mod", Position: 2, Permissions: permissions.PermManageNicknames},
			{SpaceID: pfSpace, ID: 13, Name: "admin", Position: 3, Permissions: permissions.PermAdministrator},
		},
		members: map[int64]*SpaceMember{
			pfOwner: mk(pfOwner), pfAdmin: mk(pfAdmin, 13), pfMod: mk(pfMod, 12), pfMember: mk(pfMember, 11), pfPlain: mk(pfPlain),
		},
	}
}

func TestUpdateMemberProfileRules(t *testing.T) {
	ctx := context.Background()

	t.Run("own nickname, never audited", func(t *testing.T) {
		r := profileFixture()
		s := NewService(r, nil, nil, nil, nil)
		m, err := s.UpdateMemberProfile(ctx, pfPlain, pfSpace, pfPlain, &MemberProfilePatch{Nick: profileStr("  Turtle "), Bio: profileStr("hi")})
		if err != nil {
			t.Fatal(err)
		}
		if m.Nickname != "Turtle" || m.Bio != "hi" || r.writes != 1 || len(r.audits) != 0 {
			t.Errorf("nick %q bio %q writes %d audits %d", m.Nickname, m.Bio, r.writes, len(r.audits))
		}
	})

	t.Run("own nickname without Change Nickname", func(t *testing.T) {
		r := profileFixture()
		r.roles[0].Permissions &^= permissions.PermChangeNickname
		s := NewService(r, nil, nil, nil, nil)
		if _, err := s.UpdateMemberProfile(ctx, pfPlain, pfSpace, pfPlain, &MemberProfilePatch{Nick: profileStr("x")}); !errors.Is(err, ErrCannotChangeNickname) {
			t.Errorf("got %v", err)
		}
		// The rest of the profile needs only membership, and re-sending the nickname you
		// already have (a whole-form save) is not a change.
		r.members[pfPlain].Nickname = "Set By A Mod"
		if _, err := s.UpdateMemberProfile(ctx, pfPlain, pfSpace, pfPlain, &MemberProfilePatch{Nick: profileStr("Set By A Mod"), Pronouns: profileStr("they/them")}); err != nil {
			t.Errorf("profile edit without the bit: %v", err)
		}
		if r.members[pfPlain].Pronouns != "they/them" {
			t.Error("pronouns not written")
		}
	})

	t.Run("moderator changes a lower member's nickname", func(t *testing.T) {
		r := profileFixture()
		r.members[pfMember].Nickname = "Old"
		s := NewService(r, nil, nil, nil, nil)
		if _, err := s.UpdateMemberProfile(ctx, pfMod, pfSpace, pfMember, &MemberProfilePatch{Nick: profileStr("New")}); err != nil {
			t.Fatal(err)
		}
		if len(r.audits) != 1 || r.audits[0].ActionType != AuditMemberNickUpdate || r.audits[0].TargetID != "4" || r.audits[0].UserID != pfMod {
			t.Fatalf("audits %+v", r.audits)
		}
		if got := r.audits[0].Changes; got != `{"nick":{"old":"Old","new":"New"}}` {
			t.Errorf("changes %s", got)
		}
		// Clearing it logs only the old side.
		if _, err := s.UpdateMemberProfile(ctx, pfMod, pfSpace, pfMember, &MemberProfilePatch{Nick: profileStr("")}); err != nil {
			t.Fatal(err)
		}
		if got := r.audits[1].Changes; got != `{"nick":{"old":"New"}}` {
			t.Errorf("clear changes %s", got)
		}
	})

	t.Run("hierarchy and owner", func(t *testing.T) {
		r := profileFixture()
		s := NewService(r, nil, nil, nil, nil)
		cases := []struct {
			actor, target int64
			patch         MemberProfilePatch
			want          error
		}{
			{pfMod, pfAdmin, MemberProfilePatch{Nick: profileStr("x")}, ErrRoleHierarchy},
			{pfMember, pfPlain, MemberProfilePatch{Nick: profileStr("x")}, ErrMissingPerm},
			{pfAdmin, pfOwner, MemberProfilePatch{Nick: profileStr("x")}, ErrCannotChangeOwnerNickname},
			{pfAdmin, pfOwner, MemberProfilePatch{Bio: profileStr("")}, ErrCannotResetOwnerProfile},
			{pfAdmin, pfOwner, MemberProfilePatch{Nick: profileStr(""), Bio: profileStr("")}, ErrCannotChangeOwnerNickname},
			{pfMod, pfMember, MemberProfilePatch{Bio: profileStr("my words now")}, ErrNotOwnProfile},
			{pfOwner, pfMember, MemberProfilePatch{Pronouns: profileStr("x")}, ErrNotOwnProfile},
			{pfMember, pfPlain, MemberProfilePatch{Bio: profileStr("")}, ErrMissingPerm},
			{pfMod, 99, MemberProfilePatch{Nick: profileStr("x")}, ErrMemberNotFound},
			{99, pfMember, MemberProfilePatch{Nick: profileStr("x")}, ErrNotMember},
			{pfMod, pfMember, MemberProfilePatch{}, ErrNothingToPatch},
			{pfMod, pfMember, MemberProfilePatch{Nick: profileStr("a\nb")}, ErrInvalidNickname},
		}
		for _, tc := range cases {
			p := tc.patch
			if _, err := s.UpdateMemberProfile(ctx, tc.actor, pfSpace, tc.target, &p); !errors.Is(err, tc.want) {
				t.Errorf("%d on %d %s: got %v, want %v", tc.actor, tc.target, profilePatchString(tc.patch), err, tc.want)
			}
		}
		if r.writes != 0 || len(r.audits) != 0 {
			t.Errorf("a refused patch wrote (%d writes, %d audits)", r.writes, len(r.audits))
		}
		// The owner may change anyone's nickname and reset anyone's profile.
		r.members[pfAdmin].Bio = "x"
		if _, err := s.UpdateMemberProfile(ctx, pfOwner, pfSpace, pfAdmin, &MemberProfilePatch{Nick: profileStr("y"), Bio: profileStr("")}); err != nil {
			t.Errorf("owner: %v", err)
		}
	})

	t.Run("unchanged patch writes nothing", func(t *testing.T) {
		r := profileFixture()
		r.members[pfMember].Nickname = "Same"
		s := NewService(r, nil, nil, nil, nil)
		m, err := s.UpdateMemberProfile(ctx, pfMod, pfSpace, pfMember, &MemberProfilePatch{Nick: profileStr(" Same "), Bio: profileStr("")})
		if err != nil || m == nil || m.Nickname != "Same" {
			t.Fatalf("m %+v err %v", m, err)
		}
		if r.writes != 0 || len(r.audits) != 0 {
			t.Errorf("writes %d audits %d", r.writes, len(r.audits))
		}
	})
}

// profileNebulaStub records DELETEs the way Nebula would answer them.
func profileNebulaStub(t *testing.T) (*config.Config, <-chan string) {
	t.Helper()
	deleted := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			deleted <- strings.TrimPrefix(req.URL.Path, "/v1/")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{}
	cfg.Nebula.BaseURL = srv.URL
	cfg.Nebula.PublicURL = "https://cdn.test"
	cfg.Nebula.UploadSecret = "secret"
	return cfg, deleted
}

func expectBlobDeletes(t *testing.T, deleted <-chan string, want ...string) {
	t.Helper()
	var got []string
	timeout := time.After(5 * time.Second)
	for len(got) < len(want) {
		select {
		case k := <-deleted:
			got = append(got, k)
		case <-timeout:
			t.Fatalf("deleted %v, want %v", got, want)
		}
	}
	select {
	case k := <-deleted:
		t.Fatalf("unexpected extra delete %q", k)
	case <-time.After(100 * time.Millisecond):
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
}

func profileBlob(uid int64, name string) (key, url string) {
	key = memberBlobPrefix(pfSpace, uid) + name
	return key, "https://cdn.test/v1/" + key
}

func TestMemberProfileReset(t *testing.T) {
	ctx := context.Background()
	cfg, deleted := profileNebulaStub(t)
	r := profileFixture()
	avatarKey, avatarURL := profileBlob(pfMember, "avatar-0123456789abcdef.png")
	m := r.members[pfMember]
	m.Nickname, m.Avatar, m.Banner, m.Bio = "Keeps", avatarURL, "https://cdn.elsewhere.org/v1/b.png", "rude"
	s := NewService(r, nil, nil, nil, cfg)

	out, err := s.UpdateMemberProfile(ctx, pfMod, pfSpace, pfMember, &MemberProfilePatch{
		Avatar: profileStr(""), Banner: profileStr(""), Bio: profileStr(""), AboutMe: profileStr(""), Pronouns: profileStr(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Avatar != "" || out.Banner != "" || out.Bio != "" || out.Nickname != "Keeps" {
		t.Errorf("after reset: %+v", out)
	}
	if len(r.audits) != 1 || r.audits[0].ActionType != AuditMemberProfileReset {
		t.Fatalf("audits %+v", r.audits)
	}
	// Only what was set is named, and none of the values are kept.
	if got := r.audits[0].Changes; got != `{"fields":["avatar","banner","bio"]}` {
		t.Errorf("changes %s", got)
	}
	// Our own image is released; the one on another instance's CDN is not ours to delete.
	expectBlobDeletes(t, deleted, avatarKey)
}

func TestMemberProfileImageCompareAndSet(t *testing.T) {
	ctx := context.Background()

	t.Run("lost race retries and releases what it really replaced", func(t *testing.T) {
		cfg, deleted := profileNebulaStub(t)
		r := profileFixture()
		_, oldURL := profileBlob(pfPlain, "avatar-1111111111111111.png")
		midKey, midURL := profileBlob(pfPlain, "avatar-2222222222222222.png")
		_, newURL := profileBlob(pfPlain, "avatar-3333333333333333.png")
		r.members[pfPlain].Avatar = oldURL
		// Another upload lands between our read and our write: it replaced old with mid (and
		// released old itself).
		r.beforeWrite = func() { r.members[pfPlain].Avatar = midURL }
		s := NewService(r, nil, nil, nil, cfg)
		m, err := s.UpdateMemberProfile(ctx, pfPlain, pfSpace, pfPlain, &MemberProfilePatch{Avatar: profileStr(newURL)})
		if err != nil {
			t.Fatal(err)
		}
		if m.Avatar != newURL || r.writes != 1 {
			t.Errorf("avatar %q writes %d", m.Avatar, r.writes)
		}
		// mid is what this write overwrote; old was the other request's to release.
		expectBlobDeletes(t, deleted, midKey)
	})

	t.Run("retry re-checks the nickname gate against the fresh row", func(t *testing.T) {
		r := profileFixture()
		r.roles[0].Permissions &^= permissions.PermChangeNickname
		_, avatarURL := profileBlob(pfPlain, "avatar-1111111111111111.png")
		r.members[pfPlain].Nickname, r.members[pfPlain].Avatar = "Bad", avatarURL
		// A moderator renames the member and clears their avatar between our read and our
		// write: re-sending "Bad" is no longer "the nickname you have".
		r.beforeWrite = func() { r.members[pfPlain].Nickname, r.members[pfPlain].Avatar = "Clean", "" }
		s := NewService(r, nil, nil, nil, nil)
		if _, err := s.UpdateMemberProfile(ctx, pfPlain, pfSpace, pfPlain, &MemberProfilePatch{Nick: profileStr("Bad"), Avatar: profileStr("")}); !errors.Is(err, ErrCannotChangeNickname) {
			t.Errorf("got %v, want %v", err, ErrCannotChangeNickname)
		}
		if got := r.members[pfPlain].Nickname; got != "Clean" || r.writes != 0 {
			t.Errorf("nick %q writes %d: the moderator's rename was undone", got, r.writes)
		}
	})

	t.Run("retry re-checks the hierarchy against the fresh roles", func(t *testing.T) {
		r := profileFixture()
		_, avatarURL := profileBlob(pfMember, "avatar-1111111111111111.png")
		r.members[pfMember].Avatar = avatarURL
		// The member is promoted to mod (level with the moderator) and changes their avatar
		// underneath the moderator's reset.
		r.beforeWrite = func() {
			r.members[pfMember].RoleIDs = []int64{pfEveryone, 12}
			r.members[pfMember].Avatar = ""
		}
		s := NewService(r, nil, nil, nil, nil)
		if _, err := s.UpdateMemberProfile(ctx, pfMod, pfSpace, pfMember, &MemberProfilePatch{Nick: profileStr("x"), Avatar: profileStr("")}); !errors.Is(err, ErrRoleHierarchy) {
			t.Errorf("got %v, want %v", err, ErrRoleHierarchy)
		}
		if r.members[pfMember].Nickname != "" || r.writes != 0 || len(r.audits) != 0 {
			t.Errorf("nick %q writes %d audits %d", r.members[pfMember].Nickname, r.writes, len(r.audits))
		}
	})

	t.Run("member removed underneath", func(t *testing.T) {
		r := profileFixture()
		r.beforeWrite = func() { delete(r.members, pfPlain) }
		s := NewService(r, nil, nil, nil, nil)
		if _, err := s.UpdateMemberProfile(ctx, pfPlain, pfSpace, pfPlain, &MemberProfilePatch{Bio: profileStr("x")}); !errors.Is(err, ErrMemberNotFound) {
			t.Errorf("got %v", err)
		}
	})

	t.Run("member left and rejoined underneath", func(t *testing.T) {
		r := profileFixture()
		r.beforeWrite = func() {
			r.members[pfMember] = &SpaceMember{SpaceID: pfSpace, UserID: pfMember, RoleIDs: []int64{pfEveryone}, JoinedAt: time.Now().UTC()}
		}
		s := NewService(r, nil, nil, nil, nil)
		// The moderator's check was against the old membership; it must not land on the new one.
		if _, err := s.UpdateMemberProfile(ctx, pfMod, pfSpace, pfMember, &MemberProfilePatch{Nick: profileStr("x")}); !errors.Is(err, ErrMemberNotFound) {
			t.Errorf("got %v", err)
		}
		if r.members[pfMember].Nickname != "" {
			t.Error("the write landed on the rejoined membership")
		}
	})
}

func TestMemberProfileUpdateStatement(t *testing.T) {
	joined := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	prev := &SpaceMember{UserID: 4, JoinedAt: joined, Avatar: "https://a/v1/x"}

	stmt, args := memberProfileUpdate(100, 4, prev, MemberProfilePatch{Nick: profileStr("n"), Bio: profileStr("")})
	if stmt != "UPDATE space_members SET nickname = ?, bio = ? WHERE space_id = ? AND user_id = ? IF joined_at = ?" {
		t.Errorf("text-only statement: %s", stmt)
	}
	if !reflect.DeepEqual(args, []interface{}{"n", nil, int64(100), int64(4), joined}) {
		t.Errorf("text-only args: %#v", args)
	}

	stmt, args = memberProfileUpdate(100, 4, prev, MemberProfilePatch{Avatar: profileStr("https://a/v1/y"), Banner: profileStr("")})
	if stmt != "UPDATE space_members SET avatar = ?, banner = ? WHERE space_id = ? AND user_id = ? IF joined_at = ? AND avatar = ? AND banner = ?" {
		t.Errorf("image statement: %s", stmt)
	}
	// prev's empty banner is NULL in the row, and the condition has to say so.
	if !reflect.DeepEqual(args, []interface{}{"https://a/v1/y", nil, int64(100), int64(4), joined, "https://a/v1/x", nil}) {
		t.Errorf("image args: %#v", args)
	}

	if stmt, _ := memberProfileUpdate(100, 4, prev, MemberProfilePatch{}); stmt != "" {
		t.Errorf("empty patch built %q", stmt)
	}
}

// ---- the handlers ------------------------------------------------------------------------

func (r *profileRepo) ListMembers(context.Context, int64) ([]SpaceMember, error) {
	var out []SpaceMember
	for _, m := range r.members {
		if liveMember(m) {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

type profileUsers struct {
	auth.UserRepository
	users map[int64]*auth.User
}

func (u *profileUsers) GetByID(_ context.Context, id int64) (*auth.User, error) {
	return u.users[id], nil
}

func (u *profileUsers) GetByIDs(_ context.Context, ids []int64) ([]*auth.User, error) {
	out := make([]*auth.User, len(ids))
	for i, id := range ids {
		out[i] = u.users[id]
	}
	return out, nil
}

// profileApp serves the member routes as routes/spaces.go mounts them, signed in as `as`.
func profileApp(r *profileRepo, users *profileUsers, as *int64) *fiber.App {
	h := NewHandler(NewService(r, nil, users, nil, nil), nil)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(auth.LocalsKeyUser, users.users[*as])
		return c.Next()
	})
	app.Get("/spaces/:id/members", h.Members)
	app.Get("/spaces/:id/members/:userId", h.GetMember)
	app.Patch("/spaces/:id/members/:userId", h.PatchMember)
	app.Post("/spaces/:id/members/@me/avatar", h.PostMemberAvatar)
	return app
}

func TestMemberProfileHandlers(t *testing.T) {
	r := profileFixture()
	r.members[pfMember].Nickname = "Helper"
	users := &profileUsers{users: map[int64]*auth.User{}}
	for uid := range r.members {
		users.users[uid] = &auth.User{ID: uid, Username: "user" + strings.Repeat("x", int(uid)), DisplayName: "Global"}
	}
	users.users[99] = &auth.User{ID: 99, Username: "stranger"}
	as := int64(pfPlain)
	app := profileApp(r, users, &as)

	do := func(method, path, body string) (int, map[string]interface{}) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]interface{}
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	if code, body := do("GET", "/spaces/100/members/@me", ""); code != 200 || body["id"] != "5" || body["nick"] != nil || body["joined_at"] == nil {
		t.Errorf("GET @me: %d %v", code, body)
	}
	if code, body := do("GET", "/spaces/100/members/4", ""); code != 200 || body["nick"] != "Helper" || body["display_name"] != "Global" {
		t.Errorf("GET other: %d %v", code, body)
	}
	if code, body := do("GET", "/spaces/100/members/nope", ""); code != 400 || body["error"] != "invalid user id" {
		t.Errorf("GET bad id: %d %v", code, body)
	}
	if code, body := do("GET", "/spaces/100/members/99", ""); code != 404 || body["error"] != "member not found" {
		t.Errorf("GET non-member target: %d %v", code, body)
	}

	if code, body := do("PATCH", "/spaces/100/members/@me", `{"nick":"  Turtle ","pronouns":"they/them"}`); code != 200 || body["nick"] != "Turtle" || body["space_pronouns"] != "they/them" || body["pronouns"] != "" {
		t.Errorf("PATCH @me: %d %v", code, body)
	}
	if code, body := do("PATCH", "/spaces/100/members/%40me", `{"nick":null}`); code != 200 || body["nick"] != nil {
		t.Errorf("PATCH encoded @me: %d %v", code, body)
	}
	if code, body := do("PATCH", "/spaces/100/members/@me", `{"avatar":"https://cdn.test/v1/x.png"}`); code != 400 || body["error"] != ErrInvalidProfileImage.Error() {
		t.Errorf("PATCH @me avatar URL: %d %v", code, body)
	}
	if code, body := do("PATCH", "/spaces/100/members/@me", `{"nick":`); code != 400 || body["error"] != "invalid json" {
		t.Errorf("PATCH bad json: %d %v", code, body)
	}
	if code, body := do("PATCH", "/spaces/100/members/@me", `{"nick":3}`); code != 400 || body["error"] != "nick must be a string or null" {
		t.Errorf("PATCH number: %d %v", code, body)
	}

	as = pfMod
	for _, b := range []string{`{"bio":"mine now"}`, `{"avatar":"https://cdn.test/v1/x.png"}`} {
		if code, body := do("PATCH", "/spaces/100/members/4", b); code != 403 || body["code"] != "cannot_edit_member_profile" || body["error"] != ErrNotOwnProfile.Error() {
			t.Errorf("PATCH other's value %s: %d %v", b, code, body)
		}
	}
	if code, body := do("PATCH", "/spaces/100/members/4", `{"nick":"Renamed"}`); code != 200 || body["nick"] != "Renamed" || body["id"] != "4" {
		t.Errorf("mod renames: %d %v", code, body)
	}
	if code, body := do("PATCH", "/spaces/100/members/2", `{"nick":"x"}`); code != 403 || body["error"] != ErrRoleHierarchy.Error() {
		t.Errorf("mod on admin: %d %v", code, body)
	}

	// Resetting the owner's profile is refused for being the owner's, not as "a value on
	// someone else's" - so without the cannot_edit_member_profile code, as a mirror relays it.
	as = pfAdmin
	if code, body := do("PATCH", "/spaces/100/members/1", `{"bio":null}`); code != 403 || body["error"] != ErrCannotResetOwnerProfile.Error() || body["code"] != nil {
		t.Errorf("admin resets owner: %d %v", code, body)
	}

	as = 99
	if code, body := do("GET", "/spaces/100/members/4", ""); code != 403 || body["error"] != ErrNotMember.Error() {
		t.Errorf("stranger GET: %d %v", code, body)
	}
	// Uploads are refused before any file is looked at when Nebula is not configured.
	if code, body := do("POST", "/spaces/100/members/@me/avatar", ""); code != 503 || body["error"] != "file uploads are not configured" {
		t.Errorf("upload without nebula: %d %v", code, body)
	}

	// The member list carries every member's space profile beside the global one.
	as = pfPlain
	req := httptest.NewRequest("GET", "/spaces/100/members", nil)
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil || len(list) != 5 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	for _, row := range list {
		for _, k := range []string{"nick", "space_avatar", "space_banner", "space_about_me", "space_bio", "space_pronouns", "roles", "joined_at"} {
			if _, ok := row[k]; !ok {
				t.Errorf("member %v lacks %q", row["id"], k)
			}
		}
	}
}

func TestProfileWriteRefused(t *testing.T) {
	for _, err := range []error{ErrInvalidNickname, ErrNotOwnProfile, ErrMemberNotFound, ErrMemberProfileChanged,
		fmt.Errorf("wrapped: %w", ErrCannotChangeNickname)} {
		if !profileWriteRefused(err) {
			t.Errorf("%v: a refused write must let the upload delete its new image", err)
		}
	}
	for _, err := range []error{errors.New("gocql: write timeout"), ErrOriginUnavailable} {
		if profileWriteRefused(err) {
			t.Errorf("%v: an ambiguous failure may have landed; the new image must be kept", err)
		}
	}
}
