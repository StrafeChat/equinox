package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// Role order and member roles across instances: the role.positions op on the origin, the same
// op from a mirror over the signed client and back, the fallback for an origin from before the
// op, and member.roles deciding by the role alone.

const (
	rpSpace  = 100 // a.example's space
	rpMirror = 9   // b.example's id for it

	// Users by their ids on a.example, the origin; b.example's users are shadows there.
	rpOwner  = 1 // a.example's own: the owner
	rpMod    = 4 // b.example's user 40: Moderators (Manage Roles)
	rpMember = 5 // b.example's user 50: no role
	rpBot    = 6 // b.example's bot 60: its managed role
	rpHigh   = 8 // a.example's own: Staff, above the moderator

	// b.example's ids for its users and for the origin's owner.
	rpModB      = 40
	rpMemberB   = 50
	rpBotB      = 60
	rpOwnerB    = 7
	rpOutsiderB = 70 // b.example's own, not in the space

	// Roles, with their positions. Role ids are the origin's on every instance.
	rpEveryone = 10 // 0
	rpBotRole  = 20 // 1, the bot's managed role
	rpVerified = 21 // 2
	rpHelpers  = 22 // 3
	rpModRole  = 24 // 4, Manage Roles
	rpStaff    = 25 // 5
)

// rpRoles is the space's roles as stored under spaceID, the managed role belonging to botID.
func rpRoles(spaceID, botID int64) []spaces.SpaceRole {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mk := func(roleID int64, name string, pos int, perms int64) spaces.SpaceRole {
		return spaces.SpaceRole{SpaceID: spaceID, ID: roleID, Name: name, Position: pos, Permissions: perms, CreatedAt: at, UpdatedAt: at}
	}
	bot := mk(rpBotRole, "RoleBot", 1, 0)
	bot.BotID = botID
	return []spaces.SpaceRole{
		mk(rpEveryone, spaces.EveryoneRoleName, 0, permissions.DefaultEveryone),
		bot,
		mk(rpVerified, "Verified", 2, 0),
		mk(rpHelpers, "Helpers", 3, 0),
		mk(rpModRole, "Moderators", 4, permissions.PermManageRoles),
		mk(rpStaff, "Staff", 5, 0),
	}
}

// roleRepo is originSpaceRepo plus the role and member-role writes, recorded: the ids of the
// roles written, in order, the number of whole member role lists written, and the single-role
// set operations as "add:<user>:<role>" / "remove:<user>:<role>".
type roleRepo struct {
	*originSpaceRepo
	roleWrites   []int64
	memberWrites int
	setOps       []string
}

func newRoleRepo(sp *spaces.Space, roles []spaces.SpaceRole, members map[int64]*spaces.SpaceMember) *roleRepo {
	return &roleRepo{originSpaceRepo: &originSpaceRepo{space: sp, roles: roles, members: members}}
}

