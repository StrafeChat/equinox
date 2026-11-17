package federation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/redistest"
)

// Account deletion across instances, seen from b.example: a.example deleted one of its
// accounts (POST /users/delete, or a `deleted` profile in a message), c.example is a third
// instance. A tombstone keeps the shadow's row and users_by_remote mapping, never comes
// back to life, and is only made on the word of the account's own home.

const (
	adShadow   = 500 // b.example's shadow of a.example's 90
	adOrigin   = 90
	adBob      = 501 // local
	adCarol    = 502 // local
	adThird    = 503 // b.example's shadow of c.example's 70
	adGroup    = 600 // group PM hosted on a.example: shadow + bob + carol
	adPM       = 601 // 1:1 PM: shadow + bob
	adLocalDel = 777 // a b.example account that was deleted here (no row)
)

// adUsers is a user table with shadows, the tombstone write, and new shadow rows.
type adUsers struct {
	auth.UserRepository
	byID       map[int64]*auth.User
	tombstoned []int64
	upserts    []*auth.User
}

func (u *adUsers) GetByID(_ context.Context, uid int64) (*auth.User, error) {
	if x := u.byID[uid]; x != nil {
		cp := *x
		return &cp, nil
	}
	return nil, nil
}

func (u *adUsers) GetByIDs(ctx context.Context, ids []int64) ([]*auth.User, error) {
	out := make([]*auth.User, len(ids))
	for i, uid := range ids {
		out[i], _ = u.GetByID(ctx, uid)
	}
	return out, nil
}

func (u *adUsers) GetByRemote(_ context.Context, domain string, remoteID int64) (*auth.User, error) {
	for _, x := range u.byID {
		if x.IsRemote() && x.HomeDomain == domain && *x.RemoteID == remoteID {
			cp := *x
			return &cp, nil
		}
	}
	return nil, nil
}

func (u *adUsers) UpsertShadow(_ context.Context, x *auth.User) error {
	cp := *x
	u.byID[x.ID] = &cp
	u.upserts = append(u.upserts, &cp)
	return nil
}

func (u *adUsers) TombstoneShadow(_ context.Context, uid int64) error {
	x := u.byID[uid]
	if x == nil {
		return nil
	}
	now := time.Now().UTC()
	x.Username, x.DisplayName, x.Avatar, x.Bio = "", "", "", ""
	x.Relationships, x.Spaces = nil, nil
	x.DeletedAt = &now
	u.tombstoned = append(u.tombstoned, uid)
	return nil
}

// adRooms is a room repository over participant lists.
type adRooms struct {
	rooms.Repository
	byID         map[int64]*rooms.Room
	participants map[int64][]int64
}

func (r *adRooms) GetByID(_ context.Context, roomID int64) (*rooms.Room, error) {
	return r.byID[roomID], nil
}

func (r *adRooms) GetByIDs(_ context.Context, ids []int64) ([]*rooms.Room, error) {
	out := make([]*rooms.Room, len(ids))
	for i, rid := range ids {
		out[i] = r.byID[rid]
	}
	return out, nil
}

func (r *adRooms) GetParticipants(_ context.Context, roomID int64) ([]int64, error) {
	return append([]int64(nil), r.participants[roomID]...), nil
}

func (r *adRooms) ListByUser(_ context.Context, userID int64) ([]rooms.RoomRow, error) {
	var out []rooms.RoomRow
	for rid, ps := range r.participants {
		for _, p := range ps {
			if p == userID {
				out = append(out, rooms.RoomRow{UserID: userID, RoomID: rid})
			}
		}
	}
	return out, nil
}

func (r *adRooms) RemoveParticipant(_ context.Context, roomID, userID int64) error {
	var keep []int64
	for _, p := range r.participants[roomID] {
		if p != userID {
			keep = append(keep, p)
		}
	}
	r.participants[roomID] = keep
	return nil
}

