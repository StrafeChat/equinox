package messages

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
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/push"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/redistest"
)

// A channel of a space hosted elsewhere, as this instance sees it: the origin answers for it
// and the local copy is only what was relayed. What the origin refuses stays refused, and what
// only this instance can do (its uploads, its members' read state, its members' badges) happens
// here.

const (
	mirrorRoom  = 100
	mirrorSpace = 7

	alice = 2 // a member here
	bob   = 3 // a member here, holding the role below
	carol = 4 // a member here, sender of most messages
	mods  = 50
)

type fakeMessages struct {
	Repository
	stored      map[int64]*Message
	uploads     map[int64]*AttachmentRow
	softDeleted []int64
	dropped     []int64 // attachment rows deleted
	reads       int     // reads of the local copy
}

func (f *fakeMessages) GetByID(_ context.Context, _, msgID int64) (*Message, error) {
	f.reads++
	return f.stored[msgID], nil
}

func (f *fakeMessages) List(context.Context, int64, *int64, int) ([]Message, error) {
	f.reads++
	return f.page(), nil
}

func (f *fakeMessages) ListAfter(context.Context, int64, int64, int) ([]Message, error) {
	f.reads++
	return f.page(), nil
}

func (f *fakeMessages) page() []Message {
	var out []Message
	for _, m := range f.stored {
		out = append(out, *m)
	}
	return out
}

func (f *fakeMessages) ListReactionsForMessages(context.Context, int64, []int64) (map[int64][]Reaction, error) {
	return nil, nil
}

func (f *fakeMessages) ListReactions(context.Context, int64, int64) ([]Reaction, error) {
	return nil, nil
}

func (f *fakeMessages) Insert(_ context.Context, m *Message) error {
	f.stored[m.ID] = m
	return nil
}

func (f *fakeMessages) SoftDelete(_ context.Context, _, msgID int64) error {
	f.softDeleted = append(f.softDeleted, msgID)
	return nil
}

func (f *fakeMessages) GetAttachment(_ context.Context, _, attachmentID int64) (*AttachmentRow, error) {
	return f.uploads[attachmentID], nil
}

func (f *fakeMessages) SetAttachmentMessage(_ context.Context, _, attachmentID, msgID int64) error {
	f.uploads[attachmentID].MessageID = &msgID
	return nil
}

func (f *fakeMessages) DeleteAttachment(_ context.Context, _, attachmentID int64) error {
	f.dropped = append(f.dropped, attachmentID)
	return nil
}

type fakeRooms struct {
	rooms.Repository
	room        *rooms.Room
	totals      map[int64]int
	baselines   map[int64]int64
	incremented []int64
	decremented []int64
	readTo      map[int64]int64
}

func (f *fakeRooms) GetByID(context.Context, int64) (*rooms.Room, error) { return f.room, nil }

func (f *fakeRooms) GetByIDs(context.Context, []int64) ([]*rooms.Room, error) {
	return []*rooms.Room{f.room}, nil
}

func (f *fakeRooms) ListBySpace(context.Context, int64) ([]rooms.RoomBySpaceRow, error) {
	return []rooms.RoomBySpaceRow{{SpaceID: mirrorSpace, RoomID: mirrorRoom}}, nil
}

func (f *fakeRooms) GetParticipants(context.Context, int64) ([]int64, error) { return nil, nil }

func (f *fakeRooms) ListThreadMemberIDs(context.Context, int64) ([]int64, error) { return nil, nil }

func (f *fakeRooms) UpdateLastMessageID(context.Context, int64, []int64, int64) error { return nil }

func (f *fakeRooms) UpdateReadState(_ context.Context, userID, _, msgID int64) error {
	f.readTo[userID] = msgID
	return nil
}

func (f *fakeRooms) GetMentionCount(_ context.Context, userID, _ int64) (int, error) {
	return f.totals[userID], nil
}

func (f *fakeRooms) SetMentionCountBaseline(_ context.Context, userID, _, total int64) error {
	f.baselines[userID] = total
	return nil
}

func (f *fakeRooms) GetRoomRow(_ context.Context, userID, roomID int64) (*rooms.RoomRow, error) {
	b := f.baselines[userID]
	return &rooms.RoomRow{UserID: userID, RoomID: roomID, MentionCountBaseline: &b}, nil
}

func (f *fakeRooms) IncrementMentionCounts(_ context.Context, _ int64, userIDs []int64) error {
	f.incremented = append(f.incremented, userIDs...)
	for _, uid := range userIDs {
		f.totals[uid]++
	}
	return nil
}

func (f *fakeRooms) DecrementMentionCounts(_ context.Context, _ int64, userIDs []int64) error {
	f.decremented = append(f.decremented, userIDs...)
	return nil
}

type fakeAuth struct {
	SpaceChannelAuth
	perms       map[int64]int64
	wideRoles   []bool // includeUnmentionable, per expansion of a role mention
	ownInvites  map[string]bool
	askedInvite []string
}

func (f *fakeAuth) IsMember(context.Context, int64, int64) (bool, error) { return true, nil }

