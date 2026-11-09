package federation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
)

// A bot's embeds across instances: the stored form on every body that carries content,
// mentions translated like text, and what a peer sends held to the local rules.

// embedStore is memMessages with edits applied, as the real repository applies them.
type embedStore struct{ *memMessages }

func (s embedStore) Update(_ context.Context, _, msgID int64, ciphertext, plaintext string, embeds *string) (*messages.Message, error) {
	m := s.stored[msgID]
	if m == nil {
		return nil, nil
	}
	switch {
	case plaintext != "":
		m.Plaintext = plaintext
	case ciphertext != "":
		m.Ciphertext = ciphertext
	}
	if embeds != nil {
		m.EmbedsJSON = *embeds
	}
	return m, nil
}

// embedMirror is msgMirror with edits applied to the store, and b.example's member a bot.
func embedMirror(t *testing.T, app *fiber.App) *mirrorMsgs {
	t.Helper()
	m := msgMirror(t, app)
	users := m.s.users.(*profileTestUsers)
	users.byID[rpMemberB].Bot = true
	users.byID[mmShadow].Bot = true
	store := embedStore{m.store}
	m.s.msgRepo = store
	m.s.msgSvc = messages.NewService(store, m.s.roomRepo.(msgRooms), users, nil, &config.Config{}, m.s.spaceSvc)
	m.s.msgSvc.SetFederator(m.s)
	return m
}

func storedWithEmbeds(msgID, roomID, sender int64, list ...messages.Embed) *messages.Message {
	m := &messages.Message{ID: msgID, RoomID: roomID, SenderID: sender, Plaintext: "hi", CreatedAt: time.Now().UTC()}
	m.SetEmbeds(list)
	return m
}

// ---- out -----------------------------------------------------------------------------------

func TestEmbedsGoOutInTheirStoredForm(t *testing.T) {
	ctx := context.Background()
	w := vsOrigin(t)
	spyOn(w.s)
	m := storedWithEmbeds(901, vsLounge, rpBot, messages.Embed{
		Type:        messages.EmbedTypeRich,
		Title:       "<@5> is only text here",
		Description: "for <@5>",
		Fields:      []messages.EmbedField{{Name: "who", Value: "<@5>", Inline: true}},
		Image:       &messages.EmbedImage{URL: "https://img.example/a.png", ProxyURL: "https://a.example/media/v1/external/x/y", Width: 640, Height: 480},
		Author:      &messages.EmbedAuthor{Name: "bot", IconURL: "https://img.example/i.png", ProxyIconURL: "https://a.example/media/v1/external/z/w"},
	})

	ev := w.s.messageEvent(ctx, hostedRef(vsLounge), m, nil)
	if len(ev.Embeds) != 1 {
		t.Fatalf("embeds %+v", ev.Embeds)
	}
	e := ev.Embeds[0]
	if e.Type != "" || e.Image.ProxyURL != "" || e.Author.ProxyIconURL != "" {
		t.Errorf("served fields on the wire: %+v", e)
	}
	if e.Image.Width != 640 || e.Image.Height != 480 {
		t.Errorf("the sizes the origin learned were dropped: %+v", e.Image)
	}
	if e.Description != "for <@@50:b.example>" || e.Fields[0].Value != "<@@50:b.example>" {
		t.Errorf("mentions not translated: %q / %q", e.Description, e.Fields[0].Value)
	}
	if e.Title != "<@5> is only text here" || e.Fields[0].Name != "who" {
		t.Errorf("a title or field name was rewritten: %+v", e)
	}
	if got := m.Embeds()[0]; got.Description != "for <@5>" || got.Fields[0].Value != "<@5>" {
		t.Errorf("the stored copy changed: %+v", got)
	}

	// An encrypted message never has any, whatever its row says.
	enc := storedWithEmbeds(902, vsLounge, rpBot, messages.Embed{Title: "t"})
	enc.Plaintext, enc.Ciphertext = "", "opaque"
	if ev := w.s.messageEvent(ctx, hostedRef(vsLounge), enc, nil); ev.Embeds != nil {
		t.Errorf("an encrypted message's embeds went out: %+v", ev.Embeds)
	}
	raw, _ := json.Marshal(w.s.messageEvent(ctx, hostedRef(vsLounge), &messages.Message{ID: 903, RoomID: vsLounge, SenderID: rpBot, Plaintext: "plain"}, nil))
	if strings.Contains(string(raw), `"embeds"`) {
		t.Errorf("a message without embeds carries the field: %s", raw)
	}
}

