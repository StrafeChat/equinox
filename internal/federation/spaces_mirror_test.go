package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// A mirror's rooms and settings on the wire, and what happens when the origin stops knowing
// about a mirror. A wire change here has to work with a peer on either side of it: an older
// origin says nothing about a new field, and that must leave the mirror's value alone.

// mirrorFedRepo answers the room lookups of a mirror whose local room ids are the origin's
// plus 1000, and records what is dropped.
type mirrorFedRepo struct {
	roleFedRepo
	droppedSpace, droppedPeers bool
}

func (r *mirrorFedRepo) GetRoomByOrigin(_ context.Context, domain string, originRoomID int64) (*RoomMapping, error) {
	return &RoomMapping{RoomID: originRoomID + 1000, OriginDomain: domain, OriginRoomID: originRoomID}, nil
}

func (r *mirrorFedRepo) GetRoomMapping(_ context.Context, roomID int64) (*RoomMapping, error) {
	return &RoomMapping{RoomID: roomID, OriginDomain: "a.example", OriginRoomID: roomID - 1000}, nil
}

func (r *mirrorFedRepo) GetRoomMappings(context.Context, []int64) (map[int64]*RoomMapping, error) {
	return nil, nil
}

func (r *mirrorFedRepo) DeleteSpacePeers(context.Context, int64) error {
	r.droppedPeers = true
	return nil
}

func (r *mirrorFedRepo) AddSpacePeer(context.Context, int64, string) error { return nil }

func (r *mirrorFedRepo) DeleteSpaceMapping(context.Context, *SpaceMapping) error {
	r.droppedSpace = true
	return nil
}

func mirrorWireFixture() (*Service, *mirrorFedRepo) {
	repo := &mirrorFedRepo{roleFedRepo: roleFedRepo{mapping: &SpaceMapping{SpaceID: rpMirror, OriginDomain: "a.example", OriginSpaceID: rpSpace}}}
	return &Service{
		fcfg:  config.FederationConfig{Enabled: true, Domain: "b.example"},
		users: &profileTestUsers{byID: map[int64]*auth.User{}},
		repo:  repo,
	}, repo
}

func TestPermissionsSyncedOnTheWire(t *testing.T) {
	ctx := context.Background()
	s, _ := mirrorWireFixture()
	synced, unsynced := true, false
	cases := []struct {
		name string
		flag *bool
		want bool
	}{
		{"synced", &synced, true},
		// False has to reach a mirror too: that is how an unsync gets there.
		{"unsynced", &unsynced, false},
		{"never set", nil, false},
	}
	for _, tc := range cases {
		w := s.roomWire(ctx, &rooms.Room{ID: 5, Type: rooms.TypeSpaceText, PermissionsSynced: tc.flag}, spaces.RoomOverrides{})
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		var got SpaceRoomWire
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		mr, err := s.mirrorRoom(ctx, "a.example", got, false)
		if err != nil {
			t.Fatal(err)
		}
		if mr.Room.PermissionsSynced == nil || *mr.Room.PermissionsSynced != tc.want {
			t.Errorf("%s: mirror read %v from %s", tc.name, mr.Room.PermissionsSynced, raw)
		}
	}

	// An origin that predates the field: nil on a mirror, which means leave it alone.
	var old SpaceRoomWire
	if err := json.Unmarshal([]byte(`{"id":"5","type":3,"name":"general"}`), &old); err != nil {
		t.Fatal(err)
	}
	mr, err := s.mirrorRoom(ctx, "a.example", old, false)
	if err != nil {
		t.Fatal(err)
	}
	if mr.Room.PermissionsSynced != nil {
		t.Errorf("an older origin read as %v", *mr.Room.PermissionsSynced)
	}
}