// adFedRepo maps every room to a.example (the group is hosted there) and no message, and
// records what the durable outbox was given.
type adFedRepo struct {
	Repository
	outbox []OutboxEntry
}

func (r *adFedRepo) PutOutbox(_ context.Context, e *OutboxEntry, _ time.Duration) error {
	r.outbox = append(r.outbox, *e)
	return nil
}

func (*adFedRepo) GetRoomMapping(_ context.Context, roomID int64) (*RoomMapping, error) {
	return &RoomMapping{RoomID: roomID, OriginDomain: "a.example", OriginRoomID: roomID}, nil
}

func (*adFedRepo) GetMessageMapping(context.Context, int64, int64) (*MessageMapping, error) {
	return nil, nil
}

type adWorld struct {
	s         *Service
	users     *adUsers
	rooms     *adRooms
	published *redistest.Redis
}

// adPeer is b.example. thirdParty answers c.example's GET /users/:id (nil: no client).
func adPeer(t *testing.T, thirdParty fiber.Handler) *adWorld {
	t.Helper()
	aRemote, cRemote := int64(adOrigin), int64(70)
	users := &adUsers{byID: map[int64]*auth.User{
		adShadow: {ID: adShadow, Username: "alice", DisplayName: "Alice", HomeDomain: "a.example", RemoteID: &aRemote, Relationships: []int64{adBob}},
		adBob:    {ID: adBob, Username: "bob", Relationships: []int64{adShadow}},
		adCarol:  {ID: adCarol, Username: "carol"},
		adThird:  {ID: adThird, Username: "cathy", HomeDomain: "c.example", RemoteID: &cRemote},
	}}
	off := false
	roomRepo := &adRooms{
		byID: map[int64]*rooms.Room{
			adGroup: {ID: adGroup, Type: rooms.TypeGroupPM},
			adPM:    {ID: adPM, Type: rooms.TypePM, E2EEEnabled: &off},
		},
		participants: map[int64][]int64{adGroup: {adShadow, adBob, adCarol}, adPM: {adShadow, adBob}},
	}
	rdb, published := redistest.New()
	cfg := &config.Config{}
	fcfg := config.FederationConfig{Enabled: true, Domain: "b.example"}
	cfg.Federation = fcfg
	s := &Service{cfg: cfg, fcfg: fcfg, redis: rdb, users: users, roomRepo: roomRepo, repo: &adFedRepo{}}
	s.roomSvc = rooms.NewService(roomRepo, users, rdb, cfg, nil, nil)
	if thirdParty != nil {
		app := fiber.New()
		app.Get(BasePath+"/users/:id", thirdParty)
		d := NewDiscovery(fcfg)
		d.c["c.example"] = cachedInfo{fetchedAt: time.Now(), info: &InstanceInfo{Domain: "c.example", FederationURL: "https://c.example" + BasePath}}
		s.client = &Client{cfg: fcfg, signer: testSigner(t), discovery: d, http: &http.Client{Transport: appTransport{app}}}
	}
	return &adWorld{s: s, users: users, rooms: roomRepo, published: published}
}