// An edit relay is the message's whole content: the list as it is now, [] for none, and
// nothing at all for an encrypted message.
func TestAnEditRelayCarriesTheWholeList(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		msg  *messages.Message
		want string // the embeds field as sent; "" for absent
	}{
		{"with embeds", storedWithEmbeds(901, vsLounge, rpBot, messages.Embed{Description: "hey <@5>"}), `[{"description":"hey <@@50:b.example>"}]`},
		{"without", storedWithEmbeds(901, vsLounge, rpBot), `[]`},
		{"encrypted", &messages.Message{ID: 901, RoomID: vsLounge, SenderID: rpBot, Ciphertext: "opaque"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := vsOrigin(t)
			spy := spyOn(w.s)
			w.s.AfterMessageEdited(ctx, vsLounge, nil, tc.msg)
			sent := spy.to("b.example")
			if len(sent) != 1 {
				t.Fatalf("sent %+v", spy.sent)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(sent[0].Body), &body); err != nil {
				t.Fatal(err)
			}
			if got := wireEmbeds(t, body["embeds"]); got != tc.want {
				t.Errorf("embeds %s, want %s", got, tc.want)
			}
		})
	}
}

// wireEmbeds is an embeds field as sent, decoded and written again without HTML escaping, so
// that a test can say what crossed plainly; "" when the field was left out.
func wireEmbeds(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if raw == nil {
		return ""
	}
	var list []messages.Embed
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(list); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(b.String())
}

func TestWithEmbedText(t *testing.T) {
	list := []messages.Embed{{Title: "<:a:1>", Description: "d", Fields: []messages.EmbedField{{Name: "n", Value: "<:b:2>"}}}}
	if got := withEmbedText("hi", list); got != "hi\n<:a:1>\nd\nn\n<:b:2>" {
		t.Errorf("got %q", got)
	}
	if got := withEmbedText("hi", nil); got != "hi" {
		t.Errorf("got %q", got)
	}
}

// ---- in: what a mirror keeps ----------------------------------------------------------------

