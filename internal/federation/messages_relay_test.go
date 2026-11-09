package federation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/voice"
)

// What the origin says about a channel, and to whom. Two instances are in play in these tests:
// a.example hosts the space; b.example and c.example mirror it.

// ---- the origin's relays ------------------------------------------------------------------

// spyOutbox is the durable half of relaying: what send would have queued for each peer.
type spyOutbox struct {
	*vsFedRepo
	mu   sync.Mutex
	sent map[string][]OutboxEntry
}

func (r *spyOutbox) PutOutbox(_ context.Context, e *OutboxEntry, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent[e.Domain] = append(r.sent[e.Domain], *e)
	return nil
}

func (r *spyOutbox) GetMessageMapping(context.Context, int64, int64) (*MessageMapping, error) {
	return nil, nil
}

func (r *spyOutbox) to(domain string) []OutboxEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent[domain]
}

func spyOn(s *Service) *spyOutbox {
	spy := &spyOutbox{vsFedRepo: s.repo.(*vsFedRepo), sent: map[string][]OutboxEntry{}}
	s.repo = spy
	return spy
}

// viewerOn gives an instance a member who can view the private room.
func (w *vsWorld) viewerOn(domain string, localID, remoteID int64) {
	w.users.byID[localID] = shadowUser(localID, remoteID, domain)
	w.space.members[localID] = rpMemberRow(rpSpace, localID, rpStaff)
}