func (r *roleRepo) GetSpaceRole(_ context.Context, _, roleID int64) (*spaces.SpaceRole, error) {
	for i := range r.roles {
		if r.roles[i].ID == roleID {
			cp := r.roles[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *roleRepo) putRole(role *spaces.SpaceRole) {
	r.roleWrites = append(r.roleWrites, role.ID)
	for i := range r.roles {
		if r.roles[i].ID == role.ID {
			r.roles[i] = *role
			return
		}
	}
	r.roles = append(r.roles, *role)
}

func (r *roleRepo) UpdateSpaceRole(_ context.Context, role *spaces.SpaceRole) error {
	r.putRole(role)
	return nil
}

func (r *roleRepo) InsertSpaceRole(_ context.Context, role *spaces.SpaceRole) error {
	r.putRole(role)
	return nil
}

func (r *roleRepo) SetMemberRoleIDs(_ context.Context, _, userID int64, roleIDs []int64) error {
	r.memberWrites++
	r.members[userID].RoleIDs = append([]int64(nil), roleIDs...)
	return nil
}

func (r *roleRepo) AddMemberRoleID(_ context.Context, _, userID, roleID int64) error {
	r.setOps = append(r.setOps, fmt.Sprintf("add:%d:%d", userID, roleID))
	m := r.members[userID]
	for _, rid := range m.RoleIDs {
		if rid == roleID {
			return nil
		}
	}
	m.RoleIDs = append(m.RoleIDs, roleID)
	return nil
}

func (r *roleRepo) RemoveMemberRoleID(_ context.Context, _, userID, roleID int64) error {
	r.setOps = append(r.setOps, fmt.Sprintf("remove:%d:%d", userID, roleID))
	m := r.members[userID]
	out := m.RoleIDs[:0]
	for _, rid := range m.RoleIDs {
		if rid != roleID {
			out = append(out, rid)
		}
	}
	m.RoleIDs = out
	return nil
}

func (r *roleRepo) holds(userID, roleID int64) bool {
	m := r.members[userID]
	if m == nil {
		return false
	}
	for _, rid := range m.RoleIDs {
		if rid == roleID {
			return true
		}
	}
	return false
}

func (r *roleRepo) positions() map[int64]int {
	out := make(map[int64]int, len(r.roles))
	for _, role := range r.roles {
		out[role.ID] = role.Position
	}
	return out
}

func (r *roleRepo) role(roleID int64) spaces.SpaceRole {
	for _, role := range r.roles {
		if role.ID == roleID {
			return role
		}
	}
	return spaces.SpaceRole{}
}

// roleFedRepo is the slice of the federation Repository these paths read: on a mirror, the
// space it maps to; on the origin, the instances mirroring the space.
type roleFedRepo struct {
	Repository
	mapping *SpaceMapping
	peers   []string
}

func (r *roleFedRepo) GetSpaceMapping(_ context.Context, spaceID int64) (*SpaceMapping, error) {
	if r.mapping != nil && r.mapping.SpaceID == spaceID {
		cp := *r.mapping
		return &cp, nil
	}
	return nil, nil
}

func (r *roleFedRepo) GetSpaceByOrigin(_ context.Context, domain string, originSpaceID int64) (*SpaceMapping, error) {
	if r.mapping != nil && r.mapping.OriginDomain == domain && r.mapping.OriginSpaceID == originSpaceID {
		cp := *r.mapping
		return &cp, nil
	}
	return nil, nil
}

func (r *roleFedRepo) ListSpacePeers(context.Context, int64) ([]string, error) {
	return r.peers, nil
}

func (r *roleFedRepo) IsSpacePeer(_ context.Context, _ int64, domain string) (bool, error) {
	for _, p := range r.peers {
		if p == domain {
			return true, nil
		}
	}
	return false, nil
}

func rpMemberRow(spaceID, userID int64, roles ...int64) *spaces.SpaceMember {
	return &spaces.SpaceMember{
		SpaceID: spaceID, UserID: userID, RoleIDs: append([]int64{rpEveryone}, roles...),
		JoinedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
}

// rpOrigin is a.example hosting the space, which b.example mirrors. The spaces service runs the
// real role code over roleRepo with this engine as its federator, so the relays a write makes
// for b.example are captured whenever the context asks for that (SpaceManage's does).
func rpOrigin() (*Service, *roleRepo) {
	repo := newRoleRepo(
		&spaces.Space{ID: rpSpace, OwnerID: rpOwner, EveryoneRoleID: rpEveryone},
		rpRoles(rpSpace, rpBot),
		map[int64]*spaces.SpaceMember{
			rpOwner: rpMemberRow(rpSpace, rpOwner), rpMod: rpMemberRow(rpSpace, rpMod, rpModRole),
			rpMember: rpMemberRow(rpSpace, rpMember), rpBot: rpMemberRow(rpSpace, rpBot, rpBotRole),
			rpHigh: rpMemberRow(rpSpace, rpHigh, rpStaff),
		},
	)
	bot := shadowUser(rpBot, rpBotB, "b.example")
	bot.Bot = true
	users := &profileTestUsers{byID: map[int64]*auth.User{
		rpOwner:  {ID: rpOwner, Username: "owner"},
		rpMod:    shadowUser(rpMod, rpModB, "b.example"),
		rpMember: shadowUser(rpMember, rpMemberB, "b.example"),
		rpBot:    bot,
		rpHigh:   {ID: rpHigh, Username: "staff"},
	}}
	s := &Service{
		fcfg:  config.FederationConfig{Enabled: true, Domain: "a.example"},
		users: users,
		repo:  &roleFedRepo{peers: []string{"b.example"}},
	}
	s.spaceSvc = spaces.NewService(repo, nil, users, nil, nil)
	s.spaceSvc.SetFederator(s)
	return s, repo
}

func testSigner(t *testing.T) *Signer {
	t.Helper()
	seed, err := generateSeed()
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadSigner(config.FederationConfig{SigningKeySeed: seed}, false)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// knownPeer is a Discovery that already holds domain's instance document (signed with key),
// as it does after a first lookup.
func knownPeer(cfg config.FederationConfig, domain string, key *Signer) *Discovery {
	d := NewDiscovery(cfg)
	d.c[domain] = cachedInfo{fetchedAt: time.Now(), info: &InstanceInfo{
		Domain: domain, FederationURL: "https://" + domain + BasePath, KeyID: key.KeyID, PublicKey: key.PublicKeyBase64(),
	}}
	return d
}

// appTransport delivers a request to a Fiber app in process: the instance at the other end of
// a signed client, without a network.
type appTransport struct{ app *fiber.App }

func (t appTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
}

// rpOriginApp serves the origin's POST /spaces/manage behind the real signature check, with
// mirrorKey known as b.example's key. ops records each op asked for, in order.
func rpOriginApp(origin *Service, mirrorKey *Signer, ops *[]string) *fiber.App {
	origin.discovery = knownPeer(origin.fcfg, "b.example", mirrorKey)
	app := fiber.New()
	app.Post(BasePath+"/spaces/manage", origin.RequireInstance(), func(c fiber.Ctx) error {
		var req SpaceManageRequest
		if json.Unmarshal(c.Body(), &req) == nil {
			*ops = append(*ops, req.Op)
		}
		return c.Next()
	}, NewHandler(origin).SpaceManage)
	return app
}

// rpMirrorOf is b.example mirroring the space as its space 9, its signed client reaching the
// origin (a.example, whose key is originKey) through app.
func rpMirrorOf(app *fiber.App, key, originKey *Signer) (*Service, *roleRepo) {
	repo := newRoleRepo(
		&spaces.Space{ID: rpMirror, OwnerID: rpOwnerB, EveryoneRoleID: rpEveryone},
		rpRoles(rpMirror, rpBotB),
		map[int64]*spaces.SpaceMember{
			rpModB: rpMemberRow(rpMirror, rpModB, rpModRole), rpMemberB: rpMemberRow(rpMirror, rpMemberB),
			rpBotB: rpMemberRow(rpMirror, rpBotB, rpBotRole), rpOwnerB: rpMemberRow(rpMirror, rpOwnerB),
		},
	)
	users := &profileTestUsers{byID: map[int64]*auth.User{
		rpModB:      {ID: rpModB, Username: "mod"},
		rpMemberB:   {ID: rpMemberB, Username: "member"},
		rpBotB:      {ID: rpBotB, Username: "RoleBot", Bot: true},
		rpOwnerB:    shadowUser(rpOwnerB, rpOwner, "a.example"),
		rpOutsiderB: {ID: rpOutsiderB, Username: "outsider"},
	}}
	cfg := config.FederationConfig{Enabled: true, Domain: "b.example"}
	s := &Service{
		fcfg:  cfg,
		users: users,
		repo:  &roleFedRepo{mapping: &SpaceMapping{SpaceID: rpMirror, OriginDomain: "a.example", OriginSpaceID: rpSpace}},
		client: &Client{
			cfg: cfg, signer: key, discovery: knownPeer(cfg, "a.example", originKey),
			http: &http.Client{Transport: appTransport{app}},
		},
	}
	s.spaceSvc = spaces.NewService(repo, nil, users, nil, nil)
	s.spaceSvc.SetFederator(s)
	return s, repo
}

func positionsOf(roles []spaces.SpaceRole) map[int64]int {
	out := make(map[int64]int, len(roles))
	for _, r := range roles {
		out[r.ID] = r.Position
	}
	return out
}

// The wire shape FEDERATION.md documents for role.positions.
func TestRolePositionsParamsWire(t *testing.T) {
	raw, err := json.Marshal(manageRolePositions{Positions: []manageRolePosition{{Role: "20", Position: 3}, {Role: "21", Position: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"positions":[{"role":"20","position":3},{"role":"21","position":1}]}`; got != want {
		t.Errorf("wire %s, want %s", got, want)
	}
}

// ---- the origin side -------------------------------------------------------------------

// rolePositionsAt runs role.positions on the origin as actor, as SpaceManage does for a request
// from b.example: the relays for b.example are captured, and returned.
func rolePositionsAt(t *testing.T, s *Service, actor int64, params string) (any, []RelayEvent, error) {
	t.Helper()
	ctx, capture := WithRelayCapture(context.Background(), "b.example")
	var raw json.RawMessage
	if params != "" {
		raw = json.RawMessage(params)
	}
	res, err := s.manageAt(ctx, &spaces.Space{ID: rpSpace}, s.users.(*profileTestUsers).byID[actor], "role.positions", raw)
	return res, capture.take(), err
}

// relayedRoles reads back the role relays captured for the asker: role id -> position, and
// the ids in the order they were relayed.
func relayedRoles(t *testing.T, events []RelayEvent) (map[int64]int, []int64) {
	t.Helper()
	pos := map[int64]int{}
	var order []int64
	for _, ev := range events {
		if ev.Path != "/spaces/roles" {
			t.Errorf("relay to %s", ev.Path)
			continue
		}
		var b SpaceRoleEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			t.Fatal(err)
		}
		if b.Action != "upsert" || b.Space.OriginDomain != "a.example" || b.Space.OriginSpaceID != "100" {
			t.Errorf("relay %+v", b)
		}
		rid, err := id.Parse(b.Role.ID)
		if err != nil {
			t.Fatal(err)
		}
		pos[rid] = b.Role.Position
		order = append(order, rid)
	}
	return pos, order
}

func TestManageAtRolePositions(t *testing.T) {
	t.Run("a moderator elsewhere lifts the bot's role into the slots below them", func(t *testing.T) {
		s, repo := rpOrigin()
		// What the web client sends: the movable roles in their new order, reusing the
		// positions they held (bot 1 -> 3, Helpers 3 -> 2, Verified 2 -> 1).
		res, events, err := rolePositionsAt(t, s, rpMod, `{"positions":[{"role":"20","position":3},{"role":"22","position":2},{"role":"21","position":1}]}`)
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]int{rpEveryone: 0, rpVerified: 1, rpHelpers: 2, rpBotRole: 3, rpModRole: 4, rpStaff: 5}
		if got := repo.positions(); !reflect.DeepEqual(got, want) {
			t.Errorf("stored %v, want %v", got, want)
		}

		// The answer is every role in its wire form, by position, as it travels.
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		var answer []SpaceRoleWire
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatalf("answer %s: %v", raw, err)
		}
		got := map[int64]int{}
		last := -1
		for _, w := range answer {
			rid, _ := id.Parse(w.ID)
			got[rid] = w.Position
			if w.Position < last {
				t.Errorf("answer not by position: %s", raw)
			}
			last = w.Position
			if rid == rpBotRole && w.Bot != "@60:b.example" {
				t.Errorf("the managed role lost its bot: %+v", w)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("answered %v, want %v", got, want)
		}

		// Only the moved rows are written - roles moving down first, lowest destination first,
		// then the role moving up - and each is relayed to the asker in that order.
		if wantWrites := []int64{rpVerified, rpHelpers, rpBotRole}; !reflect.DeepEqual(repo.roleWrites, wantWrites) {
			t.Errorf("writes %v, want %v", repo.roleWrites, wantWrites)
		}
		relayed, order := relayedRoles(t, events)
		if !reflect.DeepEqual(order, repo.roleWrites) {
			t.Errorf("relayed %v, written %v", order, repo.roleWrites)
		}
		for rid, p := range relayed {
			if want[rid] != p {
				t.Errorf("relayed role %d at %d, stored at %d", rid, p, want[rid])
			}
		}

		// Each listed role that moved is audited, as the member who asked.
		audited := map[string]string{}
		for _, a := range repo.audits {
			if a.ActionType != spaces.AuditRoleUpdate || a.UserID != rpMod {
				t.Errorf("audit %+v", a)
			}
			audited[a.TargetID] = a.Changes
		}
		wantAudits := map[string]string{
			"20": `{"position":{"old":1,"new":3}}`,
			"22": `{"position":{"old":3,"new":2}}`,
			"21": `{"position":{"old":2,"new":1}}`,
		}
		if !reflect.DeepEqual(audited, wantAudits) {
			t.Errorf("audits %v, want %v", audited, wantAudits)
		}
	})

	t.Run("a move into an occupied slot pushes the run above it up, unaudited", func(t *testing.T) {
		s, repo := rpOrigin()
		_, events, err := rolePositionsAt(t, s, rpMod, `{"positions":[{"role":"20","position":3}]}`)
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]int{rpEveryone: 0, rpVerified: 2, rpBotRole: 3, rpHelpers: 4, rpModRole: 5, rpStaff: 6}
		if got := repo.positions(); !reflect.DeepEqual(got, want) {
			t.Errorf("stored %v, want %v", got, want)
		}
		// Every role that moved is relayed, the bumped ones too, highest destination first.
		_, order := relayedRoles(t, events)
		if wantOrder := []int64{rpStaff, rpModRole, rpHelpers, rpBotRole}; !reflect.DeepEqual(order, wantOrder) {
			t.Errorf("relayed %v, want %v", order, wantOrder)
		}
		if len(repo.audits) != 1 || repo.audits[0].TargetID != "20" {
			t.Errorf("audits %+v, want the listed role only", repo.audits)
		}
	})

	t.Run("refusals write nothing", func(t *testing.T) {
		cases := []struct {
			name   string
			actor  int64
			params string
			want   error
		}{
			{"a role above the moderator", rpMod, `{"positions":[{"role":"25","position":1}]}`, spaces.ErrRoleHierarchy},
			{"the moderator's own role", rpMod, `{"positions":[{"role":"24","position":1}]}`, spaces.ErrRoleHierarchy},
			{"up to the moderator's rank", rpMod, `{"positions":[{"role":"21","position":4}]}`, spaces.ErrRoleHierarchy},
			{"an allowed move beside a refused one", rpMod, `{"positions":[{"role":"21","position":1},{"role":"25","position":2}]}`, spaces.ErrRoleHierarchy},
			{"@everyone", rpMod, `{"positions":[{"role":"10","position":1}]}`, spaces.ErrCannotMoveEveryone},
			{"a role the space does not have", rpMod, `{"positions":[{"role":"999","position":1}]}`, spaces.ErrRoleNotFound},
			{"without Manage Roles", rpMember, `{"positions":[{"role":"21","position":1}]}`, spaces.ErrMissingPerm},
			{"nothing to move", rpMod, `{"positions":[]}`, spaces.ErrInvalidRolePositions},
			{"the same role twice", rpMod, `{"positions":[{"role":"21","position":1},{"role":"21","position":2}]}`, spaces.ErrInvalidRolePositions},
			{"position 0", rpMod, `{"positions":[{"role":"21","position":0}]}`, spaces.ErrInvalidRolePosition},
			{"no position", rpMod, `{"positions":[{"role":"21"}]}`, spaces.ErrInvalidRolePosition},
			{"not a role id", rpMod, `{"positions":[{"role":"verified","position":1}]}`, errBadParams},
			{"not the params' shape", rpMod, `[{"role":"21","position":1}]`, errBadParams},
			{"no params", rpMod, "", errBadParams},
		}
		for _, tc := range cases {
			s, repo := rpOrigin()
			res, events, err := rolePositionsAt(t, s, tc.actor, tc.params)
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
			}
			if res != nil || len(events) != 0 || len(repo.roleWrites) != 0 || len(repo.audits) != 0 {
				t.Errorf("%s: a refusal answered %v, relayed %d, wrote %v, audited %d", tc.name, res, len(events), repo.roleWrites, len(repo.audits))
			}
		}
	})
}

// ---- mirror to origin and back ---------------------------------------------------------

// rpWire is the two instances, the mirror's client signing into the origin's real handler.
type rpWire struct {
	mirror       *Service
	mrepo, orepo *roleRepo
	ops          *[]string // the ops the origin was asked for
}

func newRPWire(t *testing.T) *rpWire {
	t.Helper()
	originKey, mirrorKey := testSigner(t), testSigner(t)
	origin, orepo := rpOrigin()
	w := &rpWire{orepo: orepo, ops: &[]string{}}
	w.mirror, w.mrepo = rpMirrorOf(rpOriginApp(origin, mirrorKey, w.ops), mirrorKey, originKey)
	return w
}

func TestRolePositionsOverTheWire(t *testing.T) {
	ctx := context.Background()

	t.Run("a moderator on the mirror lifts the bot's role", func(t *testing.T) {
		w := newRPWire(t)
		// Through the mirror's spaces service, as PATCH /spaces/9/roles on b.example.
		roles, err := w.mirror.spaceSvc.ReorderSpaceRoles(ctx, rpModB, rpMirror, []spaces.RolePosition{
			{RoleID: rpBotRole, Position: 3}, {RoleID: rpHelpers, Position: 2}, {RoleID: rpVerified, Position: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]int{rpEveryone: 0, rpVerified: 1, rpHelpers: 2, rpBotRole: 3, rpModRole: 4, rpStaff: 5}
		if got := w.orepo.positions(); !reflect.DeepEqual(got, want) {
			t.Errorf("origin %v, want %v", got, want)
		}
		// The answer: every role as the origin placed it, in the mirror's space, the managed
		// role still the (local) bot's.
		if got := positionsOf(roles); !reflect.DeepEqual(got, want) {
			t.Errorf("answered %v, want %v", got, want)
		}
		for _, r := range roles {
			if r.SpaceID != rpMirror {
				t.Errorf("role %d answered in space %d", r.ID, r.SpaceID)
			}
			if r.ID == rpBotRole && r.BotID != rpBotB {
				t.Errorf("the managed role's bot is %d, want %d", r.BotID, rpBotB)
			}
		}
		// The mirror learned the positions from the relays in the reply, applied in the order
		// the origin wrote them.
		if got := w.mrepo.positions(); !reflect.DeepEqual(got, want) {
			t.Errorf("mirror %v, want %v", got, want)
		}
		if !reflect.DeepEqual(w.mrepo.roleWrites, w.orepo.roleWrites) {
			t.Errorf("mirror applied %v, origin wrote %v", w.mrepo.roleWrites, w.orepo.roleWrites)
		}
		if got := w.mrepo.role(rpBotRole); got.BotID != rpBotB || got.Name != "RoleBot" {
			t.Errorf("mirrored bot role %+v", got)
		}
		// Audited at the origin as the member's shadow there; one request, no fallback.
		if len(w.orepo.audits) != 3 || w.orepo.audits[0].UserID != rpMod {
			t.Errorf("origin audits %+v", w.orepo.audits)
		}
		if !reflect.DeepEqual(*w.ops, []string{"role.positions"}) {
			t.Errorf("ops %v", *w.ops)
		}
	})

	t.Run("roles the move pushed up reach the mirror too", func(t *testing.T) {
		w := newRPWire(t)
		roles, err := w.mirror.spaceSvc.ReorderSpaceRoles(ctx, rpModB, rpMirror, []spaces.RolePosition{{RoleID: rpBotRole, Position: 3}})
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]int{rpEveryone: 0, rpVerified: 2, rpBotRole: 3, rpHelpers: 4, rpModRole: 5, rpStaff: 6}
		for name, got := range map[string]map[int64]int{"answer": positionsOf(roles), "origin": w.orepo.positions(), "mirror": w.mrepo.positions()} {
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %v, want %v", name, got, want)
			}
		}
	})

	t.Run("refusals: the origin's status, or the mirror's own; nothing written anywhere", func(t *testing.T) {
		cases := []struct {
			name  string
			actor int64
			moves []spaces.RolePosition
			// The origin's refusal, passed through with its status and reason...
			status int
			msg    string
			// ...or the one the mirrored roles already answer, with nothing sent.
			local error
		}{
			{name: "a role above the moderator", actor: rpModB, moves: []spaces.RolePosition{{RoleID: rpStaff, Position: 1}},
				status: http.StatusForbidden, msg: spaces.ErrRoleHierarchy.Error()},
			{name: "up to the moderator's rank", actor: rpModB, moves: []spaces.RolePosition{{RoleID: rpVerified, Position: 4}},
				status: http.StatusForbidden, msg: spaces.ErrRoleHierarchy.Error()},
			{name: "without Manage Roles", actor: rpMemberB, moves: []spaces.RolePosition{{RoleID: rpVerified, Position: 1}},
				status: http.StatusForbidden, msg: spaces.ErrMissingPerm.Error()},
			{name: "@everyone", actor: rpModB, moves: []spaces.RolePosition{{RoleID: rpVerified, Position: 1}, {RoleID: rpEveryone, Position: 2}},
				local: spaces.ErrCannotMoveEveryone},
			{name: "a role the space does not have", actor: rpModB, moves: []spaces.RolePosition{{RoleID: 999, Position: 1}},
				local: spaces.ErrRoleNotFound},
			{name: "not in the space", actor: rpOutsiderB, moves: []spaces.RolePosition{{RoleID: 999, Position: 1}},
				local: spaces.ErrNotMember},
		}
		for _, tc := range cases {
			w := newRPWire(t)
			_, err := w.mirror.spaceSvc.ReorderSpaceRoles(ctx, tc.actor, rpMirror, tc.moves)
			if tc.local != nil {
				if !errors.Is(err, tc.local) || len(*w.ops) != 0 {
					t.Errorf("%s: got %v having asked %v, want %v asking nothing", tc.name, err, *w.ops, tc.local)
				}
			} else if status, msg, ok := spaces.HTTPError(err); !ok || status != tc.status || msg != tc.msg {
				t.Errorf("%s: %v -> %d %q, want %d %q", tc.name, err, status, msg, tc.status, tc.msg)
			}
			if len(w.orepo.roleWrites) != 0 || len(w.mrepo.roleWrites) != 0 || len(w.orepo.audits) != 0 {
				t.Errorf("%s: written (origin %v, mirror %v, %d audits)", tc.name, w.orepo.roleWrites, w.mrepo.roleWrites, len(w.orepo.audits))
			}
		}
	})

	// PATCH /spaces/:id/roles on the mirror answers as it does on the origin.
	t.Run("through the mirror's REST handler", func(t *testing.T) {
		w := newRPWire(t)
		app := fiber.New()
		app.Patch("/spaces/:id/roles", func(c fiber.Ctx) error {
			c.Locals(auth.LocalsKeyUser, &auth.User{ID: rpModB, Username: "mod"})
			return c.Next()
		}, spaces.NewHandler(w.mirror.spaceSvc, nil).ReorderRoles)
		patch := func(body string) (int, string) {
			t.Helper()
			req := httptest.NewRequest(http.MethodPatch, "/spaces/9/roles", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			res, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)
			return res.StatusCode, string(raw)
		}
		if code, body := patch(`[{"id":"999","position":1}]`); code != http.StatusBadRequest || !strings.Contains(body, `"unknown role id"`) {
			t.Errorf("unknown role: %d %s", code, body)
		}
		if code, body := patch(`[{"id":"25","position":1}]`); code != http.StatusForbidden || !strings.Contains(body, spaces.ErrRoleHierarchy.Error()) {
			t.Errorf("above the moderator: %d %s", code, body)
		}
		code, body := patch(`[{"id":"20","position":3},{"id":"22","position":2},{"id":"21","position":1}]`)
		if code != http.StatusOK {
			t.Fatalf("reorder: %d %s", code, body)
		}
		var answer []struct {
			ID       string `json:"id"`
			Position int    `json:"position"`
		}
		if err := json.Unmarshal([]byte(body), &answer); err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, r := range answer {
			got[r.ID] = r.Position
		}
		if want := map[string]int{"10": 0, "21": 1, "22": 2, "20": 3, "24": 4, "25": 5}; !reflect.DeepEqual(got, want) {
			t.Errorf("answered %v, want %v", got, want)
		}
	})
}

// ---- an origin from before role.positions ----------------------------------------------

// olderOrigin answers /spaces/manage as an origin from before role.positions: that op with 400
// "unknown action" (what SpaceManage answers for any op it does not have), role.update by
// writing the position as given and relaying the role back. ops records what was asked.
type olderOrigin struct {
	roles  []spaces.SpaceRole
	ops    []string
	refuse map[int64]bool // role.update of these is refused: 403, the role hierarchy
	silent bool           // role.positions is accepted, with no result and no relays
}

func newOlderOrigin() *olderOrigin {
	return &olderOrigin{roles: rpRoles(rpSpace, rpBot), refuse: map[int64]bool{}}
}

func (o *olderOrigin) wire(r *spaces.SpaceRole) SpaceRoleWire {
	w := SpaceRoleWire{
		ID: id.Format(r.ID), Name: r.Name, Permissions: r.Permissions, Position: r.Position,
		Color: r.Color, Hoist: r.Hoist, Mentionable: r.Mentionable, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.BotID != 0 {
		w.Bot = FormatFID(rpBotB, "b.example")
	}
	return w
}

func (o *olderOrigin) app() *fiber.App {
	app := fiber.New()
	app.Post(BasePath+"/spaces/manage", func(c fiber.Ctx) error {
		var req SpaceManageRequest
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
		}
		if req.Op == "member.roles" {
			// Every origin has the whole-list op; this one records the list and relays nothing.
			var p manageMemberRoles
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": errBadParams.Error()})
			}
			o.ops = append(o.ops, fmt.Sprintf("member.roles %s %v", p.User, p.Roles))
			return c.JSON(SpaceManageReply{Events: []RelayEvent{}})
		}
		if req.Op != "role.update" {
			o.ops = append(o.ops, req.Op)
			if o.silent {
				return c.JSON(SpaceManageReply{Events: []RelayEvent{}})
			}
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": errUnknownAction.Error()})
		}
		var p manageRoleUpdate
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Update.Position == nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": errBadParams.Error()})
		}
		o.ops = append(o.ops, fmt.Sprintf("role.update %s->%d", p.Role, *p.Update.Position))
		rid, _ := id.Parse(p.Role)
		if o.refuse[rid] {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": spaces.ErrRoleHierarchy.Error()})
		}
		for i := range o.roles {
			if o.roles[i].ID != rid {
				continue
			}
			o.roles[i].Position = *p.Update.Position // as given: no placement
			w := o.wire(&o.roles[i])
			result, _ := json.Marshal(w)
			relay, _ := json.Marshal(SpaceRoleEvent{Space: SpaceRef{OriginDomain: "a.example", OriginSpaceID: id.Format(rpSpace)}, Action: "upsert", Role: w})
			return c.JSON(SpaceManageReply{Result: result, Events: []RelayEvent{{Method: http.MethodPost, Path: "/spaces/roles", Body: relay}}})
		}
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "role not found"})
	})
	return app
}

func olderOriginMirror(t *testing.T, o *olderOrigin) (*Service, *roleRepo) {
	t.Helper()
	return rpMirrorOf(o.app(), testSigner(t), testSigner(t))
}

func TestRemoteReorderRolesOnAnOlderOrigin(t *testing.T) {
	ctx := context.Background()

	t.Run("one role.update per move, lowest requested position first", func(t *testing.T) {
		o := newOlderOrigin()
		mirror, mrepo := olderOriginMirror(t, o)
		roles, err := mirror.spaceSvc.ReorderSpaceRoles(ctx, rpModB, rpMirror, []spaces.RolePosition{
			{RoleID: rpHelpers, Position: 2}, {RoleID: rpBotRole, Position: 3}, {RoleID: rpVerified, Position: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		wantOps := []string{"role.positions", "role.update 21->1", "role.update 22->2", "role.update 20->3"}
		if !reflect.DeepEqual(o.ops, wantOps) {
			t.Errorf("ops %v, want %v", o.ops, wantOps)
		}
		want := map[int64]int{rpEveryone: 0, rpVerified: 1, rpHelpers: 2, rpBotRole: 3, rpModRole: 4, rpStaff: 5}
		if got := mrepo.positions(); !reflect.DeepEqual(got, want) {
			t.Errorf("mirror %v, want %v", got, want)
		}
		// The answer is the mirror's roles once every relay is in, by position.
		if got := positionsOf(roles); !reflect.DeepEqual(got, want) {
			t.Errorf("answered %v, want %v", got, want)
		}
		for i := 1; i < len(roles); i++ {
			if roles[i].Position < roles[i-1].Position {
				t.Errorf("answer not by position: %+v", roles)
			}
		}
		if got := mrepo.role(rpBotRole); got.BotID != rpBotB {
			t.Errorf("mirrored bot role %+v", got)
		}
	})

	t.Run("what the mirror can see is refused before anything is sent", func(t *testing.T) {
		cases := []struct {
			name  string
			moves []spaces.RolePosition
			want  error
		}{
			{"@everyone", []spaces.RolePosition{{RoleID: rpVerified, Position: 1}, {RoleID: rpEveryone, Position: 2}}, spaces.ErrCannotMoveEveryone},
			{"a role the space does not have", []spaces.RolePosition{{RoleID: rpVerified, Position: 1}, {RoleID: 999, Position: 2}}, spaces.ErrRoleNotFound},
		}
		for _, tc := range cases {
			o := newOlderOrigin()
			mirror, mrepo := olderOriginMirror(t, o)
			if _, err := mirror.spaceSvc.ReorderSpaceRoles(ctx, rpModB, rpMirror, tc.moves); !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
			}
			if len(o.ops) != 0 || len(mrepo.roleWrites) != 0 {
				t.Errorf("%s: ops %v, mirror writes %v", tc.name, o.ops, mrepo.roleWrites)
			}
		}
	})

	t.Run("a refusal part-way stops there, the moves before it applied", func(t *testing.T) {
		o := newOlderOrigin()
		o.refuse[rpHelpers] = true
		mirror, mrepo := olderOriginMirror(t, o)
		_, err := mirror.spaceSvc.ReorderSpaceRoles(ctx, rpModB, rpMirror, []spaces.RolePosition{
			{RoleID: rpVerified, Position: 1}, {RoleID: rpHelpers, Position: 2}, {RoleID: rpBotRole, Position: 3},
		})
		if status, msg, _ := spaces.HTTPError(err); status != http.StatusForbidden || msg != spaces.ErrRoleHierarchy.Error() {
			t.Errorf("got %v (%d %q)", err, status, msg)
		}
		wantOps := []string{"role.positions", "role.update 21->1", "role.update 22->2"}
		if !reflect.DeepEqual(o.ops, wantOps) {
			t.Errorf("ops %v, want %v", o.ops, wantOps)
		}
		// Verified moved and was relayed; nothing after the refusal was sent. The older origin
		// now has Verified and the bot's role sharing position 1, and so does the mirror.
		want := map[int64]int{rpEveryone: 0, rpBotRole: 1, rpVerified: 1, rpHelpers: 3, rpModRole: 4, rpStaff: 5}
		if got := mrepo.positions(); !reflect.DeepEqual(got, want) {
			t.Errorf("mirror %v, want %v", got, want)
		}
	})

	t.Run("an accepted reorder with no list: the mirror's own roles", func(t *testing.T) {
		o := newOlderOrigin()
		o.silent = true
		mirror, _ := olderOriginMirror(t, o)
		roles, err := mirror.spaceSvc.ReorderSpaceRoles(ctx, rpModB, rpMirror, []spaces.RolePosition{{RoleID: rpVerified, Position: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if len(roles) != 6 {
			t.Errorf("answered %d roles, want the 6 the mirror has", len(roles))
		}
	})
}

func TestIsUnknownAction(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&spaces.OriginError{Status: http.StatusBadRequest, Message: "unknown action"}, true},
		{fmt.Errorf("manage: %w", &spaces.OriginError{Status: http.StatusBadRequest, Message: "unknown action"}), true},
		{&spaces.OriginError{Status: http.StatusBadRequest, Message: "cannot move @everyone"}, false},
		{&spaces.OriginError{Status: http.StatusBadRequest, Message: "invalid parameters"}, false},
		{&spaces.OriginError{Status: http.StatusNotFound, Message: "unknown action"}, false},
		{spaces.ErrOriginUnavailable, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isUnknownAction(tc.err); got != tc.want {
			t.Errorf("isUnknownAction(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// ---- member roles: the role decides, not the member ------------------------------------

func TestManageAtMemberRolesByTheRoleAlone(t *testing.T) {
	grant := func(t *testing.T, s *Service, target string, roles ...int64) error {
		t.Helper()
		p := manageMemberRoles{User: target, Roles: []string{}}
		for _, rid := range roles {
			p.Roles = append(p.Roles, id.Format(rid))
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		ctx, _ := WithRelayCapture(context.Background(), "b.example")
		_, err = s.manageAt(ctx, &spaces.Space{ID: rpSpace}, s.users.(*profileTestUsers).byID[rpMod], "member.roles", raw)
		return err
	}
	has := func(m *spaces.SpaceMember, roleID int64) bool {
		for _, rid := range m.RoleIDs {
			if rid == roleID {
				return true
			}
		}
		return false
	}

	t.Run("a moderator elsewhere gives the space owner a role below their own", func(t *testing.T) {
		s, repo := rpOrigin()
		if err := grant(t, s, "@1:a.example", rpVerified); err != nil {
			t.Fatal(err)
		}
		if !has(repo.members[rpOwner], rpVerified) {
			t.Errorf("owner's roles %v", repo.members[rpOwner].RoleIDs)
		}
	})

	t.Run("and a member who outranks them, whose higher role stays", func(t *testing.T) {
		s, repo := rpOrigin()
		if err := grant(t, s, "@8:a.example", rpHelpers); err != nil {
			t.Fatal(err)
		}
		if m := repo.members[rpHigh]; !has(m, rpHelpers) || !has(m, rpStaff) {
			t.Errorf("roles %v, want Helpers added and Staff carried over", m.RoleIDs)
		}
	})

	t.Run("a role at or above their own, or a bot's, is still refused", func(t *testing.T) {
		for _, tc := range []struct {
			role int64
			want error
		}{
			{rpStaff, spaces.ErrRoleHierarchy},
			{rpModRole, spaces.ErrRoleHierarchy},
			{rpBotRole, spaces.ErrManagedRole},
		} {
			s, repo := rpOrigin()
			if err := grant(t, s, "@50:b.example", tc.role); !errors.Is(err, tc.want) {
				t.Errorf("role %d: got %v, want %v", tc.role, err, tc.want)
			}
			if repo.memberWrites != 0 {
				t.Errorf("role %d: a refusal wrote", tc.role)
			}
		}
	})
}

// ---- member.role: one role, one set operation ------------------------------------------

// The wire shape FEDERATION.md documents for member.role.
func TestMemberRoleParamsWire(t *testing.T) {
	add := true
	raw, err := json.Marshal(manageMemberRole{User: "@50:b.example", Role: "21", Add: &add})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"user":"@50:b.example","role":"21","add":true}`; got != want {
		t.Errorf("wire %s, want %s", got, want)
	}
}

// memberRoleAt runs member.role on the origin as actor, as SpaceManage does for a request from
// b.example, and returns the relays captured for it.
func memberRoleAt(t *testing.T, s *Service, actor int64, params string) ([]RelayEvent, error) {
	t.Helper()
	ctx, capture := WithRelayCapture(context.Background(), "b.example")
	var raw json.RawMessage
	if params != "" {
		raw = json.RawMessage(params)
	}
	_, err := s.manageAt(ctx, &spaces.Space{ID: rpSpace}, s.users.(*profileTestUsers).byID[actor], "member.role", raw)
	return capture.take(), err
}

func TestManageAtMemberRole(t *testing.T) {
	t.Run("add and remove, each one set operation, each relayed", func(t *testing.T) {
		s, repo := rpOrigin()
		relays, err := memberRoleAt(t, s, rpMod, `{"user":"@50:b.example","role":"21","add":true}`)
		if err != nil {
			t.Fatal(err)
		}
		if !repo.holds(rpMember, rpVerified) {
			t.Errorf("roles %v", repo.members[rpMember].RoleIDs)
		}
		if len(relays) != 1 || relays[0].Path != "/spaces/members" {
			t.Fatalf("relays %+v", relays)
		}
		var ev SpaceMemberEvent
		if err := json.Unmarshal(relays[0].Body, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Action != "update" || ev.Member.User.FID != "@50:b.example" || !reflect.DeepEqual(ev.Member.Roles, []string{"10", "21"}) {
			t.Errorf("relay %+v", ev)
		}
		if _, err := memberRoleAt(t, s, rpMod, `{"user":"@50:b.example","role":"21","add":false}`); err != nil {
			t.Fatal(err)
		}
		if repo.holds(rpMember, rpVerified) {
			t.Errorf("still holds Verified: %v", repo.members[rpMember].RoleIDs)
		}
		if want := []string{"add:5:21", "remove:5:21"}; !reflect.DeepEqual(repo.setOps, want) || repo.memberWrites != 0 {
			t.Errorf("set ops %v (want %v), whole lists %d", repo.setOps, want, repo.memberWrites)
		}
		if len(repo.audits) != 2 || repo.audits[0].UserID != rpMod {
			t.Errorf("audits %+v", repo.audits)
		}
	})

	t.Run("the owner and members above the actor too: the role decides", func(t *testing.T) {
		s, repo := rpOrigin()
		for _, target := range []string{"@1:a.example", "@8:a.example"} {
			if _, err := memberRoleAt(t, s, rpMod, `{"user":"`+target+`","role":"22","add":true}`); err != nil {
				t.Errorf("%s: %v", target, err)
			}
		}
		if !repo.holds(rpOwner, rpHelpers) || !repo.holds(rpHigh, rpHelpers) || !repo.holds(rpHigh, rpStaff) {
			t.Errorf("owner %v, staff %v", repo.members[rpOwner].RoleIDs, repo.members[rpHigh].RoleIDs)
		}
	})

	t.Run("refusals, nothing written", func(t *testing.T) {
		cases := []struct {
			name   string
			actor  int64
			params string
			want   error
		}{
			{"a role at their own rank", rpMod, `{"user":"@50:b.example","role":"24","add":true}`, spaces.ErrRoleHierarchy},
			{"a role above them", rpMod, `{"user":"@50:b.example","role":"25","add":true}`, spaces.ErrRoleHierarchy},
			{"taking away a role above them", rpMod, `{"user":"@8:a.example","role":"25","add":false}`, spaces.ErrRoleHierarchy},
			{"a bot's role", rpMod, `{"user":"@50:b.example","role":"20","add":true}`, spaces.ErrManagedRole},
			{"@everyone", rpMod, `{"user":"@50:b.example","role":"10","add":false}`, spaces.ErrEveryoneRole},
			{"a role the space does not have", rpMod, `{"user":"@50:b.example","role":"999","add":true}`, spaces.ErrRoleNotFound},
			{"without Manage Roles", rpMember, `{"user":"@50:b.example","role":"21","add":true}`, spaces.ErrMissingPerm},
			{"no add", rpMod, `{"user":"@50:b.example","role":"21"}`, errBadParams},
			{"a malformed role id", rpMod, `{"user":"@50:b.example","role":"x","add":true}`, errBadParams},
			{"no params", rpMod, ``, errBadParams},
			{"a user nobody knows", rpMod, `{"user":"@99:c.example","role":"21","add":true}`, ErrRemoteUserNotFound},
		}
		for _, tc := range cases {
			s, repo := rpOrigin()
			if _, err := memberRoleAt(t, s, tc.actor, tc.params); !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
			}
			if len(repo.setOps) != 0 || repo.memberWrites != 0 || len(repo.audits) != 0 {
				t.Errorf("%s: wrote %v / %d, audited %+v", tc.name, repo.setOps, repo.memberWrites, repo.audits)
			}
		}
	})
}

func TestMemberRoleOverTheWire(t *testing.T) {
	ctx := context.Background()

	t.Run("a moderator on the mirror gives and takes one role", func(t *testing.T) {
		w := newRPWire(t)
		if err := w.mirror.spaceSvc.AddMemberRole(ctx, rpModB, rpMirror, rpMemberB, rpVerified); err != nil {
			t.Fatal(err)
		}
		if !w.orepo.holds(rpMember, rpVerified) {
			t.Errorf("origin: %v", w.orepo.members[rpMember].RoleIDs)
		}
		// The relay in the reply brought the mirror's row along.
		if !w.mrepo.holds(rpMemberB, rpVerified) {
			t.Errorf("mirror: %v", w.mrepo.members[rpMemberB].RoleIDs)
		}
		if err := w.mirror.spaceSvc.RemoveMemberRole(ctx, rpModB, rpMirror, rpMemberB, rpVerified); err != nil {
			t.Fatal(err)
		}
		if w.orepo.holds(rpMember, rpVerified) || w.mrepo.holds(rpMemberB, rpVerified) {
			t.Errorf("origin %v, mirror %v", w.orepo.members[rpMember].RoleIDs, w.mrepo.members[rpMemberB].RoleIDs)
		}
		if want := []string{"member.role", "member.role"}; !reflect.DeepEqual(*w.ops, want) {
			t.Errorf("ops %v, want %v", *w.ops, want)
		}
		// One set operation each at the origin, never a list read and written back.
		if want := []string{"add:5:21", "remove:5:21"}; !reflect.DeepEqual(w.orepo.setOps, want) || w.orepo.memberWrites != 0 {
			t.Errorf("origin set ops %v, whole lists %d", w.orepo.setOps, w.orepo.memberWrites)
		}
	})

	t.Run("a refusal only the origin can see is the answer, not retried", func(t *testing.T) {
		w := newRPWire(t)
		// The origin has taken the moderator's role away; the mirror has not heard yet.
		w.orepo.members[rpMod].RoleIDs = []int64{rpEveryone}
		err := w.mirror.spaceSvc.AddMemberRole(ctx, rpModB, rpMirror, rpMemberB, rpVerified)
		if status, msg, ok := spaces.HTTPError(err); !ok || status != http.StatusForbidden || msg != spaces.ErrMissingPerm.Error() {
			t.Errorf("got %v (%d %q)", err, status, msg)
		}
		if !reflect.DeepEqual(*w.ops, []string{"member.role"}) {
			t.Errorf("ops %v", *w.ops)
		}
	})

	t.Run("an origin from before member.role gets the whole list", func(t *testing.T) {
		o := newOlderOrigin()
		mirror, mrepo := olderOriginMirror(t, o)
		mrepo.members[rpMemberB].RoleIDs = []int64{rpEveryone, rpHelpers}
		if err := mirror.spaceSvc.AddMemberRole(ctx, rpModB, rpMirror, rpMemberB, rpVerified); err != nil {
			t.Fatal(err)
		}
		if err := mirror.spaceSvc.RemoveMemberRole(ctx, rpModB, rpMirror, rpMemberB, rpHelpers); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"member.role", "member.roles @50:b.example [22 21]",
			"member.role", "member.roles @50:b.example []",
		}
		if !reflect.DeepEqual(o.ops, want) {
			t.Errorf("ops %v, want %v", o.ops, want)
		}
	})

	t.Run("the error the fallback keys on", func(t *testing.T) {
		o := newOlderOrigin()
		mirror, _ := olderOriginMirror(t, o)
		err := mirror.RemoteChangeMemberRole(ctx, "a.example", rpMirror, &auth.User{ID: rpModB, Username: "mod"}, rpMemberB, rpVerified, true)
		if !errors.Is(err, spaces.ErrOriginLacksOperation) {
			t.Errorf("got %v, want %v", err, spaces.ErrOriginLacksOperation)
		}
	})
}