func TestAMirrorKeepsTheEmbedsTheLocalRulesKeep(t *testing.T) {
	ctx := context.Background()
	good := messages.Embed{
		Title:       "  t ",
		URL:         "HTTPS://Img.Example/page#frag",
		Description: "hi <@@50:b.example> and <@@77:a.example>",
		Fields:      []messages.EmbedField{{Name: "<@@77:a.example>", Value: "<@@77:a.example>"}},
		Image:       &messages.EmbedImage{URL: "https://img.example/a.png", ProxyURL: "https://evil.example/x", Width: 640, Height: 480},
	}

	t.Run("a bot's are kept, checked and translated", func(t *testing.T) {
		m := embedMirror(t, fiber.New())
		ev := m.event(901, mmShadow, "text")
		ev.Embeds = []messages.Embed{good}
		msg, err := m.s.messageFromEvent(ctx, "a.example", m.sc, ev)
		if err != nil {
			t.Fatal(err)
		}
		got := msg.Embeds()
		if len(got) != 1 {
			t.Fatalf("embeds %+v", got)
		}
		e := got[0]
		if e.Title != "t" || e.URL != "https://img.example/page" {
			t.Errorf("not held to the local rules: %+v", e)
		}
		if e.Description != "hi <@50> and <@7700>" || e.Fields[0].Value != "<@7700>" || e.Fields[0].Name != "<@@77:a.example>" {
			t.Errorf("mentions: %q / %q / %q", e.Description, e.Fields[0].Value, e.Fields[0].Name)
		}
		if e.Image.ProxyURL != "" || e.Image.Width != 640 || e.Image.Height != 480 {
			t.Errorf("image %+v", e.Image)
		}
	})

	dropped := []struct {
		name  string
		setup func(m *mirrorMsgs, ev *MessageEvent)
	}{
		{"a person's message", func(m *mirrorMsgs, ev *MessageEvent) {
			person := m.s.users.(*profileTestUsers).byID[rpMemberB]
			person.Bot = false
			p := m.s.ProfileOf(person)
			ev.Sender = &p
		}},
		{"a notice", func(_ *mirrorMsgs, ev *MessageEvent) {
			ev.Sender, ev.SystemType, ev.Plaintext = nil, "space_member_join", ""
		}},
		{"ciphertext", func(_ *mirrorMsgs, ev *MessageEvent) { ev.Plaintext, ev.Ciphertext = "", "opaque" }},
		{"an encrypted room", func(m *mirrorMsgs, _ *MessageEvent) {
			on := true
			m.sc.room.E2EEEnabled = &on
		}},
		{"an attachment:// from a peer", func(_ *mirrorMsgs, ev *MessageEvent) {
			ev.Embeds = []messages.Embed{{Image: &messages.EmbedImage{URL: "attachment://chart.png"}}}
		}},
		{"over a limit", func(_ *mirrorMsgs, ev *MessageEvent) {
			ev.Embeds = []messages.Embed{good, {Title: strings.Repeat("x", messages.MaxEmbedTitleRunes+1)}}
		}},
		{"a script link", func(_ *mirrorMsgs, ev *MessageEvent) {
			ev.Embeds = []messages.Embed{{Title: "t", URL: "javascript:alert(1)"}}
		}},
		{"nothing to show", func(_ *mirrorMsgs, ev *MessageEvent) {
			ev.Embeds = []messages.Embed{{Color: new(int)}}
		}},
	}
	for _, tc := range dropped {
		t.Run(tc.name, func(t *testing.T) {
			m := embedMirror(t, fiber.New())
			ev := m.event(901, mmShadow, "text")
			ev.Embeds = []messages.Embed{good}
			tc.setup(m, &ev)
			msg, err := m.s.messageFromEvent(ctx, "a.example", m.sc, ev)
			if err != nil {
				t.Fatalf("the message itself was refused: %v", err)
			}
			if msg.EmbedsJSON != "" {
				t.Errorf("kept %s", msg.EmbedsJSON)
			}
		})
	}

	// News goes through the same door, and is stored as it comes out of it.
	t.Run("news", func(t *testing.T) {
		m := embedMirror(t, fiber.New())
		ev := m.event(901, rpMemberB, "text")
		ev.Embeds = []messages.Embed{good}
		if _, err := m.s.applyMessageEvent(ctx, "a.example", m.sc, ev); err != nil {
			t.Fatal(err)
		}
		if got := m.store.stored[901].Embeds(); len(got) != 1 || got[0].Description != "hi <@50> and <@7700>" {
			t.Fatalf("stored %+v", got)
		}
		bad := m.event(902, rpMemberB, "still here")
		bad.Embeds = []messages.Embed{{Image: &messages.EmbedImage{URL: "attachment://chart.png"}}}
		if _, err := m.s.applyMessageEvent(ctx, "a.example", m.sc, bad); err != nil {
			t.Fatal(err)
		}
		if got := m.store.stored[902]; got == nil || got.Plaintext != "still here" || got.EmbedsJSON != "" {
			t.Fatalf("stored %+v", got)
		}
	})
}