func domains(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

func TestChannelPeers(t *testing.T) {
	ctx := context.Background()
	t.Run("an open room reaches every peer", func(t *testing.T) {
		w := vsOrigin(t)
		if got := domains(w.s.channelPeers(ctx, rpSpace, vsLounge)); !slices.Equal(got, []string{"b.example", "c.example"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("a private room reaches no peer without a viewer", func(t *testing.T) {
		w := vsOrigin(t)
		if got := domains(w.s.channelPeers(ctx, rpSpace, vsSecret)); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("a viewer opens the room to their own instance alone", func(t *testing.T) {
		w := vsOrigin(t)
		w.viewerOn("c.example", 90, 900)
		if got := domains(w.s.channelPeers(ctx, rpSpace, vsSecret)); !slices.Equal(got, []string{"c.example"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("an instance that does not mirror the space is not one", func(t *testing.T) {
		w := vsOrigin(t)
		w.viewerOn("d.example", 90, 900)
		if got := domains(w.s.channelPeers(ctx, rpSpace, vsSecret)); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("local viewers count for no peer", func(t *testing.T) {
		w := vsOrigin(t) // rpHigh and the owner, both a.example's, can view it
		if !slices.Contains(mustViewers(t, w, vsSecret), rpHigh) {
			t.Fatal("fixture: rpHigh should view the room")
		}
		if got := w.s.channelPeers(ctx, rpSpace, vsSecret); len(got) != 0 {
			t.Fatalf("got %v", domains(got))
		}
	})
	t.Run("viewers that cannot be read keep the room in", func(t *testing.T) {
		w := vsOrigin(t)
		w.viewerOn("b.example", 90, 900)
		w.space.listFails = true
		if got := w.s.channelPeers(ctx, rpSpace, vsSecret); len(got) != 0 {
			t.Fatalf("got %v", domains(got))
		}
	})
	t.Run("users that cannot be read keep the room in", func(t *testing.T) {
		w := vsOrigin(t)
		w.viewerOn("b.example", 90, 900)
		w.s.users = failingBatch{w.users}
		if got := w.s.channelPeers(ctx, rpSpace, vsSecret); len(got) != 0 {
			t.Fatalf("got %v", domains(got))
		}
	})
}

func mustViewers(t *testing.T, w *vsWorld, roomID int64) []int64 {
	t.Helper()
	viewers, everyone := w.s.spaceSvc.ViewersOf(context.Background(), rpSpace, roomID)
	if everyone {
		t.Fatal("fixture: room is open")
	}
	return viewers
}

type failingBatch struct{ *vsUsers }

func (failingBatch) GetByIDs(context.Context, []int64) ([]*auth.User, error) { return nil, errUserRead }

// Each relay that carries something said in a room, as the origin makes it.
var contentRelays = []struct {
	name, path string
	relay      func(ctx context.Context, s *Service, roomID int64)
}{
	{"message", "/rooms/messages", func(ctx context.Context, s *Service, roomID int64) {
		s.AfterMessageCreated(ctx, roomID, nil, &messages.Message{ID: 1, RoomID: roomID, Plaintext: "secret"})
	}},
	{"edit", "/rooms/messages", func(ctx context.Context, s *Service, roomID int64) {
		s.AfterMessageEdited(ctx, roomID, nil, &messages.Message{ID: 1, RoomID: roomID, Plaintext: "secret"})
	}},
	{"reaction", "/rooms/reactions", func(ctx context.Context, s *Service, roomID int64) {
		s.AfterReactionAdded(ctx, roomID, nil, 1, rpOwner, "👍")
	}},
	{"reaction removed", "/rooms/reactions/delete", func(ctx context.Context, s *Service, roomID int64) {
		s.AfterReactionRemoved(ctx, roomID, nil, 1, rpOwner, "👍")
	}},
	{"pin", "/rooms/pins", func(ctx context.Context, s *Service, roomID int64) {
		s.AfterMessagePinned(ctx, roomID, nil, 1, rpOwner, time.Now(), false)
	}},
}

func TestWhatIsSaidInAPrivateRoomGoesWhereItCanBeRead(t *testing.T) {
	for _, relay := range contentRelays {
		t.Run(relay.name, func(t *testing.T) {
			w := vsOrigin(t)
			spy := spyOn(w.s)
			w.viewerOn("b.example", 90, 900)
			relay.relay(context.Background(), w.s, vsSecret)
			if len(spy.to("b.example")) != 1 {
				t.Errorf("the peer with a viewer got %+v", spy.to("b.example"))
			}
			if len(spy.to("c.example")) != 0 {
				t.Errorf("the peer without one got %+v", spy.to("c.example"))
			}
		})
	}
}

func TestWhatIsSaidInAnOpenRoomGoesToEveryPeer(t *testing.T) {
	for _, relay := range contentRelays {
		t.Run(relay.name, func(t *testing.T) {
			w := vsOrigin(t)
			spy := spyOn(w.s)
			relay.relay(context.Background(), w.s, vsLounge)
			if len(spy.to("b.example")) != 1 || len(spy.to("c.example")) != 1 {
				t.Errorf("sent %+v", spy.sent)
			}
		})
	}
}

// A delete carries no content, and a peer that stored the message while it had a viewer has
// to drop it: this one relay is not narrowed.
func TestADeleteReachesEveryPeer(t *testing.T) {
	w := vsOrigin(t)
	spy := spyOn(w.s)
	w.s.AfterMessageDeleted(context.Background(), vsSecret, nil, 1)
	for _, d := range []string{"b.example", "c.example"} {
		if got := spy.to(d); len(got) != 1 || got[0].Path != "/rooms/messages/delete" {
			t.Errorf("%s got %+v", d, got)
		}
	}
}

// settled waits until everything queued for these peers so far has been handed over: a queue
// runs in order, so its turn at a marker means whatever came before it has gone. A count taken
// without this can pass only because the extra relay has not arrived yet.
func settled(t *testing.T, s *Service, peers ...string) {
	t.Helper()
	for _, peer := range peers {
		done := make(chan struct{})
		s.enqueue(peer, func() { close(done) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s's queue did not drain", peer)
		}
	}
}

// ---- voice relays --------------------------------------------------------------------------

func TestAVoiceStateInAPrivateRoomStaysOutOfPeersWithoutAViewer(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	room, _ := w.s.roomSvc.LoadRoom(context.Background(), vsSecret)
	w.s.AfterVoiceStateChanged(context.Background(), room, &voice.State{UserID: rpHigh, RoomID: vsSecret}, false)
	w.viewerOn("c.example", 90, 900)
	w.s.AfterVoiceStateChanged(context.Background(), room, &voice.State{UserID: rpHigh, RoomID: vsSecret}, true)
	settled(t, w.s, "b.example", "c.example")
	if len(log.sent["b.example"]) != 0 || len(log.sent["c.example"]) != 1 {
		t.Fatalf("sent %v", log.sent)
	}
}

// A user's own instance hears of them whatever else it can see: nothing there is a stranger's
// occupancy, and it is the one that has to hang them up.
func TestAVoiceStateAlwaysReachesItsUsersHome(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	room, _ := w.s.roomSvc.LoadRoom(context.Background(), vsSecret)
	w.s.AfterVoiceStateChanged(context.Background(), room, &voice.State{UserID: rpMember, RoomID: vsSecret}, true)
	settled(t, w.s, "b.example", "c.example")
	if len(log.sent["b.example"]) != 1 || len(log.sent["c.example"]) != 0 {
		t.Fatalf("sent %v", log.sent)
	}
}

func TestAVoiceStateDoesNotReachAHomeOutsideTheSpace(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	w.users.byID[90] = shadowUser(90, 900, "d.example")
	room, _ := w.s.roomSvc.LoadRoom(context.Background(), vsSecret)
	w.s.AfterVoiceStateChanged(context.Background(), room, &voice.State{UserID: 90, RoomID: vsSecret}, true)
	settled(t, w.s, "b.example", "c.example", "d.example")
	if len(log.sent) != 0 {
		t.Fatalf("sent %v", log.sent)
	}
}

// Where someone went is the destination's occupancy: a peer that cannot see that room hears
// they left, and not where to.
func TestAMoveNamesItsDestinationOnlyWhereThatMayBeKnown(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	w.viewerOn("c.example", 90, 900)
	dest, _ := w.s.roomSvc.LoadRoom(context.Background(), vsSecret)
	st := &voice.State{UserID: rpHigh, RoomID: vsLounge}
	w.s.AfterVoiceStateMoved(context.Background(), hostedLounge(w), st, dest)
	settled(t, w.s, "b.example", "c.example")
	if len(log.sent["b.example"]) != 1 || len(log.sent["c.example"]) != 1 {
		t.Fatalf("each peer hears of a move once: %v", log.sent)
	}
	blind, ok := log.sent["b.example"][0].(VoiceStateEvent)
	if !ok || !blind.Left || blind.MovedTo != nil {
		t.Errorf("b.example got %+v", log.sent["b.example"][0])
	}
	sighted, ok := log.sent["c.example"][0].(VoiceStateEvent)
	if !ok || !sighted.Left || sighted.MovedTo == nil {
		t.Errorf("c.example got %+v", log.sent["c.example"][0])
	}
}

// A mirror relays no voice state of its own: its own users' included, those are the origin's
// to announce.
func TestAMirrorRelaysNoVoiceState(t *testing.T) {
	s, repo := mirrorWireFixture()
	repo.peers = []string{"c.example"} // a mirror knows the others, and still leaves it to the origin
	space := int64(rpMirror)
	mirrored := &rooms.Room{ID: 1300, SpaceID: &space}
	s.roomRepo = &vsRooms{byID: map[int64]*rooms.Room{1300: mirrored}}
	s.users.(*profileTestUsers).byID[3] = shadowUser(3, 70, "c.example")
	room := &rooms.RoomWithParticipants{Room: *mirrored}
	if got := s.voiceTargets(context.Background(), room, &voice.State{UserID: 3}); len(got) != 0 {
		t.Fatalf("got %v", domains(got))
	}
}

// ---- notices -------------------------------------------------------------------------------

func TestANoticeReachesTheInstanceWhoseMemberCausedIt(t *testing.T) {
	w := vsOrigin(t)
	spy := spyOn(w.s)
	ctx := WithExcludedPeer(context.Background(), "b.example")
	notice := &messages.Message{ID: 1, RoomID: vsLounge, SystemType: messages.SystemMessagePinned, SystemPayload: `{}`}
	w.s.AfterMessageCreated(ctx, vsLounge, nil, notice)
	if len(spy.to("b.example")) != 1 || len(spy.to("c.example")) != 1 {
		t.Fatalf("sent %+v", spy.sent)
	}
}

// A message itself is in the reply, and a second copy would race it.
func TestAMessageStaysOutOfTheInstanceThatAsked(t *testing.T) {
	w := vsOrigin(t)
	spy := spyOn(w.s)
	ctx := WithExcludedPeer(context.Background(), "b.example")
	w.s.AfterMessageCreated(ctx, vsLounge, nil, &messages.Message{ID: 1, RoomID: vsLounge, Plaintext: "hi"})
	if len(spy.to("b.example")) != 0 || len(spy.to("c.example")) != 1 {
		t.Fatalf("sent %+v", spy.sent)
	}
}

// A request that collects its relays hands the notice back with the rest.
func TestANoticeInACapturedRequestGoesInTheReply(t *testing.T) {
	w := vsOrigin(t)
	spy := spyOn(w.s)
	ctx, capture := WithRelayCapture(context.Background(), "b.example")
	notice := &messages.Message{ID: 1, RoomID: vsLounge, SystemType: "space_member_leave", SystemPayload: `{}`}
	w.s.AfterMessageCreated(ctx, vsLounge, nil, notice)
	if len(spy.to("b.example")) != 0 {
		t.Fatalf("sent %+v", spy.sent)
	}
	got := capture.take()
	if len(got) != 1 || got[0].Path != "/rooms/messages" {
		t.Fatalf("captured %+v", got)
	}
}

// ---- thread hooks --------------------------------------------------------------------------

type threadHookSpy struct {
	excluded []string
	deleted  int
}

func (h *threadHookSpy) OnThreadMessage(ctx context.Context, _ *rooms.Room, _ int64, _ []int64) {
	h.excluded = append(h.excluded, excludedPeer(ctx))
}
func (h *threadHookSpy) OnThreadMessageDeleted(context.Context, *rooms.Room) { h.deleted++ }

// The hook's own relay, an unarchive, is what the reply to the writer's mirror does not say.
func TestAThreadHearsOfAPeersMessageWithTheExclusionLifted(t *testing.T) {
	spy := &threadHookSpy{}
	hooks := peerThreadHooks{spy}
	hooks.OnThreadMessage(WithExcludedPeer(context.Background(), "b.example"), &rooms.Room{}, 1, nil)
	hooks.OnThreadMessageDeleted(context.Background(), &rooms.Room{})
	if !slices.Equal(spy.excluded, []string{""}) || spy.deleted != 1 {
		t.Fatalf("hook saw %+v", spy)
	}
}

// The wiring itself: this engine has its own messages service, and it is the one a mirror's
// member writes through, so without the hooks there no remote message ever touched a thread.
func TestAMessageFromAMirrorReachesTheThread(t *testing.T) {
	ctx := WithExcludedPeer(context.Background(), "b.example")
	w := vsOrigin(t)
	spyOn(w.s)
	const thread = 400
	space, parent := int64(rpSpace), int64(vsText)
	roomRepo := w.s.roomRepo.(*vsRooms)
	roomRepo.byID[thread] = &rooms.Room{ID: thread, Type: rooms.TypeThread, SpaceID: &space, ParentID: &parent}
	mem := &memMessages{stored: map[int64]*messages.Message{}}
	w.s.msgSvc = messages.NewService(mem, msgRooms{roomRepo}, w.users, nil, &config.Config{}, w.s.spaceSvc)
	w.s.msgSvc.SetFederator(w.s)
	hooks := &threadHookSpy{}
	w.s.SetThreadHooks(hooks)
	m, err := w.s.msgSvc.CreateFromRemote(ctx, rpMember, thread, &messages.CreateMessageInput{Plaintext: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hooks.excluded, []string{""}) {
		t.Fatalf("hook saw %+v", hooks)
	}
	if err := w.s.msgSvc.Delete(ctx, rpMember, thread, m.ID); err != nil {
		t.Fatal(err)
	}
	if hooks.deleted != 1 {
		t.Fatalf("hook saw %+v", hooks)
	}
}

// ---- typing --------------------------------------------------------------------------------

func typing(t *testing.T, s *Service, peer string, roomID, remoteID int64, domain string) int {
	t.Helper()
	return post(t, voiceRoute(peer, NewHandler(s).RoomTyping), TypingEvent{Room: hostedRef(roomID), User: FormatFID(remoteID, domain)})
}

func TestTypingInAHostedChannelNeedsView(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	if got := typing(t, w.s, "b.example", vsSecret, rpMemberB, "b.example"); got != http.StatusForbidden {
		t.Fatalf("status %d", got)
	}
	settled(t, w.s, "b.example", "c.example")
	if len(log.sent) != 0 {
		t.Fatalf("relayed %v", log.sent)
	}
}

func TestTypingFromAViewerIsFannedOutToPeersThatCanSeeTheRoom(t *testing.T) {
	w := vsOrigin(t)
	log := recordRelays(w.s)
	w.viewerOn("c.example", 90, 900)
	w.space.members[rpMember].RoleIDs = append(w.space.members[rpMember].RoleIDs, rpStaff)
	w.s.repo.(*vsFedRepo).peers = append(w.s.repo.(*vsFedRepo).peers, "d.example")
	if got := typing(t, w.s, "b.example", vsSecret, rpMemberB, "b.example"); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	settled(t, w.s, "b.example", "c.example", "d.example")
	if len(log.sent["b.example"]) != 0 || len(log.sent["c.example"]) != 1 || len(log.sent["d.example"]) != 0 {
		t.Fatalf("relayed %v", log.sent)
	}
}

// ---- the mirror ----------------------------------------------------------------------------

// memMessages is a message store over a map.
type memMessages struct {
	messages.Repository
	stored      map[int64]*messages.Message
	softDeleted []int64
	pins        []int64
}

func (m *memMessages) GetByID(_ context.Context, _, msgID int64) (*messages.Message, error) {
	return m.stored[msgID], nil
}

func (m *memMessages) Insert(_ context.Context, msg *messages.Message) error {
	m.stored[msg.ID] = msg
	return nil
}

func (m *memMessages) Create(ctx context.Context, msg *messages.Message) error {
	return m.Insert(ctx, msg)
}

func (m *memMessages) SoftDelete(_ context.Context, _, msgID int64) error {
	m.softDeleted = append(m.softDeleted, msgID)
	return nil
}

func (m *memMessages) Update(_ context.Context, _, msgID int64, _, _ string, _ *string) (*messages.Message, error) {
	return m.stored[msgID], nil
}

func (m *memMessages) ListPins(context.Context, int64, int) ([]messages.Pin, error) { return nil, nil }

func (m *memMessages) Pin(_ context.Context, _, msgID, _ int64, _ time.Time) error {
	m.pins = append(m.pins, msgID)
	return nil
}

func (m *memMessages) ListReactions(context.Context, int64, int64) ([]messages.Reaction, error) {
	return nil, nil
}

// msgRooms adds to vsRooms the writes a stored message makes.
type msgRooms struct{ *vsRooms }

func (msgRooms) UpdateLastMessageID(context.Context, int64, []int64, int64) error { return nil }
func (msgRooms) ClearLastMessageID(context.Context, int64, []int64) error         { return nil }
func (msgRooms) UpdateReadState(context.Context, int64, int64, int64) error       { return nil }
func (msgRooms) GetMentionCount(context.Context, int64, int64) (int, error)       { return 0, nil }
func (msgRooms) SetMentionCountBaseline(context.Context, int64, int64, int64) error {
	return nil
}

type msgFedRepo struct {
	*mirrorFedRepo
	mapped map[int64]*MessageMapping
}

func (r *msgFedRepo) GetMessageByOrigin(_ context.Context, _ string, originID int64) (*MessageMapping, error) {
	return r.mapped[originID], nil
}

func (r *msgFedRepo) GetMessageMapping(_ context.Context, _, msgID int64) (*MessageMapping, error) {
	return r.mapped[msgID], nil
}

func (r *msgFedRepo) PutMessageMapping(_ context.Context, m *MessageMapping) error {
	r.mapped[m.OriginMessageID] = m
	return nil
}

const (
	mmRoom   = 1300 // b.example's copy of the origin's room 300
	mmOrigin = 300
	mmFormer = 77   // a.example's id for someone who has left the space
	mmShadow = 7700 // b.example's shadow of them
)

type mirrorMsgs struct {
	s     *Service
	store *memMessages
	space *vsSpaceRepo
	sc    *roomScope
}

// msgMirror is b.example mirroring the channel, its signed client reaching app.
func msgMirror(t *testing.T, app *fiber.App) *mirrorMsgs {
	t.Helper()
	s, repo := resyncFixture(t, app, false)
	s.repo = &msgFedRepo{mirrorFedRepo: repo, mapped: map[int64]*MessageMapping{}}
	space := int64(rpMirror)
	roomRepo := msgRooms{&vsRooms{byID: map[int64]*rooms.Room{mmRoom: {ID: mmRoom, Type: rooms.TypeSpaceText, SpaceID: &space}}}}
	members := map[int64]*spaces.SpaceMember{rpMemberB: rpMemberRow(rpMirror, rpMemberB)}
	store := &vsSpaceRepo{roleRepo: newRoleRepo(&spaces.Space{ID: rpMirror, OwnerID: rpOwnerB, EveryoneRoleID: rpEveryone}, rpRoles(rpMirror, rpBotB), members)}
	users := s.users.(*profileTestUsers)
	users.byID[rpMemberB] = &auth.User{ID: rpMemberB, Username: "member"}
	users.byID[mmShadow] = shadowUser(mmShadow, mmFormer, "a.example")
	s.roomRepo = roomRepo
	s.spaceSvc = spaces.NewService(store, roomRepo, users, nil, nil)
	mem := &memMessages{stored: map[int64]*messages.Message{}}
	s.msgRepo = mem
	s.msgSvc = messages.NewService(mem, roomRepo, users, nil, &config.Config{}, s.spaceSvc)
	s.msgSvc.SetFederator(s)
	room, _ := roomRepo.GetByID(context.Background(), mmRoom)
	sc, _, err := s.mirrorScope(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	return &mirrorMsgs{s: s, store: mem, space: store, sc: sc}
}

func (m *mirrorMsgs) event(msgID int64, sender int64, text string) MessageEvent {
	p := m.s.ProfileOf(m.s.users.(*profileTestUsers).byID[sender])
	return MessageEvent{
		Room:    RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(mmOrigin)},
		Message: MessageRef{OriginDomain: "a.example", OriginMessageID: id.Format(msgID)},
		Sender:  &p, Plaintext: text, CreatedAt: time.Now().UTC(),
	}
}

func originSays(t *testing.T, path string, reply any) *fiber.App {
	t.Helper()
	app := fiber.New()
	app.Post(BasePath+path, func(c fiber.Ctx) error { return c.JSON(reply) })
	return app
}

// Someone who left the space still wrote what they wrote. Every read path used to drop their
// messages, and a short page ends infinite scroll.
func TestHistoryKeepsTheMessagesOfFormerMembers(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, m *mirrorMsgs, got []messages.Message) {
		t.Helper()
		if len(got) != 2 || got[0].SenderID != mmShadow || got[1].SenderID != rpMemberB {
			t.Fatalf("served %+v", got)
		}
		if m.store.stored[901] == nil || m.store.stored[901].SenderID != mmShadow {
			t.Fatalf("not stored: %+v", m.store.stored[901])
		}
	}
	room := func(m *mirrorMsgs) *rooms.Room { return m.sc.room }
	var reply SpaceMessagesReply
	build := func(m *mirrorMsgs) []MessageEvent {
		return []MessageEvent{m.event(901, mmShadow, "from someone gone"), m.event(902, rpMemberB, "still here")}
	}

	t.Run("history", func(t *testing.T) {
		app := fiber.New()
		app.Post(BasePath+"/spaces/messages/list", func(c fiber.Ctx) error { return c.JSON(reply) })
		m := msgMirror(t, app)
		reply = SpaceMessagesReply{Messages: build(m)}
		got, _, err := m.s.ListRemote(ctx, "a.example", room(m), rpMemberB, nil, 50)
		if err != nil {
			t.Fatal(err)
		}
		check(t, m, got)
	})
	t.Run("search", func(t *testing.T) {
		var search SpaceSearchReply
		app := fiber.New()
		app.Post(BasePath+"/spaces/messages/search", func(c fiber.Ctx) error { return c.JSON(search) })
		m := msgMirror(t, app)
		search = SpaceSearchReply{Messages: build(m)}
		res, err := m.s.SearchRemote(ctx, "a.example", room(m), rpMemberB, &messages.SearchOptions{Query: "x"})
		if err != nil {
			t.Fatal(err)
		}
		check(t, m, res.Messages)
	})
	t.Run("pins", func(t *testing.T) {
		var pins PinsReply
		app := fiber.New()
		app.Post(BasePath+"/spaces/pins/list", func(c fiber.Ctx) error { return c.JSON(pins) })
		m := msgMirror(t, app)
		evs := build(m)
		pins = PinsReply{Items: []PinItem{{Message: evs[0], PinnedAt: time.Now()}, {Message: evs[1], PinnedAt: time.Now()}}}
		got, err := m.s.ListPinsRemote(ctx, "a.example", room(m), rpMemberB)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Message.SenderID != mmShadow {
			t.Fatalf("served %+v", got)
		}
	})
	t.Run("one message", func(t *testing.T) {
		var one MessageEvent
		app := fiber.New()
		app.Post(BasePath+"/spaces/messages/get", func(c fiber.Ctx) error { return c.JSON(one) })
		m := msgMirror(t, app)
		one = m.event(901, mmShadow, "from someone gone")
		got, _, err := m.s.GetRemote(ctx, "a.example", room(m), rpMemberB, 901)
		if err != nil {
			t.Fatal(err)
		}
		if got.SenderID != mmShadow {
			t.Fatalf("served %+v", got)
		}
	})
}

// News is held to more than history: a peer may not invent a sender.
func TestARelayedMessageNeedsItsSenderInTheRoom(t *testing.T) {
	m := msgMirror(t, fiber.New())
	_, err := m.s.applyMessageEvent(context.Background(), "a.example", m.sc, m.event(901, mmShadow, "a stranger"))
	if !errors.Is(err, errNotInRoom) {
		t.Fatalf("err = %v", err)
	}
	if len(m.store.stored) != 0 {
		t.Fatalf("stored %+v", m.store.stored)
	}
	if _, err := m.s.applyMessageEvent(context.Background(), "a.example", m.sc, m.event(902, rpMemberB, "a member")); err != nil {
		t.Fatal(err)
	}
	if m.store.stored[902] == nil {
		t.Fatal("a member's message was not stored")
	}
}

func TestANoticeNeedsNoSender(t *testing.T) {
	m := msgMirror(t, fiber.New())
	ev := m.event(901, rpMemberB, "")
	ev.Sender, ev.SystemType = nil, "space_member_join"
	if _, err := m.s.applyMessageEvent(context.Background(), "a.example", m.sc, ev); err != nil {
		t.Fatal(err)
	}
}

// History comes newest first, so a reply is read before its parent. A channel's ids are the
// origin's everywhere, so the link needs no stored row.
func TestAReplyKeepsItsLinkWithoutItsParent(t *testing.T) {
	m := msgMirror(t, fiber.New())
	ev := m.event(902, rpMemberB, "a reply")
	ev.ReplyTo = &MessageRef{OriginDomain: "a.example", OriginMessageID: "901"}
	msg, err := m.s.messageFromEvent(context.Background(), "a.example", m.sc, ev)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ReplyToID == nil || *msg.ReplyToID != 901 {
		t.Fatalf("reply_to = %v", msg.ReplyToID)
	}
}

func TestLocalMessageID(t *testing.T) {
	ctx := context.Background()
	s := &Service{
		fcfg: config.FederationConfig{Enabled: true, Domain: "b.example"},
		repo: &msgFedRepo{mapped: map[int64]*MessageMapping{
			500: {RoomID: 5, MessageID: 42, OriginDomain: "a.example", OriginMessageID: 500},
			600: {RoomID: 6, MessageID: 43, OriginDomain: "a.example", OriginMessageID: 600},
		}},
	}
	pm := &roomScope{room: &rooms.Room{ID: 5}}
	channel := &roomScope{room: &rooms.Room{ID: 5}, spaceID: rpMirror}
	cases := []struct {
		name string
		sc   *roomScope
		ref  MessageRef
		want int64
	}{
		{"a channel's message, stored or not", channel, MessageRef{OriginDomain: "a.example", OriginMessageID: "900"}, 900},
		// A mirror names what it has no mapping for by its own domain; the id is still the id.
		{"as a mirror names one it never stored", channel, MessageRef{OriginDomain: "c.example", OriginMessageID: "900"}, 900},
		{"a channel's bad id", channel, MessageRef{OriginDomain: "a.example", OriginMessageID: "nope"}, 0},
		{"a PM's own message", pm, MessageRef{OriginDomain: "b.example", OriginMessageID: "900"}, 900},
		{"a PM's relayed message keeps a local id", pm, MessageRef{OriginDomain: "a.example", OriginMessageID: "500"}, 42},
		{"a PM's message from another room", pm, MessageRef{OriginDomain: "a.example", OriginMessageID: "600"}, 0},
		{"a PM's unknown message", pm, MessageRef{OriginDomain: "a.example", OriginMessageID: "700"}, 0},
	}
	for _, tc := range cases {
		if got := s.localMessageID(ctx, tc.sc, tc.ref); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func relayRoute(s *Service, handler fiber.Handler) *fiber.App {
	return voiceRoute("a.example", handler)
}

// A pin on a message older than the mirror still reaches its clients (PinFederated's promise),
// where it used to stop at a 404.
func TestAPinRelayForAMessageNotStoredHere(t *testing.T) {
	m := msgMirror(t, fiber.New())
	m.s.users.(*profileTestUsers).byID[rpOwnerB] = shadowUser(rpOwnerB, rpOwner, "a.example")
	m.s.spaceSvc = spaces.NewService(&vsSpaceRepo{roleRepo: newRoleRepo(&spaces.Space{ID: rpMirror, OwnerID: rpOwnerB, EveryoneRoleID: rpEveryone}, rpRoles(rpMirror, rpBotB),
		map[int64]*spaces.SpaceMember{rpOwnerB: rpMemberRow(rpMirror, rpOwnerB)})}, m.s.roomRepo, m.s.users, nil, nil)
	body := PinEvent{
		Room: RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(mmOrigin)}, Message: MessageRef{OriginDomain: "a.example", OriginMessageID: "901"},
		User: FormatFID(rpOwner, "a.example"), PinnedAt: time.Now(),
	}
	if got := post(t, voiceRoute("a.example", NewHandler(m.s).PinUpdate), body); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	if len(m.store.pins) != 0 {
		t.Fatalf("pinned %v, on a message nobody here holds", m.store.pins)
	}
}

func TestADeleteRelayForAMessageNotStoredHere(t *testing.T) {
	m := msgMirror(t, fiber.New())
	body := MessageDelete{
		Room: RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(mmOrigin)}, Message: MessageRef{OriginDomain: "a.example", OriginMessageID: "901"},
	}
	if got := post(t, voiceRoute("a.example", NewHandler(m.s).MessageDelete), body); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	if len(m.store.softDeleted) != 0 || len(m.store.stored) != 0 {
		t.Fatalf("wrote %v / %v", m.store.softDeleted, m.store.stored)
	}
}

func TestADeleteRelayOfAStoredMessage(t *testing.T) {
	m := msgMirror(t, fiber.New())
	m.store.stored[901] = &messages.Message{ID: 901, RoomID: mmRoom}
	body := MessageDelete{
		Room: RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(mmOrigin)}, Message: MessageRef{OriginDomain: "a.example", OriginMessageID: "901"},
	}
	if got := post(t, voiceRoute("a.example", NewHandler(m.s).MessageDelete), body); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	if !slices.Equal(m.store.softDeleted, []int64{901}) {
		t.Fatalf("deleted %v", m.store.softDeleted)
	}
}

func TestAnEditRelayForAMessageNotStoredHere(t *testing.T) {
	m := msgMirror(t, fiber.New())
	body := MessageEdit{
		Room: RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(mmOrigin)}, Message: MessageRef{OriginDomain: "a.example", OriginMessageID: "901"},
		Plaintext: "edited",
	}
	if got := post(t, voiceRoute("a.example", NewHandler(m.s).MessageEdit), body); got != http.StatusNotFound {
		t.Fatalf("status %d", got)
	}
	if len(m.store.stored) != 0 {
		t.Fatalf("stored %+v", m.store.stored)
	}
}

// A channel's messages are the origin's. A mirror's edits and deletes reach it through
// /spaces/messages, as the member, where they are judged; a relay naming the mirror as the
// message's origin must not get a stored message rewritten with nothing checked.
func TestAMirrorCannotRewriteAHostedChannelByRelay(t *testing.T) {
	w := vsOrigin(t)
	ref := MessageRef{OriginDomain: "b.example", OriginMessageID: "901"}
	h := NewHandler(w.s)
	edit := MessageEdit{Room: hostedRef(vsSecret), Message: ref, Plaintext: "forged"}
	if got := post(t, voiceRoute("b.example", h.MessageEdit), edit); got != http.StatusForbidden {
		t.Fatalf("edit: status %d", got)
	}
	del := MessageDelete{Room: hostedRef(vsSecret), Message: ref}
	if got := post(t, voiceRoute("b.example", h.MessageDelete), del); got != http.StatusForbidden {
		t.Fatalf("delete: status %d", got)
	}
}

// The origin has judged a typer by the time it passes them on, and a mirror's copy of the
// overrides is the one that can lag: it asks only that they are in the space, and the fan-out
// is the origin's alone.
func TestAMirrorTakesTheOriginsWordOnTyping(t *testing.T) {
	m := msgMirror(t, fiber.New())
	log := recordRelays(m.s)
	m.s.repo.(*msgFedRepo).peers = []string{"c.example"}
	m.s.roomSvc = rooms.NewService(m.s.roomRepo, m.s.users, nil, nil, nil, m.s.spaceSvc)
	m.space.overs = map[int64][]spaces.SpaceRoomRoleOverride{mmRoom: {{SpaceID: rpMirror, RoomID: mmRoom, RoleID: rpEveryone, Deny: permissions.PermViewRoom}}}
	body := TypingEvent{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(mmOrigin)}, User: FormatFID(rpMemberB, "b.example")}
	if got := post(t, voiceRoute("a.example", NewHandler(m.s).RoomTyping), body); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	settled(t, m.s, "c.example")
	if len(log.sent) != 0 {
		t.Fatalf("relayed %v", log.sent)
	}
}

func TestAMirrorRefusesTypingFromOutsideTheSpace(t *testing.T) {
	m := msgMirror(t, fiber.New())
	m.s.roomSvc = rooms.NewService(m.s.roomRepo, m.s.users, nil, nil, nil, m.s.spaceSvc)
	m.s.users.(*profileTestUsers).byID[3] = shadowUser(3, 70, "a.example")
	body := TypingEvent{Room: RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(mmOrigin)}, User: FormatFID(70, "a.example")}
	if got := post(t, voiceRoute("a.example", NewHandler(m.s).RoomTyping), body); got != http.StatusForbidden {
		t.Fatalf("status %d", got)
	}
}

// ---- system payloads -----------------------------------------------------------------------

func TestSystemPayloadCrossesTheBoundary(t *testing.T) {
	ctx := context.Background()
	origin, _ := rpOrigin()
	origin.repo = &vsFedRepo{}

	cases := []struct {
		name, stored string
		wire         map[string]any
	}{
		{
			"a member of a third instance",
			`{"user_id":"5"}`,
			map[string]any{"user_id": FormatFID(rpMemberB, "b.example")},
		},
		{
			"the origin's own user",
			`{"actor_id":"8","message_id":"123456"}`,
			map[string]any{"actor_id": FormatFID(rpHigh, "a.example"), "message_id": "123456"},
		},
		{
			"everything else stays",
			`{"actor_id":"8","name":"a <b> & c","how":"kicked","duration_seconds":12,"extra":{"user_id":"9"}}`,
			map[string]any{
				"actor_id": FormatFID(rpHigh, "a.example"), "name": "a <b> & c", "how": "kicked", "duration_seconds": float64(12),
				"extra": map[string]any{"user_id": "9"},
			},
		},
		{
			"a thread",
			`{"actor_id":"8","thread_id":"300"}`,
			map[string]any{"actor_id": FormatFID(rpHigh, "a.example"), "thread_id": RoomFID(300, "a.example")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			if err := json.Unmarshal([]byte(origin.systemPayloadToWire(ctx, tc.stored)), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.wire) {
				t.Fatalf("wire %v, want %v", got, tc.wire)
			}
		})
	}
}

func TestSystemPayloadToWireLeavesWhatItCannotName(t *testing.T) {
	ctx := context.Background()
	origin, _ := rpOrigin()
	origin.repo = &vsFedRepo{}
	for _, raw := range []string{
		``, `not json`, `[1,2]`, `{"name":"a"}`,
		`{"user_id":"404"}`,                          // nobody by that id
		`{"user_id":12}`,                             // not a string
		`{"user_id":"@5:b.example"}`,                 // already translated
		`{"message_id":"5"}`,                         // a channel keeps the origin's message ids
		`{"thread_id":"not an id","user_id":"nope"}`, // not ids at all
	} {
		if got := origin.systemPayloadToWire(ctx, raw); got != raw {
			t.Errorf("%q came out as %q", raw, got)
		}
	}
}

func TestSystemPayloadFromWire(t *testing.T) {
	ctx := context.Background()
	m := msgMirror(t, fiber.New())
	m.s.users.(*profileTestUsers).byID[rpOwnerB] = shadowUser(rpOwnerB, rpOwner, "a.example")
	cases := []struct {
		name, wire string
		want       map[string]any
	}{
		{"this instance's own user", `{"user_id":"@50:b.example"}`, map[string]any{"user_id": id.Format(rpMemberB)}},
		{"the origin's user", `{"actor_id":"@1:a.example"}`, map[string]any{"actor_id": id.Format(rpOwnerB)}},
		{"a room", `{"thread_id":"!300:a.example"}`, map[string]any{"thread_id": id.Format(mmRoom)}},
		{"message ids stay", `{"message_id":"999"}`, map[string]any{"message_id": "999"}},
		{"everything else stays", `{"user_id":"@50:b.example","how":"banned"}`, map[string]any{"user_id": id.Format(rpMemberB), "how": "banned"}},
		// An older origin's numbers mean nothing here: not ours to read as local ids either.
		{"an older peer's plain id", `{"user_id":"12345"}`, map[string]any{"user_id": "12345"}},
		{"nobody by that id", `{"user_id":"@404:b.example"}`, map[string]any{"user_id": "@404:b.example"}},
		{"not a room", `{"thread_id":"300"}`, map[string]any{"thread_id": "300"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			if err := json.Unmarshal([]byte(m.s.systemPayloadFromWire(ctx, tc.wire)), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, raw := range []string{``, `not json`, `[]`} {
		if got := m.s.systemPayloadFromWire(ctx, raw); got != raw {
			t.Errorf("%q came out as %q", raw, got)
		}
	}
}

// A room this mirror has not heard of stays as it came, and never allocates a mapping: a
// payload is no way to introduce a room.
func TestSystemPayloadFromWireWithAnUnknownRoom(t *testing.T) {
	m := msgMirror(t, fiber.New())
	m.s.repo = &vsNoRooms{msgFedRepo: m.s.repo.(*msgFedRepo)}
	got := m.s.systemPayloadFromWire(context.Background(), `{"thread_id":"!300:a.example"}`)
	if got != `{"thread_id":"!300:a.example"}` {
		t.Fatalf("got %s", got)
	}
}

type vsNoRooms struct{ *msgFedRepo }

func (*vsNoRooms) GetRoomByOrigin(context.Context, string, int64) (*RoomMapping, error) {
	return nil, nil
}

func (*vsNoRooms) PutRoomMapping(context.Context, *RoomMapping) error {
	panic("a payload allocated a room")
}

// The relay and the mirror together: "Carol joined" names Carol wherever it is read.
func TestANoticeNamesItsPersonOnTheMirror(t *testing.T) {
	ctx := context.Background()
	w := vsOrigin(t)
	spy := spyOn(w.s)
	notice := &messages.Message{ID: 901, RoomID: vsLounge, SystemType: "space_member_join", SystemPayload: `{"user_id":"5"}`}
	w.s.AfterMessageCreated(ctx, vsLounge, nil, notice)
	var ev MessageEvent
	if err := json.Unmarshal([]byte(spy.to("b.example")[0].Body), &ev); err != nil {
		t.Fatal(err)
	}
	m := msgMirror(t, fiber.New())
	msg, err := m.s.messageFromEvent(ctx, "a.example", m.sc, ev)
	if err != nil {
		t.Fatal(err)
	}
	if msg.SystemPayload != `{"user_id":"50"}` {
		t.Fatalf("payload %s", msg.SystemPayload)
	}
}

func TestParseRoomFID(t *testing.T) {
	roomID, domain, err := ParseRoomFID(RoomFID(300, "A.example:8443"))
	if err != nil || roomID != 300 || domain != "a.example:8443" {
		t.Fatalf("got %d %q %v", roomID, domain, err)
	}
	for _, bad := range []string{"", "300", "!300", "!300:", "!x:a.example", "@300:a.example"} {
		if _, _, err := ParseRoomFID(bad); !errors.Is(err, ErrInvalidFID) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

// ---- refusals ------------------------------------------------------------------------------

func TestSpaceFailAnswersAsALocalSenderIsAnswered(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		fields map[string]any
		header string
	}{
		{"automod", &messages.AutomodError{Rule: messages.AutomodRuleRepeated}, http.StatusBadRequest, map[string]any{"code": "automod_blocked", "rule": "repeated"}, ""},
		{"verification", &messages.VerificationError{Requirement: messages.RequireMemberAge, Level: 3}, http.StatusForbidden, map[string]any{"code": "verification_level", "requirement": "member_age", "level": float64(3)}, ""},
		{"slowmode", &messages.SlowmodeError{RetryAfter: 30 * time.Second}, http.StatusTooManyRequests, map[string]any{"retry_after": float64(30)}, "30"},
		{"a locked thread", messages.ErrThreadLocked, http.StatusForbidden, nil, ""},
		{"an archived thread", messages.ErrThreadArchived, http.StatusForbidden, nil, ""},
		{"attach", messages.ErrAttachForbidden, http.StatusForbidden, nil, ""},
		{"too long", messages.ErrContentTooLong, http.StatusBadRequest, nil, ""},
		{"too many pins", messages.ErrTooManyPins, http.StatusBadRequest, nil, ""},
		{"not a member", messages.ErrNotParticipant, http.StatusForbidden, map[string]any{"error": "not a member of that space"}, ""},
		{"no permission", messages.ErrForbidden, http.StatusForbidden, map[string]any{"error": "missing permission in that channel"}, ""},
		{"no such message", messages.ErrMessageNotFound, http.StatusNotFound, nil, ""},
		{"something broke", errors.New("scylla"), http.StatusInternalServerError, map[string]any{"error": "internal error"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()
			app.Get("/", func(c fiber.Ctx) error { return spaceFail(c, tc.err, nil) })
			res, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil), fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != tc.status {
				t.Errorf("status %d, want %d: %s", res.StatusCode, tc.status, raw)
			}
			for k, want := range tc.fields {
				if body[k] != want {
					t.Errorf("%s = %v, want %v", k, body[k], want)
				}
			}
			if got := res.Header.Get("Retry-After"); got != tc.header {
				t.Errorf("Retry-After %q", got)
			}
		})
	}
}

func TestMessageOriginError(t *testing.T) {
	asOrigin := func(t *testing.T, err error) *messages.OriginError {
		t.Helper()
		var oe *messages.OriginError
		if !errors.As(err, &oe) {
			t.Fatalf("err = %v", err)
		}
		return oe
	}
	status := func(code int, body string) error { return &StatusError{Domain: "a.example", Status: code, Body: body} }

	t.Run("a refusal keeps what it said", func(t *testing.T) {
		oe := asOrigin(t, messageOriginError(status(http.StatusBadRequest, `{"error":"blocked","code":"automod_blocked","rule":"invite"}`), "a.example", "/x"))
		if oe.Status != http.StatusBadRequest || oe.Message != "blocked" || !reflect.DeepEqual(oe.Body, map[string]any{"code": "automod_blocked", "rule": "invite"}) {
			t.Fatalf("got %+v", oe)
		}
	})
	t.Run("slowmode keeps its wait", func(t *testing.T) {
		oe := asOrigin(t, messageOriginError(status(http.StatusTooManyRequests, `{"error":"slowmode","retry_after":12}`), "a.example", "/x"))
		if oe.Body["retry_after"] != 12 {
			t.Fatalf("got %+v", oe)
		}
	})
	t.Run("verification keeps its detail", func(t *testing.T) {
		oe := asOrigin(t, messageOriginError(status(http.StatusForbidden, `{"error":"no","code":"verification_level","requirement":"email","level":1}`), "a.example", "/x"))
		if !reflect.DeepEqual(oe.Body, map[string]any{"code": "verification_level", "requirement": "email", "level": 1}) {
			t.Fatalf("got %+v", oe)
		}
	})
	t.Run("nothing else a peer says gets through", func(t *testing.T) {
		long := strings.Repeat("x", maxRefusalField+1)
		oe := asOrigin(t, messageOriginError(status(http.StatusForbidden, `{"error":"no","html":"<script>","code":7,"rule":"`+long+`","retry_after":"soon","level":"3"}`), "a.example", "/x"))
		if len(oe.Body) != 0 {
			t.Fatalf("got %+v", oe.Body)
		}
	})
	t.Run("a wait out of any range is dropped", func(t *testing.T) {
		for _, body := range []string{`{"retry_after":0}`, `{"retry_after":-5}`, `{"retry_after":99999999}`} {
			if oe := asOrigin(t, messageOriginError(status(http.StatusTooManyRequests, body), "a.example", "/x")); len(oe.Body) != 0 {
				t.Errorf("%s: %+v", body, oe.Body)
			}
		}
	})
	t.Run("a plain-text refusal", func(t *testing.T) {
		oe := asOrigin(t, messageOriginError(status(http.StatusForbidden, `go away`), "a.example", "/x"))
		if oe.Message != "go away" || len(oe.Body) != 0 {
			t.Fatalf("got %+v", oe)
		}
	})
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"no answer", ErrRemoteUnavailable},
		{"a broken API", status(http.StatusInternalServerError, `{"error":"internal error"}`)},
		{"a proxy in front of a dead API", status(http.StatusBadGateway, `<html>Bad Gateway</html>`)},
		{"a proxy with nothing behind it", status(http.StatusServiceUnavailable, ``)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := messageOriginError(tc.err, "a.example", "/x"); !errors.Is(err, messages.ErrOriginUnavailable) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The whole hop: a refusal on the origin reaches the mirror's messages service whole.
func TestARefusalCrossesToTheMirror(t *testing.T) {
	app := fiber.New()
	app.Post(BasePath+"/spaces/messages", func(c fiber.Ctx) error {
		return spaceFail(c, &messages.SlowmodeError{RetryAfter: 45 * time.Second}, nil)
	})
	m := msgMirror(t, app)
	_, err := m.s.CreateRemote(context.Background(), "a.example", m.sc.room, rpMemberB, &messages.CreateMessageInput{Plaintext: "hi"}, nil)
	var oe *messages.OriginError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v", err)
	}
	if oe.Status != http.StatusTooManyRequests || oe.Body["retry_after"] != 45 {
		t.Fatalf("got %+v", oe)
	}
}

// The other half of a mirror's reads: a refusal there is the origin's, and the copy here is
// not asked. What a mirror's own View says does not come into it.
func TestAMirrorServesNothingTheOriginRefused(t *testing.T) {
	app := fiber.New()
	app.Post(BasePath+"/spaces/messages/list", func(c fiber.Ctx) error {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission in that channel"})
	})
	m := msgMirror(t, app)
	m.store.stored[901] = &messages.Message{ID: 901, RoomID: mmRoom, Plaintext: "relayed secret"}
	_, _, err := m.s.msgSvc.ListWithReactions(context.Background(), rpMemberB, mmRoom, nil, 50)
	var oe *messages.OriginError
	if !errors.As(err, &oe) || oe.Status != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
}