func (f *fakeAuth) EffectiveChannelPermissions(_ context.Context, userID, _, _ int64) (int64, error) {
	return f.perms[userID], nil
}

func (f *fakeAuth) ListSpaceMemberUserIDs(context.Context, int64) ([]int64, error) {
	return []int64{alice, bob, carol}, nil
}

func (f *fakeAuth) ListSpaceMemberUserIDsByRoles(_ context.Context, _ int64, roleIDs []int64, includeUnmentionable bool) ([]int64, error) {
	f.wideRoles = append(f.wideRoles, includeUnmentionable)
	if slices.Contains(roleIDs, mods) {
		return []int64{bob, carol}, nil // the sender holds it too
	}
	return nil, nil
}

func (f *fakeAuth) InviteBelongsToSpace(_ context.Context, code string, _ int64) (bool, error) {
	f.askedInvite = append(f.askedInvite, code)
	return f.ownInvites[code], nil
}

// fakeFederator is the origin as the messages service reaches it. err is its answer to any
// read; a nil err answers with origin's messages.
type fakeFederator struct {
	Federator
	host    string
	err     error
	message *Message
	created []*CreateMessageInput
	create  func(in *CreateMessageInput, attachments []Attachment) (*Message, error)
}

func (f *fakeFederator) RemoteOrigin(context.Context, *rooms.Room) string              { return f.host }
func (f *fakeFederator) RemoteSpaceOrigin(context.Context, int64) string               { return f.host }
func (f *fakeFederator) AfterMessageCreated(context.Context, int64, []int64, *Message) {}
func (f *fakeFederator) CreateRemote(_ context.Context, _ string, _ *rooms.Room, _ int64, in *CreateMessageInput, attachments []Attachment) (*Message, error) {
	f.created = append(f.created, in)
	return f.create(in, attachments)
}

func (f *fakeFederator) history() ([]Message, map[int64][]ReactionSummary, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return []Message{*f.message}, nil, nil
}

func (f *fakeFederator) ListRemote(context.Context, string, *rooms.Room, int64, *int64, int) ([]Message, map[int64][]ReactionSummary, error) {
	return f.history()
}

func (f *fakeFederator) ListAfterRemote(context.Context, string, *rooms.Room, int64, int64, int) ([]Message, map[int64][]ReactionSummary, error) {
	return f.history()
}

func (f *fakeFederator) ListAroundRemote(context.Context, string, *rooms.Room, int64, int64, int) ([]Message, map[int64][]ReactionSummary, error) {
	return f.history()
}

func (f *fakeFederator) GetRemote(context.Context, string, *rooms.Room, int64, int64) (*Message, []ReactionSummary, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.message, nil, nil
}

func (f *fakeFederator) SearchRemote(context.Context, string, *rooms.Room, int64, *SearchOptions) (*SearchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &SearchResult{Messages: []Message{*f.message}, Searchable: true}, nil
}

func (f *fakeFederator) SearchSpaceRemote(context.Context, string, int64, int64, *SpaceSearchOptions) (*SpaceSearchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &SpaceSearchResult{Messages: []Message{*f.message}}, nil
}

type mirrorWorld struct {
	svc   *Service
	repo  *fakeMessages
	rooms *fakeRooms
	auth  *fakeAuth
	fed   *fakeFederator
}

func newMirrorWorld() *mirrorWorld {
	space := int64(mirrorSpace)
	view := permissions.PermViewRoom | permissions.PermReadMessageHistory | permissions.PermSendMessages | permissions.PermAttachFiles
	w := &mirrorWorld{
		repo:  &fakeMessages{stored: map[int64]*Message{}, uploads: map[int64]*AttachmentRow{}},
		rooms: &fakeRooms{room: &rooms.Room{ID: mirrorRoom, Type: rooms.TypeSpaceText, SpaceID: &space}, totals: map[int64]int{}, baselines: map[int64]int64{}, readTo: map[int64]int64{}},
		auth:  &fakeAuth{perms: map[int64]int64{alice: view, bob: view, carol: view}, ownInvites: map[string]bool{}},
		fed:   &fakeFederator{host: "a.example", message: &Message{ID: 900, RoomID: mirrorRoom, Plaintext: "from the origin"}},
	}
	w.svc = &Service{repo: w.repo, rooms: w.rooms, spaceAuth: w.auth, federator: w.fed, cfg: &config.Config{}}
	return w
}