// The limits count an embed as it is stored here, mentions translated, not its wire form, in
// which every mention is longer: an embed the origin accepted at a limit is kept.
func TestAMirrorCountsTheLimitsOnTheLocalForm(t *testing.T) {
	ctx := context.Background()
	const mention, local = "<@@77:a.example>", "<@7700>"
	pad := func(c string, n int) string { return strings.Repeat(c, n-len(local)) + mention }
	for name, list := range map[string][]messages.Embed{
		"a field value at its limit": {{Title: "t", Fields: []messages.EmbedField{{Name: "n", Value: pad("x", messages.MaxEmbedFieldValueRunes)}}}},
		"a description at its limit": {{Description: pad("x", messages.MaxEmbedDescriptionRunes)}},
		"the total at its limit": {
			{Description: pad("x", messages.MaxEmbedDescriptionRunes)},
			{Description: pad("y", messages.MaxEmbedTotalRunes-messages.MaxEmbedDescriptionRunes)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := embedMirror(t, fiber.New())
			ev := m.event(901, mmShadow, "text")
			ev.Embeds = list
			msg, err := m.s.messageFromEvent(ctx, "a.example", m.sc, ev)
			if err != nil {
				t.Fatal(err)
			}
			got := msg.Embeds()
			if len(got) != len(list) {
				t.Fatalf("kept %d of %d embeds", len(got), len(list))
			}
			if !strings.HasSuffix(got[0].Description+valueOf(got[0]), local) {
				t.Errorf("not translated: %+v", got[0])
			}
		})
	}
}

func valueOf(e messages.Embed) string {
	if len(e.Fields) == 0 {
		return ""
	}
	return e.Fields[0].Value
}

// Wire text that could not fit however its mentions came out is refused before any of them is
// looked up.
func TestWireEmbedsTooLongAreRefusedBeforeLookups(t *testing.T) {
	s, _ := mirrorWireFixture()
	users := &countingUsers{profileTestUsers: s.users.(*profileTestUsers)}
	s.users = users
	long := strings.Repeat("x", messages.MaxEmbedDescriptionRunes) + "<@@999:b.example>"
	_, err := s.localEmbeds(context.Background(), []messages.Embed{{Description: long}, {Description: long}, {Description: long}})
	if !errors.Is(err, messages.ErrInvalidEmbed) {
		t.Fatalf("got %v", err)
	}
	if users.byIDCalls != 0 {
		t.Fatalf("%d lookups for embeds that were refused", users.byIDCalls)
	}
}

// One budget of lookups for all of a message's embeds, as one text has.
func TestEmbedMentionLookupsAreBounded(t *testing.T) {
	s, _ := mirrorWireFixture()
	users := &countingUsers{profileTestUsers: s.users.(*profileTestUsers)}
	s.users = users
	var fields []messages.EmbedField
	for i := 1; i <= 30; i++ {
		fields = append(fields, messages.EmbedField{Name: "n", Value: "<@@" + id.Format(int64(1000+i)) + ":b.example>"})
	}
	s.embedsFromWire(context.Background(), []messages.Embed{{Description: "<@@999:b.example>", Fields: fields}})
	if users.byIDCalls != maxMentionLookups {
		t.Fatalf("%d lookups, want %d", users.byIDCalls, maxMentionLookups)
	}
}

type countingUsers struct {
	*profileTestUsers
	byIDCalls int
}

func (u *countingUsers) GetByID(ctx context.Context, uid int64) (*auth.User, error) {
	u.byIDCalls++
	return u.profileTestUsers.GetByID(ctx, uid)
}