func (w *adWorld) events(eventType string) []redistest.Event {
	var out []redistest.Event
	for _, ev := range w.published.Published() {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

// deleteNotice posts POST /users/delete as requester.
func deleteNotice(t *testing.T, w *adWorld, requester, fid string) int {
	t.Helper()
	app := fiber.New()
	app.Post(BasePath+"/users/delete", func(c fiber.Ctx) error {
		c.Locals(localsKeyDomain, requester)
		return c.Next()
	}, NewHandler(w.s).UserDeleted)
	req := httptest.NewRequest(http.MethodPost, BasePath+"/users/delete", strings.NewReader(`{"user":"`+fid+`"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

func TestUserDeletedSpeaksOnlyForItsOwnAccounts(t *testing.T) {
	w := adPeer(t, nil)
	for _, tc := range []struct {
		requester, fid string
	}{
		{"c.example", "@90:a.example"},  // a third instance about a.example's account
		{"a.example", "@501:b.example"}, // a peer about one of ours
	} {
		if got := deleteNotice(t, w, tc.requester, tc.fid); got != http.StatusForbidden {
			t.Errorf("%s about %s: %d, want 403", tc.requester, tc.fid, got)
		}
	}
	if got := deleteNotice(t, w, "a.example", "not a fid"); got != http.StatusBadRequest {
		t.Errorf("bad fid: %d", got)
	}
	if len(w.users.tombstoned) != 0 {
		t.Fatalf("tombstoned %v on a refused notice", w.users.tombstoned)
	}
}

func TestUserDeletedForgetsTheShadow(t *testing.T) {
	w := adPeer(t, nil)
	if got := deleteNotice(t, w, "a.example", "@90:a.example"); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	shadow := w.users.byID[adShadow]
	if !shadow.IsDeleted() || shadow.Username != "" || len(w.users.tombstoned) != 1 {
		t.Fatalf("shadow %+v, tombstoned %v", shadow, w.users.tombstoned)
	}
	// users_by_remote still maps it: the same FID finds the tombstone, never a fresh row.
	if got, _ := w.users.GetByRemote(context.Background(), "a.example", adOrigin); got == nil || got.ID != adShadow {
		t.Fatalf("mapping lost: %+v", got)
	}
	// The group (hosted on a.example) loses it locally and its local members rotate keys;
	// the 1:1 PM keeps it as its other participant.
	if ps := w.rooms.participants[adGroup]; len(ps) != 2 || ps[0] != adBob || ps[1] != adCarol {
		t.Errorf("group participants %v", ps)
	}
	if ps := w.rooms.participants[adPM]; len(ps) != 2 {
		t.Errorf("1:1 PM participants %v, want the tombstone kept", ps)
	}
	rotated := map[string]bool{}
	for _, ev := range w.events("SESSION_ROTATE") {
		rotated[ev.UserID] = true
	}
	if !rotated[id.Format(adBob)] || !rotated[id.Format(adCarol)] {
		t.Errorf("SESSION_ROTATE to %v", rotated)
	}
	// Local clients see the placeholder.
	sawDeleted := false
	for _, ev := range w.events("USER_UPDATE") {
		if ev.Data["deleted"] == true && ev.Data["user_id"] == id.Format(adShadow) && ev.Data["username"] == "" {
			sawDeleted = true
		}
	}
	if !sawDeleted {
		t.Errorf("no USER_UPDATE deleted: %+v", w.published.Published())
	}

	// Idempotent: a second notice (an outbox retry) changes nothing.
	before := len(w.published.Published())
	if got := deleteNotice(t, w, "a.example", "@90:a.example"); got != http.StatusNoContent {
		t.Fatalf("second notice %d", got)
	}
	if len(w.users.tombstoned) != 1 || len(w.published.Published()) != before {
		t.Errorf("second notice did work again: tombstoned %v", w.users.tombstoned)
	}
	// Nothing known about the account: nothing to forget.
	if got := deleteNotice(t, w, "a.example", "@12345:a.example"); got != http.StatusNoContent {
		t.Errorf("unknown account: %d", got)
	}
}

func TestEnsureShadowNeverResurrectsATombstone(t *testing.T) {
	w := adPeer(t, nil)
	ctx := context.Background()
	if err := w.s.ApplyRemoteUserDeleted(ctx, w.users.byID[adShadow], "a.example"); err != nil {
		t.Fatal(err)
	}
	// A profile for the same FID (a stale relay) neither renames nor revives it.
	got, err := w.s.EnsureShadow(ctx, "a.example", Profile{FID: "@90:a.example", Username: "alice", DisplayName: "Alice"})
	if err != nil || got.ID != adShadow || !got.IsDeleted() || got.Username != "" {
		t.Fatalf("EnsureShadow = %+v, %v", got, err)
	}
	if len(w.users.upserts) != 0 {
		t.Fatalf("a tombstone was rewritten: %+v", w.users.upserts)
	}
	// A new account that took the name at home has a new id, so a new shadow.
	fresh, err := w.s.EnsureShadow(ctx, "a.example", Profile{FID: "@91:a.example", Username: "alice"})
	if err != nil || fresh.ID == adShadow || fresh.IsDeleted() || fresh.Username != "alice" {
		t.Fatalf("new account's shadow = %+v, %v", fresh, err)
	}
}

func TestMessageEventNamesADeletedLocalSender(t *testing.T) {
	w := adPeer(t, nil)
	ctx := context.Background()
	ev := w.s.messageEvent(ctx, RoomRef{OriginDomain: "b.example", OriginRoomID: "5"}, &messages.Message{ID: 1, RoomID: 5, SenderID: adLocalDel, Plaintext: "still here"}, nil)
	if ev.Sender == nil || !ev.Sender.Deleted || ev.Sender.FID != "@777:b.example" || ev.Sender.Username != "" {
		t.Fatalf("sender %+v", ev.Sender)
	}
	// A tombstoned shadow goes out as deleted too, under its own FID.
	_ = w.users.TombstoneShadow(ctx, adShadow)
	ev = w.s.messageEvent(ctx, RoomRef{OriginDomain: "b.example", OriginRoomID: "5"}, &messages.Message{ID: 2, RoomID: 5, SenderID: adShadow}, nil)
	if ev.Sender == nil || !ev.Sender.Deleted || ev.Sender.FID != "@90:a.example" {
		t.Fatalf("tombstone sender %+v", ev.Sender)
	}
	// A live sender is a normal profile.
	ev = w.s.messageEvent(ctx, RoomRef{OriginDomain: "b.example", OriginRoomID: "5"}, &messages.Message{ID: 3, RoomID: 5, SenderID: adBob}, nil)
	if ev.Sender == nil || ev.Sender.Deleted || ev.Sender.Username != "bob" {
		t.Fatalf("live sender %+v", ev.Sender)
	}
}

func deletedEvent(fid string) MessageEvent {
	return MessageEvent{
		Room:      RoomRef{OriginDomain: "a.example", OriginRoomID: "5"},
		Message:   MessageRef{OriginDomain: "a.example", OriginMessageID: "900"},
		Sender:    &Profile{FID: fid, Deleted: true},
		Plaintext: "kept", CreatedAt: time.Now().UTC(),
	}
}

// A history page keeps the messages of a deleted account in every case.
func TestMessageFromEventKeepsADeletedSendersMessage(t *testing.T) {
	ctx := context.Background()
	sc := &roomScope{room: &rooms.Room{ID: 5, Type: rooms.TypeSpaceText}, spaceID: 9}

	t.Run("our own deleted account", func(t *testing.T) {
		w := adPeer(t, nil)
		msg, err := w.s.messageFromEvent(ctx, "a.example", sc, deletedEvent("@777:b.example"))
		if err != nil || msg.SenderID != adLocalDel {
			t.Fatalf("msg %+v, %v", msg, err)
		}
	})

	t.Run("the vouching instance's account", func(t *testing.T) {
		w := adPeer(t, nil)
		msg, err := w.s.messageFromEvent(ctx, "a.example", sc, deletedEvent("@90:a.example"))
		if err != nil || msg.SenderID != adShadow {
			t.Fatalf("msg %+v, %v", msg, err)
		}
		if !w.users.byID[adShadow].IsDeleted() {
			t.Fatal("its home's word did not tombstone the shadow")
		}
		// One never seen before becomes a tombstone from the start.
		msg, err = w.s.messageFromEvent(ctx, "a.example", sc, deletedEvent("@91:a.example"))
		if err != nil || msg.SenderID == 0 {
			t.Fatalf("msg %+v, %v", msg, err)
		}
		made := w.users.byID[msg.SenderID]
		if made == nil || !made.IsDeleted() || made.HomeDomain != "a.example" || *made.RemoteID != 91 {
			t.Fatalf("new tombstone %+v", made)
		}
	})

	t.Run("a third instance's account, its home says 404", func(t *testing.T) {
		w := adPeer(t, func(c fiber.Ctx) error {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
		})
		msg, err := w.s.messageFromEvent(ctx, "a.example", sc, deletedEvent("@71:c.example"))
		if err != nil || msg.SenderID == 0 {
			t.Fatalf("msg %+v, %v", msg, err)
		}
		if made := w.users.byID[msg.SenderID]; made == nil || !made.IsDeleted() || made.HomeDomain != "c.example" {
			t.Fatalf("tombstone %+v", made)
		}
	})

	t.Run("a third instance's account, its home still has it", func(t *testing.T) {
		w := adPeer(t, func(c fiber.Ctx) error {
			return c.JSON(Profile{FID: "@71:c.example", Username: "dora"})
		})
		msg, err := w.s.messageFromEvent(ctx, "a.example", sc, deletedEvent("@71:c.example"))
		if err != nil {
			t.Fatal(err)
		}
		if made := w.users.byID[msg.SenderID]; made == nil || made.IsDeleted() || made.Username != "dora" {
			t.Fatalf("a third party's 'deleted' was trusted: %+v", made)
		}
	})

	t.Run("a third instance's account we already know", func(t *testing.T) {
		w := adPeer(t, nil) // no client: nothing may be fetched
		msg, err := w.s.messageFromEvent(ctx, "a.example", sc, deletedEvent("@70:c.example"))
		if err != nil || msg.SenderID != adThird {
			t.Fatalf("msg %+v, %v", msg, err)
		}
		if w.users.byID[adThird].IsDeleted() {
			t.Fatal("a third party's word tombstoned a known shadow")
		}
	})
}

func TestProfileOfATombstone(t *testing.T) {
	w := adPeer(t, nil)
	_ = w.users.TombstoneShadow(context.Background(), adShadow)
	p := w.s.ProfileOf(w.users.byID[adShadow])
	if !p.Deleted || p.FID != "@90:a.example" || p.Username != "" || p.DisplayName != "" {
		t.Fatalf("profile %+v", p)
	}
}

func TestDeletionAudienceAndNotice(t *testing.T) {
	w := adPeer(t, nil)
	ctx := context.Background()
	// bob (local) shares the group with a.example's alice and has a pending request with
	// c.example's cathy: both instances hold a shadow of him.
	got := w.s.DeletionAudience(ctx, w.users.byID[adBob], []int64{adThird})
	if _, ok := got["a.example"]; !ok {
		t.Errorf("audience %v lacks a.example", got)
	}
	if _, ok := got["c.example"]; !ok {
		t.Errorf("audience %v lacks c.example", got)
	}
	if _, ok := got["b.example"]; ok {
		t.Errorf("audience %v includes this instance", got)
	}

	// Through the durable outbox (not the ephemeral queue): it must survive a peer being down.
	w.s.AfterAccountDeleted(ctx, w.users.byID[adBob], map[string]struct{}{"a.example": {}})
	repo := w.s.repo.(*adFedRepo)
	if len(repo.outbox) != 1 || repo.outbox[0].Domain != "a.example" || repo.outbox[0].Path != "/users/delete" ||
		repo.outbox[0].Body != `{"user":"@501:b.example"}` {
		t.Fatalf("outbox %+v", repo.outbox)
	}
	// Never about a shadow: its home speaks for it.
	w.s.AfterAccountDeleted(ctx, w.users.byID[adShadow], map[string]struct{}{"a.example": {}})
	if len(repo.outbox) != 1 {
		t.Fatalf("a shadow's deletion was announced: %+v", repo.outbox)
	}
}