// A read of a mirrored channel by each path the handlers use, returning what it served.
var mirrorReads = []struct {
	name string
	read func(context.Context, *Service) ([]Message, error)
}{
	{"list", func(ctx context.Context, s *Service) ([]Message, error) {
		out, _, err := s.ListWithReactions(ctx, alice, mirrorRoom, nil, 50)
		return out, err
	}},
	{"list after", func(ctx context.Context, s *Service) ([]Message, error) {
		out, _, err := s.ListAfterWithReactions(ctx, alice, mirrorRoom, 1, 50)
		return out, err
	}},
	{"list around", func(ctx context.Context, s *Service) ([]Message, error) {
		out, _, err := s.ListAroundWithReactions(ctx, alice, mirrorRoom, 1, 50)
		return out, err
	}},
	{"search", func(ctx context.Context, s *Service) ([]Message, error) {
		res, err := s.SearchMessages(ctx, alice, mirrorRoom, &SearchOptions{Query: "relayed"})
		if err != nil {
			return nil, err
		}
		return res.Messages, nil
	}},
	{"search the space", func(ctx context.Context, s *Service) ([]Message, error) {
		res, err := s.SearchSpaceMessages(ctx, alice, mirrorSpace, &SpaceSearchOptions{SearchOptions: SearchOptions{Query: "relayed"}})
		if err != nil {
			return nil, err
		}
		return res.Messages, nil
	}},
	{"get", func(ctx context.Context, s *Service) ([]Message, error) {
		m, _, err := s.Get(ctx, alice, mirrorRoom, 800)
		if err != nil {
			return nil, err
		}
		return []Message{*m}, nil
	}},
}

// The bug this pins: a mirror whose View had drifted read a private room out of its relayed
// copy, because any error from the origin counted as "the origin is down".
func TestAnOriginRefusalIsNotServedFromTheLocalCopy(t *testing.T) {
	for _, read := range mirrorReads {
		t.Run(read.name, func(t *testing.T) {
			w := newMirrorWorld()
			w.repo.stored[800] = &Message{ID: 800, RoomID: mirrorRoom, Plaintext: "relayed secret"}
			w.fed.err = &OriginError{Status: http.StatusForbidden, Message: "missing permission"}
			got, err := read.read(context.Background(), w.svc)
			var oe *OriginError
			if !errors.As(err, &oe) || oe.Status != http.StatusForbidden {
				t.Fatalf("err = %v, want the refusal", err)
			}
			if len(got) != 0 || w.repo.reads != 0 {
				t.Fatalf("served %v after %d local reads", got, w.repo.reads)
			}
		})
	}
}

// The origin not answering, or answering only for itself - a rate limit on this instance's
// federation traffic, a timeout, a signature it could not check - says nothing about the member
// or the room, so a read is served from the local copy as it was before.
func TestAnUnreachableOriginFallsBackToTheLocalCopy(t *testing.T) {
	downs := map[string]error{
		"unavailable": ErrOriginUnavailable,
		"401":         &OriginError{Status: http.StatusUnauthorized, Message: "bad signature"},
		"408":         &OriginError{Status: http.StatusRequestTimeout, Message: "timeout"},
		"425":         &OriginError{Status: http.StatusTooEarly, Message: "too early"},
		"429":         &OriginError{Status: http.StatusTooManyRequests, Message: "too many requests"},
	}
	for _, read := range mirrorReads {
		for name, down := range downs {
			t.Run(read.name+"/"+name, func(t *testing.T) {
				w := newMirrorWorld()
				w.repo.stored[800] = &Message{ID: 800, RoomID: mirrorRoom, Plaintext: "relayed copy"}
				w.fed.err = down
				got, err := read.read(context.Background(), w.svc)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) == 0 || got[0].Plaintext != "relayed copy" {
					t.Fatalf("served %+v", got)
				}
			})
		}
	}
}

func TestReadsTakeTheOriginsAnswerOverTheLocalCopy(t *testing.T) {
	for _, read := range mirrorReads {
		t.Run(read.name, func(t *testing.T) {
			w := newMirrorWorld()
			w.repo.stored[800] = &Message{ID: 800, RoomID: mirrorRoom, Plaintext: "stale copy"}
			got, err := read.read(context.Background(), w.svc)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Plaintext != "from the origin" {
				t.Fatalf("served %+v", got)
			}
		})
	}
}