func TestARelayedEditReplacesTheWholeList(t *testing.T) {
	ref := RoomRef{OriginDomain: "a.example", OriginRoomID: id.Format(mmOrigin)}
	msgRef := func(msgID int64) MessageRef {
		return MessageRef{OriginDomain: "a.example", OriginMessageID: id.Format(msgID)}
	}
	list := func(e ...messages.Embed) *[]messages.Embed {
		out := append([]messages.Embed{}, e...)
		return &out
	}

	m := embedMirror(t, fiber.New())
	m.store.stored[901] = storedWithEmbeds(901, mmRoom, mmShadow, messages.Embed{Title: "A"})
	app := voiceRoute("a.example", NewHandler(m.s).MessageEdit)
	edit := func(t *testing.T, body MessageEdit) {
		t.Helper()
		if got := post(t, app, body); got != http.StatusNoContent {
			t.Fatalf("status %d", got)
		}
	}

	edit(t, MessageEdit{Room: ref, Message: msgRef(901), Plaintext: "hi", Embeds: list(messages.Embed{Title: "B", Description: "for <@@50:b.example>"})})
	if got := m.store.stored[901].Embeds(); len(got) != 1 || got[0].Title != "B" || got[0].Description != "for <@50>" {
		t.Fatalf("replaced: %+v", got)
	}
	// An origin from before embeds sends none, and the copy keeps its own.
	edit(t, MessageEdit{Room: ref, Message: msgRef(901), Plaintext: "edited"})
	if got := m.store.stored[901]; got.Plaintext != "edited" || len(got.Embeds()) != 1 {
		t.Fatalf("an older origin's edit: %+v", got)
	}
	edit(t, MessageEdit{Room: ref, Message: msgRef(901), Plaintext: "edited", Embeds: list()})
	if got := m.store.stored[901].EmbedsJSON; got != "" {
		t.Fatalf("cleared: %s", got)
	}
	edit(t, MessageEdit{Room: ref, Message: msgRef(901), Plaintext: "edited", Embeds: list(messages.Embed{Image: &messages.EmbedImage{URL: "attachment://x.png"}})})
	if got := m.store.stored[901]; got.EmbedsJSON != "" || got.Plaintext != "edited" {
		t.Fatalf("an attachment:// from a peer: %+v", got)
	}

	// A person's message cannot be given a bot's card by an edit.
	m.s.users.(*profileTestUsers).byID[rpMemberB].Bot = false
	m.store.stored[902] = storedWithEmbeds(902, mmRoom, rpMemberB)
	edit(t, MessageEdit{Room: ref, Message: msgRef(902), Plaintext: "hi", Embeds: list(messages.Embed{Title: "B"})})
	if got := m.store.stored[902].EmbedsJSON; got != "" {
		t.Fatalf("a person's message got %s", got)
	}
}

// ---- a mirror's writes, and the origin's answer ----------------------------------------------