func TestSpaceInfoNewFieldsOnTheWire(t *testing.T) {
	ctx := context.Background()
	s, _ := mirrorWireFixture()
	room := int64(44)
	sp := &spaces.Space{ID: rpSpace, Name: "s", AutomodFlags: 3, AutomodMentionLimit: 7, BirthdayChannelID: &room, BirthdayMessage: "hello"}
	raw, err := json.Marshal(s.spaceInfo(ctx, sp))
	if err != nil {
		t.Fatal(err)
	}
	var info SpaceInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	got := &spaces.Space{ID: rpMirror}
	s.applySpaceInfo(ctx, got, "a.example", info)
	if got.AutomodFlags != 3 || got.AutomodMentionLimit != 7 || got.BirthdayMessage != "hello" {
		t.Errorf("applied %+v", got)
	}
	if got.BirthdayChannelID == nil || *got.BirthdayChannelID != 1044 {
		t.Errorf("birthday room %v, want the local id for 44", got.BirthdayChannelID)
	}

	// Zero is a value: an origin that turned automod off must turn it off here.
	off := &spaces.Space{ID: rpSpace, Name: "s"}
	raw, _ = json.Marshal(s.spaceInfo(ctx, off))
	var cleared SpaceInfo
	if err := json.Unmarshal(raw, &cleared); err != nil {
		t.Fatal(err)
	}
	s.applySpaceInfo(ctx, got, "a.example", cleared)
	if got.AutomodFlags != 0 || got.AutomodMentionLimit != 0 || got.BirthdayMessage != "" || got.BirthdayChannelID != nil {
		t.Errorf("not cleared: %+v", got)
	}

	// An origin that predates the fields says nothing, which changes nothing.
	kept := &spaces.Space{ID: rpMirror, AutomodFlags: 3, AutomodMentionLimit: 7, BirthdayChannelID: &room, BirthdayMessage: "hello"}
	var old SpaceInfo
	if err := json.Unmarshal([]byte(`{"name":"s","everyone_role":"10"}`), &old); err != nil {
		t.Fatal(err)
	}
	s.applySpaceInfo(ctx, kept, "a.example", old)
	if kept.AutomodFlags != 3 || kept.AutomodMentionLimit != 7 || kept.BirthdayMessage != "hello" || kept.BirthdayChannelID == nil {
		t.Errorf("an older origin cleared %+v", kept)
	}
}

func TestApplySpaceInfoNeverTakesARoleOfZero(t *testing.T) {
	s, _ := mirrorWireFixture()
	sp := &spaces.Space{ID: rpMirror, EveryoneRoleID: rpEveryone}
	s.applySpaceInfo(context.Background(), sp, "a.example", SpaceInfo{Name: "s", EveryoneRole: "0"})
	if sp.EveryoneRoleID != rpEveryone {
		t.Errorf("everyone role became %d: every @everyone deny would stop matching", sp.EveryoneRoleID)
	}
}