// With nothing stored and nobody to ask, the answer is "could not reach", never "no such message".
func TestGetWithNoCopyAndNoOrigin(t *testing.T) {
	w := newMirrorWorld()
	w.fed.err = ErrOriginUnavailable
	if _, _, err := w.svc.Get(context.Background(), alice, mirrorRoom, 800); !errors.Is(err, ErrOriginUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetInARoomHostedHereReadsTheStore(t *testing.T) {
	w := newMirrorWorld()
	w.fed.host = ""
	if _, _, err := w.svc.Get(context.Background(), alice, mirrorRoom, 800); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// ---- refusals ---------------------------------------------------------------------------------

func answer(t *testing.T, handler fiber.Handler) (int, http.Header, map[string]any) {
	t.Helper()
	app := fiber.New()
	app.Get("/", handler)
	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil), fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return res.StatusCode, res.Header, body
}

func TestHTTPError(t *testing.T) {
	cases := []struct {
		err    error
		status int
		want   map[string]any
	}{
		{ErrThreadLocked, http.StatusForbidden, map[string]any{"error": ErrThreadLocked.Error()}},
		{ErrThreadArchived, http.StatusForbidden, map[string]any{"error": ErrThreadArchived.Error()}},
		{ErrAttachForbidden, http.StatusForbidden, map[string]any{"error": ErrAttachForbidden.Error()}},
		{ErrBlocked, http.StatusForbidden, map[string]any{"error": ErrBlocked.Error()}},
		{ErrSystemReadOnly, http.StatusForbidden, map[string]any{"error": ErrSystemReadOnly.Error()}},
		{ErrNotParticipant, http.StatusForbidden, map[string]any{"error": "not a participant"}},
		{ErrForbidden, http.StatusForbidden, map[string]any{"error": "missing permission to send messages in this channel"}},
		{ErrInvalidInput, http.StatusBadRequest, map[string]any{"error": "plaintext required when E2EE is off; ciphertext required when E2EE is on"}},
		{ErrContentTooLong, http.StatusBadRequest, map[string]any{"error": ErrContentTooLong.Error()}},
		{ErrTooManyMentions, http.StatusBadRequest, map[string]any{"error": ErrTooManyMentions.Error()}},
		{ErrTooManyAttachments, http.StatusBadRequest, map[string]any{"error": ErrTooManyAttachments.Error()}},
		{ErrAttachmentNotFound, http.StatusBadRequest, map[string]any{"error": ErrAttachmentNotFound.Error()}},
		{&SlowmodeError{RetryAfter: 12 * time.Second}, http.StatusTooManyRequests, map[string]any{"error": "slowmode is on in this channel", "retry_after": float64(12)}},
		{&SlowmodeError{RetryAfter: 10 * time.Millisecond}, http.StatusTooManyRequests, map[string]any{"error": "slowmode is on in this channel", "retry_after": float64(1)}},
		{&VerificationError{Requirement: RequireMemberAge, Level: 3}, http.StatusForbidden, map[string]any{
			"error": "you must have been a member of this space for 10 minutes to send messages", "code": "verification_level", "requirement": "member_age", "level": float64(3),
		}},
		{&AutomodError{Rule: AutomodRuleInvite}, http.StatusBadRequest, map[string]any{
			"error": "invite links to other spaces are not allowed here", "code": "automod_blocked", "rule": "invite",
		}},
	}
	for _, tc := range cases {
		status, body, ok := HTTPError(tc.err)
		if !ok {
			t.Errorf("%v: not a refusal", tc.err)
			continue
		}
		got, header, sent := answer(t, func(c fiber.Ctx) error { return WriteRefusal(c, status, body) })
		if got != tc.status || !reflect.DeepEqual(sent, tc.want) {
			t.Errorf("%v: %d %v, want %d %v", tc.err, got, sent, tc.status, tc.want)
		}
		if want := tc.want["retry_after"]; want != nil && header.Get("Retry-After") == "" {
			t.Errorf("%v: no Retry-After header", tc.err)
		} else if want == nil && header.Get("Retry-After") != "" {
			t.Errorf("%v: Retry-After %q on a refusal without one", tc.err, header.Get("Retry-After"))
		}
	}
	if _, _, ok := HTTPError(errors.New("scylla fell over")); ok {
		t.Error("an internal error read as a refusal")
	}
}

// What the origin said a client phrases a refusal from gets to the client.
func TestOriginErrorCarriesTheRefusalsFields(t *testing.T) {
	err := &OriginError{Status: http.StatusTooManyRequests, Message: "slowmode is on in this channel", Body: map[string]any{"retry_after": 9}}
	status, header, body := answer(t, func(c fiber.Ctx) error {
		res, ok := originError(c, err)
		if !ok {
			t.Fatal("not handled")
		}
		return res
	})
	if status != http.StatusTooManyRequests || body["retry_after"] != float64(9) || header.Get("Retry-After") != "9" {
		t.Fatalf("%d %v %v", status, body, header)
	}

	err = &OriginError{Status: http.StatusBadRequest, Message: "no", Body: map[string]any{"code": "automod_blocked", "rule": "repeated"}}
	_, header, body = answer(t, func(c fiber.Ctx) error {
		res, _ := originError(c, err)
		return res
	})
	if body["code"] != "automod_blocked" || body["rule"] != "repeated" || body["error"] != "no" || header.Get("Retry-After") != "" {
		t.Fatalf("%v %v", body, header)
	}

	status, _, _ = answer(t, func(c fiber.Ctx) error {
		return WriteRefusal(c, http.StatusTooManyRequests, fiber.Map{"retry_after": float64(30)})
	})
	if status != http.StatusTooManyRequests {
		t.Fatal(status)
	}
}

func postMessage(t *testing.T, h *Handler, userID int64) (int, map[string]any) {
	t.Helper()
	app := fiber.New()
	app.Post("/rooms/:id/messages", func(c fiber.Ctx) error {
		c.Locals(auth.LocalsKeyUser, &auth.User{ID: userID})
		return c.Next()
	}, h.Create)
	req := httptest.NewRequest(http.MethodPost, "/rooms/100/messages", strings.NewReader(`{"plaintext":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return res.StatusCode, body
}

func TestSendRefusalsReachTheClient(t *testing.T) {
	w := newMirrorWorld()
	w.auth.perms[bob] &^= permissions.PermSendMessages
	status, body := postMessage(t, NewHandler(w.svc), bob)
	if status != http.StatusForbidden || body["error"] != "missing permission to send messages in this channel" {
		t.Fatalf("%d %v", status, body)
	}

	w = newMirrorWorld()
	w.fed.create = func(*CreateMessageInput, []Attachment) (*Message, error) {
		return nil, &OriginError{Status: http.StatusBadRequest, Message: "no", Body: map[string]any{"code": "automod_blocked"}}
	}
	status, body = postMessage(t, NewHandler(w.svc), bob)
	if status != http.StatusBadRequest || body["code"] != "automod_blocked" {
		t.Fatalf("%d %v", status, body)
	}
}

// Search once turned every error it did not know into a 500: with the fallback gone, an
// origin's refusal would have become one.
func TestSearchPassesARefusalOn(t *testing.T) {
	status, _, body := answer(t, func(c fiber.Ctx) error {
		return searchError(c, &OriginError{Status: http.StatusForbidden, Message: "not yours"}, nil)
	})
	if status != http.StatusForbidden || body["error"] != "not yours" {
		t.Fatalf("%d %v", status, body)
	}
}

// ---- uploads ----------------------------------------------------------------------------------

const (
	bobsUpload  = 70
	cdn         = "https://cdn.b.example"
	bobsFileURL = cdn + "/v1/attachments/100/70-abc/cat.png"
)

func (w *mirrorWorld) withCDN(t *testing.T) *[]string {
	t.Helper()
	var deleted []string
	nebula := httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			deleted = append(deleted, req.URL.Path)
		}
		res.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(nebula.Close)
	w.svc.cfg = &config.Config{}
	w.svc.cfg.Nebula.BaseURL = nebula.URL
	w.svc.cfg.Nebula.UploadSecret = "secret"
	w.svc.cfg.Nebula.PublicURL = cdn
	w.repo.uploads[bobsUpload] = &AttachmentRow{RoomID: mirrorRoom, ID: bobsUpload, UploaderID: bob, Filename: "cat.png", URL: bobsFileURL}
	return &deleted
}

func TestAMirrorSendClaimsItsUploadsOnceTheOriginTookTheMessage(t *testing.T) {
	w := newMirrorWorld()
	w.withCDN(t)
	w.fed.create = func(_ *CreateMessageInput, attachments []Attachment) (*Message, error) {
		if len(attachments) != 1 || attachments[0].URL != bobsFileURL {
			t.Fatalf("origin was handed %+v", attachments)
		}
		return &Message{ID: 901, RoomID: mirrorRoom}, nil
	}
	if _, err := w.svc.Create(context.Background(), bob, mirrorRoom, &CreateMessageInput{Attachments: []string{id.Format(bobsUpload)}}); err != nil {
		t.Fatal(err)
	}
	if got := w.repo.uploads[bobsUpload].MessageID; got == nil || *got != 901 {
		t.Fatalf("claimed by %v, want the origin's message", got)
	}
}

// Refused, the upload is still bob's to try again with.
func TestARefusedMirrorSendLeavesItsUploadsUnclaimed(t *testing.T) {
	w := newMirrorWorld()
	w.withCDN(t)
	refusal := &OriginError{Status: http.StatusBadRequest, Message: "automod"}
	w.fed.create = func(*CreateMessageInput, []Attachment) (*Message, error) { return nil, refusal }
	_, err := w.svc.Create(context.Background(), bob, mirrorRoom, &CreateMessageInput{Attachments: []string{id.Format(bobsUpload)}})
	if !errors.Is(err, refusal) {
		t.Fatalf("err = %v", err)
	}
	if w.repo.uploads[bobsUpload].MessageID != nil {
		t.Fatal("a refused upload was claimed")
	}
	if len(w.rooms.readTo) != 0 {
		t.Fatalf("read state moved on a refusal: %v", w.rooms.readTo)
	}
}

func relayedWith(sender int64, msgID int64, a Attachment) *Message {
	m := &Message{ID: msgID, RoomID: mirrorRoom, SenderID: sender, CreatedAt: time.Now()}
	m.SetAttachments([]Attachment{a})
	return m
}

func TestARelayedMessageKeepsOnlyItsSendersOwnUpload(t *testing.T) {
	other := int64(902)
	this := int64(901)
	cases := []struct {
		name string
		edit func(row *AttachmentRow, a *Attachment, sender *int64)
		keep bool
	}{
		{"the sender's upload", func(*AttachmentRow, *Attachment, *int64) {}, true},
		{"already claimed by this message", func(row *AttachmentRow, _ *Attachment, _ *int64) { row.MessageID = &this }, true},
		{"claimed by another message", func(row *AttachmentRow, _ *Attachment, _ *int64) { row.MessageID = &other }, false},
		{"someone else's upload", func(_ *AttachmentRow, _ *Attachment, sender *int64) { *sender = alice }, false},
		{"an id with another file's URL", func(_ *AttachmentRow, a *Attachment, _ *int64) { a.URL = cdn + "/avatars/somebody.png" }, false},
		{"no registry row", func(_ *AttachmentRow, a *Attachment, _ *int64) { a.ID = "71" }, false},
		{"a message with no sender", func(_ *AttachmentRow, _ *Attachment, sender *int64) { *sender = 0 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMirrorWorld()
			w.withCDN(t)
			a := Attachment{ID: id.Format(bobsUpload), URL: bobsFileURL}
			sender := int64(bob)
			tc.edit(w.repo.uploads[bobsUpload], &a, &sender)
			m := relayedWith(sender, 901, a)
			if err := w.svc.CreateFederated(context.Background(), mirrorRoom, []int64{alice, bob}, m); err != nil {
				t.Fatal(err)
			}
			if kept := len(w.repo.stored[901].Attachments()) == 1; kept != tc.keep {
				t.Fatalf("kept = %v, want %v", kept, tc.keep)
			}
		})
	}
}

// A file on someone else's CDN was never in question.
func TestARelayedMessageKeepsFilesFromElsewhere(t *testing.T) {
	w := newMirrorWorld()
	w.withCDN(t)
	m := relayedWith(carol, 901, Attachment{ID: "71", URL: "https://cdn.c.example/x.png"})
	if err := w.svc.CreateFederated(context.Background(), mirrorRoom, nil, m); err != nil {
		t.Fatal(err)
	}
	if len(w.repo.stored[901].Attachments()) != 1 {
		t.Fatal("a foreign attachment was dropped")
	}
}

// What came from the origin is what the local send gets, so the sender's own instance shows the
// file: the reply has its attachment, as does the copy read during an origin outage.
func TestAnUploadReachesTheSendersInstanceThroughTheReply(t *testing.T) {
	w := newMirrorWorld()
	w.withCDN(t)
	w.fed.create = func(_ *CreateMessageInput, attachments []Attachment) (*Message, error) {
		m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: bob}
		m.SetAttachments(attachments)
		if err := w.svc.CreateFederated(context.Background(), mirrorRoom, []int64{alice, bob}, m); err != nil {
			t.Fatal(err)
		}
		return m, nil
	}
	m, err := w.svc.Create(context.Background(), bob, mirrorRoom, &CreateMessageInput{Attachments: []string{id.Format(bobsUpload)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Attachments()) != 1 || len(w.repo.stored[901].Attachments()) != 1 {
		t.Fatalf("reply %v, stored %v", m.Attachments(), w.repo.stored[901].Attachments())
	}
}

func TestDeletingARelayedMessageRemovesTheSendersUploadHere(t *testing.T) {
	w := newMirrorWorld()
	deleted := w.withCDN(t)
	this := int64(901)
	w.repo.uploads[bobsUpload].MessageID = &this
	w.repo.stored[901] = relayedWith(bob, 901, Attachment{ID: id.Format(bobsUpload), URL: bobsFileURL})
	if err := w.svc.DeleteFederated(context.Background(), mirrorRoom, 901); err != nil {
		t.Fatal(err)
	}
	if len(*deleted) != 1 || (*deleted)[0] != "/v1/attachments/100/70-abc/cat.png" {
		t.Fatalf("blobs deleted: %v", *deleted)
	}
	if !slices.Contains(w.repo.dropped, bobsUpload) {
		t.Fatalf("rows dropped: %v", w.repo.dropped)
	}
}

// A relay names whatever it likes: a message from a stranger pointing at bob's file must not
// be how it goes away.
func TestDeletingARelayedMessageLeavesEveryoneElsesFiles(t *testing.T) {
	w := newMirrorWorld()
	deleted := w.withCDN(t)
	w.repo.stored[901] = relayedWith(carol, 901, Attachment{ID: id.Format(bobsUpload), URL: bobsFileURL})
	if err := w.svc.DeleteFederated(context.Background(), mirrorRoom, 901); err != nil {
		t.Fatal(err)
	}
	if len(*deleted) != 0 || len(w.repo.dropped) != 0 {
		t.Fatalf("blobs %v, rows %v", *deleted, w.repo.dropped)
	}
}

// SoftDelete is an UPDATE, and a channel's delete reaches mirrors that never stored the message:
// writing it would make a row out of nothing, in every room a peer has no viewer of.
func TestDeletingAMessageNeverStoredHereWritesNothing(t *testing.T) {
	w := newMirrorWorld()
	rdb, published := redistest.New()
	w.svc.redis = rdb
	if err := w.svc.DeleteFederated(context.Background(), mirrorRoom, 901); err != nil {
		t.Fatal(err)
	}
	if len(w.repo.softDeleted) != 0 {
		t.Fatalf("wrote %v", w.repo.softDeleted)
	}
	if got := published.Published(); len(got) != 0 {
		t.Fatalf("published %+v", got)
	}
}

type countingHooks struct {
	created, deleted int
}

func (h *countingHooks) OnThreadMessage(context.Context, *rooms.Room, int64, []int64) { h.created++ }
func (h *countingHooks) OnThreadMessageDeleted(context.Context, *rooms.Room)          { h.deleted++ }

// The engine's messages service has the thread hooks too, now the origin needs them. A mirror's
// copy of a message never counted it in, so its delete must not count it out.
func TestARelayedMessageDoesNotTouchAThreadsCount(t *testing.T) {
	w := newMirrorWorld()
	hooks := &countingHooks{}
	w.svc.SetThreadHooks(hooks)
	w.rooms.room.Type = rooms.TypeThread
	m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, Plaintext: "hi", CreatedAt: time.Now()}
	if err := w.svc.CreateFederated(context.Background(), mirrorRoom, nil, m); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.DeleteFederated(context.Background(), mirrorRoom, 901); err != nil {
		t.Fatal(err)
	}
	if hooks.created != 0 || hooks.deleted != 0 {
		t.Fatalf("hooks ran: %+v", hooks)
	}
}

// ---- role mentions and read state -----------------------------------------------------------

func TestARelayedRoleMentionCounts(t *testing.T) {
	w := newMirrorWorld()
	m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, Plaintext: "<@&50>", MentionRoles: []int64{mods}, Mentions: []int64{alice}, CreatedAt: time.Now()}
	if err := w.svc.CreateFederated(context.Background(), mirrorRoom, []int64{alice, bob, carol}, m); err != nil {
		t.Fatal(err)
	}
	slices.Sort(w.rooms.incremented)
	if want := []int64{alice, bob}; !slices.Equal(w.rooms.incremented, want) {
		t.Fatalf("counted %v, want %v: the direct mention and the holder, never the sender", w.rooms.incremented, want)
	}
}

func TestARelayedRoleMentionPingsWhatItsSenderMay(t *testing.T) {
	for _, tc := range []struct {
		perms int64
		wide  bool
	}{{0, false}, {permissions.PermMentionEveryone, true}} {
		w := newMirrorWorld()
		w.auth.perms[carol] |= tc.perms
		m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, MentionRoles: []int64{mods}, CreatedAt: time.Now()}
		if err := w.svc.CreateFederated(context.Background(), mirrorRoom, nil, m); err != nil {
			t.Fatal(err)
		}
		if len(w.auth.wideRoles) == 0 || w.auth.wideRoles[0] != tc.wide {
			t.Fatalf("perms %d: expansions %v", tc.perms, w.auth.wideRoles)
		}
	}
}

func TestARelayedRoleMentionSendsTheBadge(t *testing.T) {
	w := newMirrorWorld()
	rdb, published := redistest.New()
	w.svc.redis = rdb
	m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, MentionRoles: []int64{mods}, CreatedAt: time.Now()}
	if err := w.svc.CreateFederated(context.Background(), mirrorRoom, []int64{alice, bob, carol}, m); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range published.Published() {
			if ev.Type == "MESSAGE_MENTION_UPDATE" {
				if ev.UserID != id.Format(bob) || ev.Data["message_id"] != "901" || ev.Data["mention_count"] != float64(1) {
					t.Fatalf("event %+v", ev)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no badge in %+v", published.Published())
}

// A role holder's phone has to hear of it as well: the worker tells a mention from noise by
// this list, and the relay carries role ids rather than people.
func TestARelayedRoleMentionIsPushedAsAMention(t *testing.T) {
	push.SetProducing(true)
	defer push.SetProducing(false)
	w := newMirrorWorld()
	rdb, mem := redistest.New()
	w.svc.redis = rdb
	mem.AddToSet("push:users", id.Format(alice), id.Format(bob))
	m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, Plaintext: "<@&50>", MentionRoles: []int64{mods}, CreatedAt: time.Now()}
	if err := w.svc.CreateFederated(context.Background(), mirrorRoom, []int64{alice, bob, carol}, m); err != nil {
		t.Fatal(err)
	}
	jobs := mem.Stream("push:jobs")
	if len(jobs) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	var job struct {
		Users   []int64 `json:"u"`
		Message struct {
			Mentioned []int64 `json:"mu"`
		} `json:"m"`
	}
	if err := json.Unmarshal([]byte(jobs[0]), &job); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(job.Message.Mentioned, []int64{bob}) || len(job.Users) != 2 {
		t.Fatalf("job %s", jobs[0])
	}
}

// The relay's count and the delete's rollback have to name the same people, or a delete eats
// someone's real mention.
func TestARelayedDeleteTakesBackWhatTheRelayCounted(t *testing.T) {
	w := newMirrorWorld()
	m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, MentionRoles: []int64{mods}, CreatedAt: time.Now()}
	if err := w.svc.CreateFederated(context.Background(), mirrorRoom, []int64{alice, bob, carol}, m); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.DeleteFederated(context.Background(), mirrorRoom, 901); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.rooms.incremented, w.rooms.decremented) {
		t.Fatalf("counted %v, took back %v", w.rooms.incremented, w.rooms.decremented)
	}
}

// A copy stored from history was never counted here: deleting it must not take back a mention
// someone still has unread in the room. The relayed one, counted, is taken back.
func TestADeleteTakesBackOnlyWhatWasCountedHere(t *testing.T) {
	w := newMirrorWorld()
	rdb, _ := redistest.New()
	w.svc.redis = rdb
	ctx := context.Background()
	w.rooms.totals[bob] = 1 // a real mention, still unread
	w.repo.stored[700] = &Message{ID: 700, RoomID: mirrorRoom, SenderID: carol, Mentions: []int64{bob}, CreatedAt: time.Now()}
	if err := w.svc.DeleteFederated(ctx, mirrorRoom, 700); err != nil {
		t.Fatal(err)
	}
	if len(w.rooms.decremented) != 0 {
		t.Fatalf("a history copy took back %v", w.rooms.decremented)
	}
	relayed := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, Mentions: []int64{bob}, CreatedAt: time.Now()}
	if err := w.svc.CreateFederated(ctx, mirrorRoom, []int64{alice, bob, carol}, relayed); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.DeleteFederated(ctx, mirrorRoom, 901); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.rooms.incremented, w.rooms.decremented) {
		t.Fatalf("counted %v, took back %v", w.rooms.incremented, w.rooms.decremented)
	}
}

// Without the ack and the baseline, the sender's other devices keep the room unread and an old
// mention stays on its badge: that is what a local send does and a mirror's did not.
func TestAMirrorSendReadsTheRoomForItsSender(t *testing.T) {
	w := newMirrorWorld()
	rdb, published := redistest.New()
	w.svc.redis = rdb
	w.rooms.totals[bob] = 4
	w.fed.create = func(*CreateMessageInput, []Attachment) (*Message, error) {
		return &Message{ID: 901, RoomID: mirrorRoom, SenderID: bob}, nil
	}
	if _, err := w.svc.Create(context.Background(), bob, mirrorRoom, &CreateMessageInput{Plaintext: "hello"}); err != nil {
		t.Fatal(err)
	}
	if w.rooms.readTo[bob] != 901 || w.rooms.baselines[bob] != 4 {
		t.Fatalf("read to %d, baseline %d", w.rooms.readTo[bob], w.rooms.baselines[bob])
	}
	var acks int
	for _, ev := range published.Published() {
		if ev.Type == "MESSAGE_ACK" && ev.UserID == id.Format(bob) && ev.Data["last_read_message_id"] == "901" {
			acks++
		}
	}
	if acks != 1 {
		t.Fatalf("events %+v", published.Published())
	}
}

// ---- automod -----------------------------------------------------------------------------------

func TestInviteRuleKnowsThisInstancesOwnDomain(t *testing.T) {
	cases := []struct {
		name, text string
		own        bool
		foreign    bool
		asked      bool
	}{
		{name: "this space's, with this domain", text: "https://a.example/invite/abc123@a.example", own: true, asked: true},
		{name: "any case", text: "https://a.example/invite/abc123@A.Example", own: true, asked: true},
		{name: "escaped @", text: "https://a.example/invite/abc123%40a.example", own: true, asked: true},
		{name: "with a port, escaped", text: "https://a.example/invite/abc123%40a.example%3A8443", own: false, foreign: true},
		{name: "another space here", text: "https://a.example/invite/abc123@a.example", foreign: true, asked: true},
		{name: "another instance", text: "https://b.example/invite/abc123@b.example", own: true, foreign: true},
		{name: "a domain that merely ends in ours", text: "https://b.example/invite/abc123@aa.example", own: true, foreign: true},
		{name: "no domain, this space's", text: "https://a.example/invite/abc123", own: true, asked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMirrorWorld()
			w.svc.cfg.Federation.Domain = "a.example"
			w.auth.ownInvites["abc123"] = tc.own
			if got := w.svc.hasForeignInvite(context.Background(), mirrorSpace, tc.text); got != tc.foreign {
				t.Fatalf("foreign = %v", got)
			}
			if asked := len(w.auth.askedInvite) > 0; asked != tc.asked {
				t.Fatalf("asked = %v", asked)
			}
		})
	}
}

func TestInviteRuleWithAPortInTheDomain(t *testing.T) {
	w := newMirrorWorld()
	w.svc.cfg.Federation.Domain = "a.example:8443"
	w.auth.ownInvites["abc123"] = true
	if w.svc.hasForeignInvite(context.Background(), mirrorSpace, "https://a.example/invite/abc123%40a.example%3a8443") {
		t.Fatal("read this instance's own invite as foreign")
	}
}

// An instance that does not federate has no domain, and no suffix is its own.
func TestInviteRuleWithoutFederation(t *testing.T) {
	w := newMirrorWorld()
	w.auth.ownInvites["abc123"] = true
	if !w.svc.hasForeignInvite(context.Background(), mirrorSpace, "https://a.example/invite/abc123@a.example") {
		t.Fatal("a code@domain counted as local on an instance with no domain")
	}
}