func TestAMirrorWritesEmbedsThroughTheOrigin(t *testing.T) {
	ctx := context.Background()
	var (
		created   SpaceMessageRequest
		editRaw   map[string]json.RawMessage
		originHas []messages.Embed
		mirror    *mirrorMsgs
	)
	answer := func(msgID int64, text string) MessageEvent {
		ev := mirror.event(msgID, rpMemberB, text)
		ev.Embeds = originHas
		return ev
	}
	app := fiber.New()
	app.Post(BasePath+"/spaces/messages", func(c fiber.Ctx) error {
		if err := json.Unmarshal(c.Body(), &created); err != nil {
			return err
		}
		originHas = created.Embeds
		return c.Status(http.StatusCreated).JSON(answer(905, created.Plaintext))
	})
	app.Patch(BasePath+"/spaces/messages", func(c fiber.Ctx) error {
		var req SpaceMessageEditRequest
		editRaw = nil
		if err := json.Unmarshal(c.Body(), &editRaw); err != nil {
			return err
		}
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return err
		}
		if req.Embeds != nil {
			originHas = *req.Embeds
		}
		return c.JSON(answer(905, req.Plaintext))
	})
	mirror = embedMirror(t, app)
	room := mirror.sc.room

	m, err := mirror.s.CreateRemote(ctx, "a.example", room, rpMemberB, &messages.CreateMessageInput{
		Plaintext: "hi",
		Embeds:    messages.EmbedList{Set: true, List: []messages.Embed{{Title: "T", Description: "for <@7700>"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Embeds) != 1 || created.Embeds[0].Description != "for <@@77:a.example>" {
		t.Fatalf("the origin was sent %+v", created.Embeds)
	}
	if got := m.Embeds(); len(got) != 1 || got[0].Description != "for <@7700>" {
		t.Fatalf("the copy from the origin's answer: %+v", got)
	}

	edit := func(t *testing.T, in *messages.EditMessageInput) *messages.Message {
		t.Helper()
		got, err := mirror.s.EditRemote(ctx, "a.example", room, rpMemberB, 905, in)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := edit(t, &messages.EditMessageInput{Plaintext: "edited"})
	if _, sent := editRaw["embeds"]; sent {
		t.Errorf("a text edit sent embeds: %s", editRaw["embeds"])
	}
	if got.Plaintext != "edited" || len(got.Embeds()) != 1 {
		t.Errorf("a text edit: %+v", got)
	}
	got = edit(t, &messages.EditMessageInput{Embeds: messages.EmbedList{Set: true, List: []messages.Embed{{Title: "U", Fields: []messages.EmbedField{{Name: "n", Value: "<@7700>"}}}}}})
	if wireEmbeds(t, editRaw["embeds"]) != `[{"title":"U","fields":[{"name":"n","value":"<@@77:a.example>"}]}]` {
		t.Errorf("a replacement sent %s", wireEmbeds(t, editRaw["embeds"]))
	}
	if e := got.Embeds(); len(e) != 1 || e[0].Title != "U" || e[0].Fields[0].Value != "<@7700>" {
		t.Errorf("a replacement: %+v", e)
	}
	got = edit(t, &messages.EditMessageInput{Embeds: messages.EmbedList{Set: true}})
	if wireEmbeds(t, editRaw["embeds"]) != `[]` {
		t.Errorf("a clear sent %s", wireEmbeds(t, editRaw["embeds"]))
	}
	if got.EmbedsJSON != "" {
		t.Errorf("a clear left %s", got.EmbedsJSON)
	}
}

// ---- the origin judges a mirror's write ------------------------------------------------------

// postBody is post, with the answer's body.
func postBody(t *testing.T, app *fiber.App, body any) (int, map[string]json.RawMessage) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "/voice", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(out, &fields)
	return res.StatusCode, fields
}

func TestTheOriginJudgesAMirrorsEmbeds(t *testing.T) {
	w := vsOrigin(t)
	spyOn(w.s)
	mem := &memMessages{stored: map[int64]*messages.Message{}}
	store := embedStore{mem}
	w.s.msgRepo = store
	w.s.msgSvc = messages.NewService(store, msgRooms{w.s.roomRepo.(*vsRooms)}, w.users, nil, &config.Config{}, w.s.spaceSvc)
	w.s.msgSvc.SetFederator(w.s)
	h := NewHandler(w.s)
	create := voiceRoute("b.example", h.SpaceMessageCreate)
	bot, person := FormatFID(rpBotB, "b.example"), FormatFID(rpMemberB, "b.example")
	send := func(user string, embeds ...messages.Embed) (int, map[string]json.RawMessage) {
		return postBody(t, create, SpaceMessageRequest{Room: hostedRef(vsText), User: user, Plaintext: "hi", Embeds: embeds})
	}
	errorOf := func(body map[string]json.RawMessage) string {
		var s string
		_ = json.Unmarshal(body["error"], &s)
		return s
	}

	status, body := send(bot, messages.Embed{Title: " T ", Description: "for <@@50:b.example>"})
	if status != http.StatusCreated {
		t.Fatalf("status %d: %v", status, body)
	}
	var reply []messages.Embed
	_ = json.Unmarshal(body["embeds"], &reply)
	if len(reply) != 1 || reply[0].Title != "T" || reply[0].Description != "for <@@50:b.example>" {
		t.Fatalf("answered %s", body["embeds"])
	}
	var stored *messages.Message
	for _, m := range mem.stored {
		stored = m
	}
	if got := stored.Embeds(); len(got) != 1 || got[0].Description != "for <@5>" {
		t.Fatalf("stored %+v", got)
	}

	// At a limit as it is stored here, longer on the wire: accepted, as the mirror accepted it.
	atLimit := strings.Repeat("x", messages.MaxEmbedFieldValueRunes-len("<@5>")) + "<@@50:b.example>"
	status, body = send(bot, messages.Embed{Title: "T", Fields: []messages.EmbedField{{Name: "n", Value: atLimit}}})
	if status != http.StatusCreated {
		t.Fatalf("a field value at its limit: %d %v", status, body)
	}

	status, body = send(bot, messages.Embed{Image: &messages.EmbedImage{URL: "attachment://chart.png"}})
	var code, path string
	_ = json.Unmarshal(body["code"], &code)
	_ = json.Unmarshal(body["path"], &path)
	if status != http.StatusBadRequest || code != "invalid_embed" || path != "embeds[0].image.url" {
		t.Errorf("attachment:// from a mirror: %d %v", status, body)
	}
	if status, body := send(person, messages.Embed{Title: "T"}); status != http.StatusBadRequest || errorOf(body) != messages.ErrEmbedsForBots.Error() {
		t.Errorf("a person's embeds: %d %s", status, body["error"])
	}

	// The edit, as a local one: absent keeps, a list replaces, [] clears.
	edit := voiceRoute("b.example", h.SpaceMessageEdit)
	ref := MessageRef{OriginDomain: "a.example", OriginMessageID: id.Format(stored.ID)}
	editWith := func(text string, embeds *[]messages.Embed) (int, map[string]json.RawMessage) {
		return postBody(t, edit, SpaceMessageEditRequest{Room: hostedRef(vsText), Message: ref, User: bot, Plaintext: text, Embeds: embeds})
	}
	if status, body := editWith("edited", nil); status != http.StatusOK || len(stored.Embeds()) != 1 || stored.Plaintext != "edited" {
		t.Fatalf("a text edit: %d %v / %+v", status, body, stored)
	}
	if status, body := editWith("", &[]messages.Embed{{Image: &messages.EmbedImage{URL: "attachment://chart.png"}}}); status != http.StatusBadRequest || len(stored.Embeds()) != 1 {
		t.Errorf("an attachment:// edit: %d %v", status, body)
	}
	if status, body := editWith("", &[]messages.Embed{{Title: "U", Fields: []messages.EmbedField{{Name: "n", Value: "<@@50:b.example>"}}}}); status != http.StatusOK ||
		stored.Embeds()[0].Title != "U" || stored.Embeds()[0].Fields[0].Value != "<@5>" {
		t.Errorf("a replacement: %d %v / %+v", status, body, stored.Embeds())
	}
	if status, body := editWith("", &[]messages.Embed{}); status != http.StatusOK || stored.EmbedsJSON != "" || stored.Plaintext != "edited" {
		t.Errorf("a clear: %d %v / %+v", status, body, stored)
	}

	// An encrypted channel refuses them as it would a local bot's.
	on := true
	w.s.roomRepo.(*vsRooms).byID[vsText].E2EEEnabled = &on
	if status, body := send(bot, messages.Embed{Title: "T"}); status != http.StatusBadRequest || errorOf(body) != messages.ErrEmbedsEncrypted.Error() {
		t.Errorf("an encrypted channel: %d %s", status, body["error"])
	}
}

// The refused field reaches the mirror's client, as a local refusal names it.
func TestAnEmbedRefusalNamesItsField(t *testing.T) {
	err := messageOriginError(&StatusError{Domain: "a.example", Status: http.StatusBadRequest,
		Body: `{"error":"embeds[0].image.url: names no file attached to this message","code":"invalid_embed","path":"embeds[0].image.url"}`}, "a.example", "/spaces/messages")
	var oe *messages.OriginError
	if !errors.As(err, &oe) || oe.Body["code"] != "invalid_embed" || oe.Body["path"] != "embeds[0].image.url" {
		t.Fatalf("got %v", err)
	}
}