func TestOriginForgot(t *testing.T) {
	notPeer := `{"error":"` + ErrPeerNotAllowed.Error() + `"}`
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"gone", &StatusError{Status: http.StatusNotFound, Body: `{"error":"space not found"}`}, true},
		{"no longer a peer", &StatusError{Status: http.StatusForbidden, Body: notPeer}, true},
		// The instance as a whole is refused, which can be lifted: nothing here is orphaned.
		{"instance policy", &StatusError{Status: http.StatusForbidden, Body: `{"error":"instance not allowed"}`}, false},
		{"another refusal", &StatusError{Status: http.StatusForbidden, Body: `{"error":"not a member of this space"}`}, false},
		{"origin failing", &StatusError{Status: http.StatusInternalServerError, Body: notPeer}, false},
		{"unreachable", errors.New("dial tcp: refused"), false},
		// Our own policy refusing to call the peer is not the peer saying anything.
		{"refused here", ErrPeerNotAllowed, false},
	}
	for _, tc := range cases {
		if got := originForgot(tc.err); got != tc.want {
			t.Errorf("%s: originForgot = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// syncApp is an origin answering POST /spaces/sync as the handler says.
func syncApp(reply func(c fiber.Ctx) error) *fiber.App {
	app := fiber.New()
	app.Post(BasePath+"/spaces/sync", reply)
	return app
}

// resyncFixture is b.example's mirror with its signed client reaching app; roleRepo holds the
// space row unless noRow, for a mapping whose row is gone.
func resyncFixture(t *testing.T, app *fiber.App, noRow bool) (*Service, *mirrorFedRepo) {
	t.Helper()
	s, repo := mirrorWireFixture()
	key, originKey := testSigner(t), testSigner(t)
	s.client = &Client{
		cfg: s.fcfg, signer: key, discovery: knownPeer(s.fcfg, "a.example", originKey),
		http: &http.Client{Transport: appTransport{app}},
	}
	s.roomRepo = emptyRooms{}
	store := newRoleRepo(&spaces.Space{ID: rpMirror, OwnerID: rpOwnerB, EveryoneRoleID: rpEveryone}, nil, nil)
	if noRow {
		store.space = nil
	}
	s.spaceSvc = spaces.NewService(store, emptyRooms{}, s.users, nil, nil)
	return s, repo
}

// emptyRooms is a room repository holding nothing.
type emptyRooms struct{ rooms.Repository }

func (emptyRooms) ListBySpace(context.Context, int64) ([]rooms.RoomBySpaceRow, error) {
	return nil, nil
}

func TestResyncMirrorDropsWhatTheOriginForgot(t *testing.T) {
	ctx := context.Background()
	mapping := func() *SpaceMapping {
		return &SpaceMapping{SpaceID: rpMirror, OriginDomain: "a.example", OriginSpaceID: rpSpace}
	}
	cases := []struct {
		name    string
		status  int
		body    string
		dropped bool
		wantErr bool
	}{
		{"space gone", http.StatusNotFound, `{"error":"space not found"}`, true, false},
		{"no longer a peer", http.StatusForbidden, `{"error":"` + ErrPeerNotAllowed.Error() + `"}`, true, false},
		{"this instance refused", http.StatusForbidden, `{"error":"instance not allowed"}`, false, true},
		{"origin failing", http.StatusInternalServerError, `{"error":"internal error"}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := syncApp(func(c fiber.Ctx) error {
				c.Set("Content-Type", "application/json")
				return c.Status(tc.status).SendString(tc.body)
			})
			s, repo := resyncFixture(t, app, true)
			err := s.ResyncMirror(ctx, mapping())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if repo.droppedSpace != tc.dropped {
				t.Errorf("mapping dropped = %v, want %v", repo.droppedSpace, tc.dropped)
			}
		})
	}

	// The other half of a takedown from before mirrors refused them: the origin still has the
	// space and this instance has lost its row. Nothing can ever be applied to it again, and
	// the stale mapping stops anyone here rejoining.
	t.Run("mapping with no row", func(t *testing.T) {
		app := syncApp(func(c fiber.Ctx) error {
			return c.JSON(SpaceSnapshot{Space: SpaceRef{OriginDomain: "a.example", OriginSpaceID: "100"}, Info: SpaceInfo{Name: "s", EveryoneRole: "10"}})
		})
		s, repo := resyncFixture(t, app, true)
		if err := s.ResyncMirror(ctx, mapping()); err != nil {
			t.Fatal(err)
		}
		if !repo.droppedSpace {
			t.Error("the mapping stayed")
		}
	})
}

// hostedAt is a.example's handler for hostedSpace's callers, with the peer signature check
// done: requests come from b.example.
func hostedAt(s *Service) *fiber.App {
	app := fiber.New()
	app.Post("/sync", func(c fiber.Ctx) error {
		c.Locals(localsKeyDomain, "b.example")
		return c.Next()
	}, NewHandler(s).SpaceSync)
	return app
}

func TestHostedSpaceLooksForTheSpaceFirst(t *testing.T) {
	s, repo := rpOrigin()
	s.repo = &roleFedRepo{} // b.example is nobody's peer: forgetSpace dropped the rows
	body := func(spaceID string) *http.Request {
		raw, _ := json.Marshal(SpaceSyncRequest{Space: SpaceRef{OriginDomain: "a.example", OriginSpaceID: spaceID}})
		req, _ := http.NewRequest(http.MethodPost, "/sync", strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	repo.space = nil
	res, err := hostedAt(s).Test(body("100"))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a deleted space answers %d: a mirror that missed the delete would keep it for good", res.StatusCode)
	}
	repo.space = &spaces.Space{ID: rpSpace}
	res, err = hostedAt(s).Test(body("100"))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a space this peer does not mirror answers %d", res.StatusCode)
	}
}

func TestRemotePatchSpaceCarriesEverySetting(t *testing.T) {
	var got manageSpacePatch
	var op string
	app := fiber.New()
	app.Post(BasePath+"/spaces/manage", func(c fiber.Ctx) error {
		var req SpaceManageRequest
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return err
		}
		op = req.Op
		if err := json.Unmarshal(req.Params, &got); err != nil {
			return err
		}
		return c.JSON(SpaceManageReply{Events: []RelayEvent{}})
	})
	s, _ := resyncFixture(t, app, false)
	level, flags, limit, message, room := 2, 3, 7, "happy", "1044"
	in := &spaces.PatchSpaceInput{VerificationLevel: &level, AutomodFlags: &flags, AutomodMentionLimit: &limit, BirthdayChannelID: &room, BirthdayMessage: &message}
	if _, err := s.RemotePatchSpace(context.Background(), "a.example", rpMirror, &auth.User{ID: 40}, in); err != nil {
		t.Fatal(err)
	}
	if op != "space.patch" {
		t.Fatalf("op %q", op)
	}
	if got.VerificationLevel == nil || *got.VerificationLevel != 2 || got.AutomodFlags == nil || *got.AutomodFlags != 3 ||
		got.AutomodMentionLimit == nil || *got.AutomodMentionLimit != 7 || got.BirthdayMessage == nil || *got.BirthdayMessage != "happy" {
		t.Errorf("params %+v", got)
	}
	if got.BirthdayRoom == nil || *got.BirthdayRoom != "44" {
		t.Errorf("birthday room %v, want the origin's id", got.BirthdayRoom)
	}
}

// patchRepo is roleRepo recording the columns a settings patch writes.
type patchRepo struct {
	*roleRepo
	set map[string]interface{}
}

func (r *patchRepo) UpdateSpaceFields(_ context.Context, _ int64, set map[string]interface{}) error {
	r.set = set
	return nil
}

func TestManageSpacePatchAppliesEverySetting(t *testing.T) {
	s, repo := rpOrigin()
	store := &patchRepo{roleRepo: repo}
	s.spaceSvc = spaces.NewService(store, nil, s.users, nil, nil)
	s.spaceSvc.SetFederator(s)
	params := json.RawMessage(`{"verification_level":2,"automod_flags":3,"automod_mention_limit":7,"birthday_message":"happy"}`)
	owner := s.users.(*profileTestUsers).byID[rpOwner]
	ctx, _ := WithRelayCapture(context.Background(), "b.example")
	if _, err := s.manageAt(ctx, &spaces.Space{ID: rpSpace}, owner, "space.patch", params); err != nil {
		t.Fatal(err)
	}
	for col, want := range map[string]any{"verification_level": 2, "automod_flags": 3, "automod_mention_limit": 7, "birthday_message": "happy"} {
		if store.set[col] != want {
			t.Errorf("%s = %v, want %v", col, store.set[col], want)
		}
	}
}

func TestBirthdayGreetingOp(t *testing.T) {
	ctx := context.Background()
	origin, _ := rpOrigin()
	mirrorKey, originKey := testSigner(t), testSigner(t)
	var ops []string
	app := rpOriginApp(origin, mirrorKey, &ops)
	mirror, _ := rpMirrorOf(app, mirrorKey, originKey)
	member := &auth.User{ID: rpMemberB, Username: "member"}
	if err := mirror.RemoteBirthdayGreeting(ctx, "a.example", rpMirror, member); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0] != "member.birthday" {
		t.Errorf("ops %v", ops)
	}
	// Someone who is not in the space is refused like any other management call.
	outsider := &auth.User{ID: rpOutsiderB, Username: "outsider"}
	mirror.users.(*profileTestUsers).byID[rpOutsiderB] = outsider
	err := mirror.RemoteBirthdayGreeting(ctx, "a.example", rpMirror, outsider)
	var refused *spaces.OriginError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v", err)
	}
}
