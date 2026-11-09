package messages

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/push"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/redistest"
)

// ---- decoding the request body -----------------------------------------------------------------

func TestEmbedListDecode(t *testing.T) {
	var in CreateMessageInput
	body := `{"embeds":[{"title":" Hi ","type":"video","provider":{"name":"x"},"video":{"url":"https://v.example"},
		"image":{"url":"https://a.example/i.png","width":5,"height":7,"proxy_url":"https://evil.example"},
		"author":{"name":"bot","proxy_icon_url":"https://evil.example"},
		"fields":[{"name":"a","value":"b","inline":true}],"color":16711680}]}`
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	if !in.Embeds.Set || len(in.Embeds.List) != 1 {
		t.Fatalf("embeds %+v", in.Embeds)
	}
	e := in.Embeds.List[0]
	if e.Title != " Hi " || e.Type != "" || e.Color == nil || *e.Color != 0xFF0000 || !e.Fields[0].Inline {
		t.Fatalf("embed %+v", e)
	}
	// What only the server sets is never read from a sender.
	if e.Image.Width != 0 || e.Image.Height != 0 || e.Image.ProxyURL != "" || e.Author.ProxyIconURL != "" {
		t.Fatalf("server-set fields came from the body: %+v %+v", e.Image, e.Author)
	}

	for body, want := range map[string]EmbedList{
		`{"plaintext":"x"}`: {},
		`{"embeds":null}`:   {Set: true},
		`{"embeds":[]}`:     {Set: true, List: []Embed{}},
	} {
		var in EditMessageInput
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			t.Fatal(err)
		}
		if in.Embeds.Set != want.Set || len(in.Embeds.List) != 0 {
			t.Errorf("%s: %+v, want %+v", body, in.Embeds, want)
		}
	}
	// Encoded (a forwarded edit), absent stays absent and a clear stays a clear.
	for want, in := range map[string]EditMessageInput{
		`{"plaintext":"x"}`:          {Plaintext: "x"},
		`{"embeds":[]}`:              {Embeds: EmbedList{Set: true}},
		`{"embeds":[{"title":"t"}]}`: {Embeds: embedsIn(Embed{Title: "t"})},
	} {
		if b, _ := json.Marshal(in); string(b) != want {
			t.Errorf("encoded %s, want %s", b, want)
		}
	}
}

func TestEmbedListDecodeNamesTheField(t *testing.T) {
	eleven := "[" + strings.TrimSuffix(strings.Repeat(`{"title":"x"},`, 11), ",") + "]"
	cases := map[string]string{
		`{"embeds":{}}`:                    "embeds",
		`{"embeds":"x"}`:                   "embeds",
		`{"embeds":` + eleven + `}`:        "embeds",
		`{"embeds":[1]}`:                   "embeds[0]",
		`{"embeds":[{"title":"a"},null]}`:  "embeds[1]",
		`{"embeds":[{"title":5}]}`:         "embeds[0].title",
		`{"embeds":[{"color":"red"}]}`:     "embeds[0].color",
		`{"embeds":[{"color":16777216}]}`:  "embeds[0].color",
		`{"embeds":[{"color":-1}]}`:        "embeds[0].color",
		`{"embeds":[{"color":1.5}]}`:       "embeds[0].color",
		`{"embeds":[{"footer":"x"}]}`:      "embeds[0].footer",
		`{"embeds":[{"image":{"url":7}}]}`: "embeds[0].image.url",
		`{"embeds":[{"fields":{}}]}`:       "embeds[0].fields",
		`{"embeds":[{"fields":[{"name":"a","value":"b"},{"name":"a","value":"b","inline":"yes"}]}]}`: "embeds[0].fields[1].inline",
	}
	for body, path := range cases {
		var in CreateMessageInput
		err := json.Unmarshal([]byte(body), &in)
		var eerr *EmbedError
		if !errors.As(err, &eerr) || !errors.Is(err, ErrInvalidEmbed) {
			t.Errorf("%s: err %v, want an EmbedError", body, err)
			continue
		}
		if eerr.Path != path {
			t.Errorf("%s: path %q, want %q", body, eerr.Path, path)
		}
	}
}

// ---- limits and the stored form ----------------------------------------------------------------

func embedPath(t *testing.T, err error) string {
	t.Helper()
	var eerr *EmbedError
	if err == nil {
		return ""
	}
	if !errors.As(err, &eerr) {
		t.Fatalf("err %v is not an EmbedError", err)
	}
	return eerr.Path
}

func TestNormalizeEmbedsLimits(t *testing.T) {
	fields := func(n int) []EmbedField {
		out := make([]EmbedField, n)
		for i := range out {
			out[i] = EmbedField{Name: "n", Value: "v"}
		}
		return out
	}
	r := strings.Repeat
	cases := []struct {
		name string
		list []Embed
		path string // "" = accepted
	}{
		{"title at the limit, in code points", []Embed{{Title: r("é", 256)}}, ""},
		{"title over", []Embed{{Title: r("a", 257)}}, "embeds[0].title"},
		{"whitespace around the title is not counted", []Embed{{Title: r(" ", 300) + r("a", 256) + "\n"}}, ""},
		{"description over", []Embed{{Description: r("a", 4097)}}, "embeds[0].description"},
		{"26 fields", []Embed{{Fields: fields(26)}}, "embeds[0].fields"},
		{"25 fields", []Embed{{Fields: fields(25)}}, ""},
		{"field value over", []Embed{{Fields: []EmbedField{{Name: "n", Value: r("a", 1025)}}}}, "embeds[0].fields[0].value"},
		{"field name over", []Embed{{Fields: []EmbedField{{Name: r("a", 257), Value: "v"}}}}, "embeds[0].fields[0].name"},
		{"field without a name", []Embed{{Fields: []EmbedField{{Name: "  ", Value: "v"}}}}, "embeds[0].fields[0].name"},
		{"field without a value", []Embed{{Fields: []EmbedField{{Name: "n"}}}}, "embeds[0].fields[0].value"},
		{"footer over", []Embed{{Footer: &EmbedFooter{Text: r("a", 2049)}}}, "embeds[0].footer.text"},
		{"footer with only an icon", []Embed{{Title: "t", Footer: &EmbedFooter{IconURL: "https://a.example/i.png"}}}, "embeds[0].footer.text"},
		{"author over", []Embed{{Author: &EmbedAuthor{Name: r("a", 257)}}}, "embeds[0].author.name"},
		{"author without a name", []Embed{{Author: &EmbedAuthor{URL: "https://a.example"}}}, "embeds[0].author.name"},
		{"image without a url", []Embed{{Title: "t", Image: &EmbedImage{}}}, "embeds[0].image.url"},
		{"nothing to show", []Embed{{Title: "t"}, {URL: "https://a.example", Color: new(int)}}, "embeds[1]"},
		{"only an image", []Embed{{Image: &EmbedImage{URL: "https://a.example/i.png"}}}, ""},
		{"6000 in all", []Embed{{Description: r("a", 3000)}, {Description: r("a", 3000)}}, ""},
		{"over 6000 in all", []Embed{{Description: r("a", 3000)}, {Description: r("a", 3000), Title: "x"}}, "embeds"},
		{"eleven embeds", make([]Embed, 11), "embeds"},
		{"colour out of range", []Embed{{Title: "t", Color: func() *int { v := 0x1000000; return &v }()}}, "embeds[0].color"},
		{"not a timestamp", []Embed{{Title: "t", Timestamp: "yesterday"}}, "embeds[0].timestamp"},
	}
	for _, tc := range cases {
		_, err := NormalizeEmbeds(tc.list, nil)
		if got := embedPath(t, err); got != tc.path {
			t.Errorf("%s: refused at %q (%v), want %q", tc.name, got, err, tc.path)
		}
	}
}

// The first look at a peer's embeds refuses only what the validator would refuse however
// their text shrank by slack: counts, and the total past the limit plus slack - never one
// field past its own limit, which translating a mention may bring back under it.
func TestCheckEmbedsWithin(t *testing.T) {
	fields := func(n int) []EmbedField {
		out := make([]EmbedField, n)
		for i := range out {
			out[i] = EmbedField{Name: "n", Value: "v"}
		}
		return out
	}
	r := strings.Repeat
	const slack = 100
	cases := []struct {
		name string
		list []Embed
		path string // "" = passed on to the validator
	}{
		{"eleven embeds", make([]Embed, 11), "embeds"},
		{"26 fields", []Embed{{Title: "t"}, {Fields: fields(26)}}, "embeds[1].fields"},
		{"a field past its limit", []Embed{{Fields: []EmbedField{{Name: "n", Value: r("a", MaxEmbedFieldValueRunes+slack)}}}}, ""},
		{"the total plus slack", []Embed{{Description: r("a", 3000)}, {Description: r("a", 3000+slack)}}, ""},
		{"past the total plus slack", []Embed{{Description: r("a", 3000)}, {Description: r("a", 3000+slack), Title: "x"}}, "embeds"},
		{"past it in an author line", []Embed{{Description: r("a", MaxEmbedTotalRunes+slack), Author: &EmbedAuthor{Name: "x"}}}, "embeds"},
		{"past it in a field", []Embed{{Description: r("a", MaxEmbedTotalRunes+slack-1), Fields: fields(1)}}, "embeds"},
		{"past it in a footer", []Embed{{Description: r("a", MaxEmbedTotalRunes+slack), Footer: &EmbedFooter{Text: "x"}}}, "embeds"},
		{"whitespace is not counted", []Embed{{Description: r(" ", 20000) + "a"}}, ""},
	}
	for _, tc := range cases {
		if got := embedPath(t, CheckEmbedsWithin(tc.list, slack)); got != tc.path {
			t.Errorf("%s: refused at %q, want %q", tc.name, got, tc.path)
		}
	}
}

const (
	embedCDN   = "https://cdn.b.example"
	chartURL   = embedCDN + "/v1/attachments/100/71-abc/chart.png"
	chartID    = 71
	notesID    = 72
	chartAgain = 73
)

func embedFiles() []Attachment {
	return []Attachment{
		{ID: id.Format(chartID), Filename: "chart.png", ContentType: "image/png", URL: chartURL, Width: 640, Height: 480},
		{ID: id.Format(notesID), Filename: "notes.txt", ContentType: "text/plain", URL: embedCDN + "/v1/attachments/100/72-abc/notes.txt"},
	}
}

func TestNormalizeEmbedURLs(t *testing.T) {
	one := func(e Embed, atts []Attachment) (Embed, string) {
		out, err := NormalizeEmbeds([]Embed{e}, atts)
		if p := embedPath(t, err); p != "" {
			return Embed{}, p
		}
		return out[0], ""
	}
	refusals := []struct {
		name string
		e    Embed
		atts []Attachment
		path string
	}{
		{"a script link", Embed{Title: "t", URL: "javascript:alert(1)"}, nil, "embeds[0].url"},
		{"credentials", Embed{Title: "t", URL: "https://user:pw@a.example/"}, nil, "embeds[0].url"},
		{"no host", Embed{Title: "t", URL: "https:///path"}, nil, "embeds[0].url"},
		{"an ftp image", Embed{Image: &EmbedImage{URL: "ftp://a.example/i.png"}}, nil, "embeds[0].image.url"},
		{"a data author link", Embed{Author: &EmbedAuthor{Name: "a", URL: "data:text/html,hi"}}, nil, "embeds[0].author.url"},
		{"a long URL", Embed{Title: "t", URL: "https://a.example/" + strings.Repeat("a", 2048)}, nil, "embeds[0].url"},
		{"attachment:// as a link", Embed{Title: "t", URL: "attachment://chart.png"}, embedFiles(), "embeds[0].url"},
		{"attachment:// of a text file", Embed{Image: &EmbedImage{URL: "attachment://notes.txt"}}, embedFiles(), "embeds[0].image.url"},
		{"attachment:// of nothing", Embed{Thumbnail: &EmbedImage{URL: "attachment://missing.png"}}, embedFiles(), "embeds[0].thumbnail.url"},
		{"attachment:// from a peer", Embed{Image: &EmbedImage{URL: "attachment://chart.png"}}, nil, "embeds[0].image.url"},
		{"attachment:// of two files", Embed{Image: &EmbedImage{URL: "attachment://chart.png"}},
			append(embedFiles(), Attachment{ID: id.Format(chartAgain), Filename: "chart.png", ContentType: "image/png", URL: embedCDN + "/x"}), "embeds[0].image.url"},
		{"attachment:// as a footer icon of nothing", Embed{Footer: &EmbedFooter{Text: "f", IconURL: "attachment://x.png"}}, embedFiles(), "embeds[0].footer.icon_url"},
	}
	for _, tc := range refusals {
		if _, p := one(tc.e, tc.atts); p != tc.path {
			t.Errorf("%s: refused at %q, want %q", tc.name, p, tc.path)
		}
	}

	e, p := one(Embed{
		Title:     "  t  ",
		URL:       "HTTPS://A.Example/Path?q=1#frag",
		Timestamp: "2026-11-02T10:00:00.5+02:00",
		Color:     new(int),
		Author:    &EmbedAuthor{Name: "a", IconURL: "attachment://" + id.Format(chartID)},
		Image:     &EmbedImage{URL: "attachment://chart.png"},
		Thumbnail: &EmbedImage{URL: "https://img.example/t.png", Width: 50, Height: 40},
		Footer:    &EmbedFooter{Text: " f ", IconURL: " https://img.example/f.png "},
	}, embedFiles())
	if p != "" {
		t.Fatalf("refused at %s", p)
	}
	if e.Title != "t" || e.URL != "https://a.example/Path?q=1" || e.Timestamp != "2026-11-02T08:00:00.5Z" {
		t.Errorf("text/url/timestamp: %q %q %q", e.Title, e.URL, e.Timestamp)
	}
	if e.Color == nil || *e.Color != 0 {
		t.Errorf("black was lost: %v", e.Color)
	}
	if e.Image.URL != chartURL || e.Image.Width != 640 || e.Image.Height != 480 || e.Author.IconURL != chartURL {
		t.Errorf("attachment:// not resolved: %+v %+v", e.Image, e.Author)
	}
	if e.Footer.Text != "f" || e.Footer.IconURL != "https://img.example/f.png" {
		t.Errorf("footer %+v", e.Footer)
	}
	// A size a server learned (a relayed stored form) is kept; it only lays the image out.
	if e.Thumbnail.Width != 50 || e.Thumbnail.Height != 40 {
		t.Errorf("thumbnail %+v", e.Thumbnail)
	}
	// Canonical once: the stored form normalizes to itself, as a peer's relay of it will.
	again, err := NormalizeEmbeds([]Embed{e}, nil)
	if err != nil || !reflect.DeepEqual(again[0], e) {
		t.Errorf("not stable: %+v, %v", again, err)
	}
}

// A learned size is kept only within what the proxy would serve: a side and the area, so a
// peer cannot have readers lay out room for a picture no instance would show.
func TestLearnedSizesAreBounded(t *testing.T) {
	for _, c := range []struct {
		w, h int
		keep bool
	}{
		{640, 480, true},
		{maxEmbedImageSide, maxEmbedImagePixels / maxEmbedImageSide, true},
		{maxEmbedImageSide + 1, 10, false},
		{maxEmbedImageSide, maxEmbedImageSide, false},
		{8000, 8000, false},
		{0, 480, false},
	} {
		got, err := NormalizeEmbeds([]Embed{{Image: &EmbedImage{URL: "https://img.example/a.png", Width: c.w, Height: c.h}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if kept := got[0].Image.Width == c.w && got[0].Image.Height == c.h && c.w > 0; kept != c.keep {
			t.Errorf("relayed %dx%d: kept %+v", c.w, c.h, got[0].Image)
		}
		list := []Embed{{Image: &EmbedImage{URL: "https://img.example/a.png"}}}
		if changed := withSizes(list, map[string][2]int{"https://img.example/a.png": {c.w, c.h}}); changed != c.keep {
			t.Errorf("warmed %dx%d: filled in %+v", c.w, c.h, list[0].Image)
		}
	}
}

func TestStoredFormDropsServedFields(t *testing.T) {
	m := &Message{}
	m.SetEmbeds([]Embed{{
		Type:   EmbedTypeRich,
		Title:  "t",
		Image:  &EmbedImage{URL: "https://a.example/i.png", ProxyURL: "https://proxy.example/x", Width: 3, Height: 4},
		Author: &EmbedAuthor{Name: "a", IconURL: "https://a.example/a.png", ProxyIconURL: "https://proxy.example/y"},
		Footer: &EmbedFooter{Text: "f", ProxyIconURL: "https://proxy.example/z"},
	}})
	if strings.Contains(m.EmbedsJSON, "proxy") || strings.Contains(m.EmbedsJSON, "rich") {
		t.Fatalf("stored %s", m.EmbedsJSON)
	}
	got := m.Embeds()
	if len(got) != 1 || got[0].Image.Width != 3 || got[0].Author.IconURL == "" {
		t.Fatalf("round trip %+v", got)
	}
	m.SetEmbeds(nil)
	if m.EmbedsJSON != "" || m.Embeds() != nil {
		t.Fatalf("cleared to %q", m.EmbedsJSON)
	}
}

// ---- serving ------------------------------------------------------------------------------------

type fakeSigner struct{}

func (fakeSigner) ProxyURL(src string) string {
	return "https://b.example/media/v1/external/sig/" + src
}
func (fakeSigner) IsLocal(u string) bool { return strings.HasPrefix(u, "https://b.example/media/") }

func TestEmbedsAreServedThroughTheProxy(t *testing.T) {
	w := newMirrorWorld()
	w.svc.cfg.Nebula.PublicURL = embedCDN
	m := &Message{ID: 1, RoomID: mirrorRoom, Plaintext: "hi"}
	m.SetEmbeds([]Embed{{
		Title:     "t",
		Image:     &EmbedImage{URL: chartURL},
		Thumbnail: &EmbedImage{URL: "https://img.example/t.png"},
		Author:    &EmbedAuthor{Name: "a", IconURL: "https://img.example/a.png"},
		Footer:    &EmbedFooter{Text: "f"},
	}})

	// No proxy: an image on this instance's CDN is served as it is, any other gets no
	// address at all (the client offers to load it rather than loading it unasked).
	got := w.svc.embedsJSON(m)[0]
	if got.Type != EmbedTypeRich || got.Image.ProxyURL != chartURL || got.Thumbnail.ProxyURL != "" || got.Author.ProxyIconURL != "" {
		t.Fatalf("without a proxy: %+v %+v %+v", got.Image, got.Thumbnail, got.Author)
	}
	if got.Footer.ProxyIconURL != "" {
		t.Fatalf("a footer with no icon got %q", got.Footer.ProxyIconURL)
	}

	sig := fakeSigner{}
	w.svc.SetMediaSigner(sig)
	got = w.svc.embedsJSON(m)[0]
	if got.Image.ProxyURL != chartURL || got.Thumbnail.ProxyURL != sig.ProxyURL("https://img.example/t.png") ||
		got.Author.ProxyIconURL != sig.ProxyURL("https://img.example/a.png") {
		t.Fatalf("with a proxy: %+v %+v %+v", got.Image, got.Thumbnail, got.Author)
	}
	// The stored row never gains what was served.
	if strings.Contains(m.EmbedsJSON, "proxy") {
		t.Fatalf("serving wrote into the row: %s", m.EmbedsJSON)
	}

	// Both serializers carry it, and an empty list is still a list (an edit that removed them
	// must reach the client as []).
	for name, out := range map[string]map[string]interface{}{
		"rest":    w.svc.messageToJSON(m, nil),
		"gateway": w.svc.messageEventPayload(m),
	} {
		list, ok := out["embeds"].([]Embed)
		if !ok || len(list) != 1 || list[0].Thumbnail.ProxyURL == "" {
			t.Errorf("%s: embeds %#v", name, out["embeds"])
		}
	}
	plain := &Message{ID: 2, RoomID: mirrorRoom, Plaintext: "no embeds"}
	b, _ := json.Marshal(w.svc.messageToJSON(plain, nil))
	if !strings.Contains(string(b), `"embeds":[]`) {
		t.Errorf("no embeds served as %s", b)
	}
	// An end-to-end encrypted message never shows any, whatever its row holds.
	enc := &Message{ID: 3, RoomID: mirrorRoom, Ciphertext: "AAAA", EmbedsJSON: m.EmbedsJSON}
	if got := w.svc.embedsJSON(enc); got == nil || len(got) != 0 {
		t.Errorf("encrypted message served %+v", got)
	}
}

// ---- sending and editing ----------------------------------------------------------------------

const embedBot = 9 // a bot member of the space

// embedMessages is fakeMessages that can create and edit, as the real repository does.
type embedMessages struct{ *fakeMessages }

func (f embedMessages) Create(ctx context.Context, m *Message) error {
	m.CreatedAt = time.Now()
	return f.Insert(ctx, m)
}

func (f embedMessages) Update(_ context.Context, _, msgID int64, ciphertext, plaintext string, embeds *string) (*Message, error) {
	m := f.stored[msgID]
	if m == nil {
		return nil, nil
	}
	cp := *m
	switch {
	case plaintext != "":
		cp.Plaintext = plaintext
	case ciphertext != "" || embeds == nil:
		cp.Ciphertext = ciphertext
	}
	if embeds != nil {
		cp.EmbedsJSON = *embeds
	}
	f.stored[msgID] = &cp
	return &cp, nil
}

// policyAuth is fakeAuth with a space policy (automod) for the sender.
type policyAuth struct {
	*fakeAuth
	policy *spaces.SendPolicy
}

func (a *policyAuth) SendPolicy(context.Context, int64, int64) (*spaces.SendPolicy, error) {
	return a.policy, nil
}

// botUsers knows which accounts are bots.
type botUsers struct {
	auth.UserRepository
	bots map[int64]bool
}

func (u botUsers) GetByID(_ context.Context, uid int64) (*auth.User, error) {
	return &auth.User{ID: uid, Bot: u.bots[uid]}, nil
}

func (u botUsers) GetByIDs(ctx context.Context, ids []int64) ([]*auth.User, error) {
	out := make([]*auth.User, 0, len(ids))
	for _, uid := range ids {
		usr, _ := u.GetByID(ctx, uid)
		out = append(out, usr)
	}
	return out, nil
}

type embedWorld struct {
	*mirrorWorld
	policy *policyAuth
}

// newEmbedWorld is a space channel hosted here, with a bot member that may embed links and
// has uploaded chart.png into it.
func newEmbedWorld() *embedWorld {
	w := newMirrorWorld()
	w.svc.federator = nil
	w.auth.perms[embedBot] = permissions.PermViewRoom | permissions.PermSendMessages | permissions.PermAttachFiles | permissions.PermEmbedLinks
	pa := &policyAuth{fakeAuth: w.auth}
	w.svc.spaceAuth = pa
	w.svc.repo = embedMessages{w.repo}
	w.svc.userRepo = botUsers{bots: map[int64]bool{embedBot: true}}
	w.svc.cfg.Nebula.PublicURL = embedCDN
	w.repo.uploads[chartID] = &AttachmentRow{RoomID: mirrorRoom, ID: chartID, UploaderID: embedBot, Filename: "chart.png", ContentType: "image/png", URL: chartURL, Width: 640, Height: 480}
	return &embedWorld{mirrorWorld: w, policy: pa}
}

func embedsIn(list ...Embed) EmbedList { return EmbedList{Set: true, List: list} }

func TestABotSendsEmbeds(t *testing.T) {
	w := newEmbedWorld()
	ctx := context.Background()
	m, err := w.svc.Create(ctx, embedBot, mirrorRoom, &CreateMessageInput{
		Attachments: []string{id.Format(chartID)},
		Embeds:      embedsIn(Embed{Title: " Weekly ", Image: &EmbedImage{URL: "attachment://chart.png"}}),
	})
	if err != nil {
		t.Fatalf("an embed-only message was refused: %v", err)
	}
	stored := w.repo.stored[m.ID].Embeds()
	if len(stored) != 1 || stored[0].Title != "Weekly" || stored[0].Image.URL != chartURL || stored[0].Image.Width != 640 {
		t.Fatalf("stored %+v", stored)
	}
	if got := w.repo.uploads[chartID].MessageID; got == nil || *got != m.ID {
		t.Fatalf("the embedded upload was not claimed: %v", got)
	}
}

func TestEmbedSendRefusals(t *testing.T) {
	ctx := context.Background()
	send := func(w *embedWorld, from int64, list ...Embed) error {
		_, err := w.svc.Create(ctx, from, mirrorRoom, &CreateMessageInput{Plaintext: "hi", Embeds: embedsIn(list...)})
		return err
	}
	ok := Embed{Title: "t"}

	if err := send(newEmbedWorld(), alice, ok); !errors.Is(err, ErrEmbedsForBots) {
		t.Errorf("a person's embeds: %v", err)
	}
	w := newEmbedWorld()
	w.auth.perms[embedBot] &^= permissions.PermEmbedLinks
	if err := send(w, embedBot, ok); !errors.Is(err, ErrEmbedForbidden) {
		t.Errorf("without Embed Links: %v", err)
	}
	w = newEmbedWorld()
	on := true
	w.rooms.room.E2EEEnabled = &on
	if err := send(w, embedBot, ok); !errors.Is(err, ErrEmbedsEncrypted) {
		t.Errorf("into an encrypted room: %v", err)
	}
	if err := send(newEmbedWorld(), embedBot, ok, Embed{Fields: []EmbedField{{Name: "n"}}}); embedPath(t, err) != "embeds[1].fields[0].value" {
		t.Errorf("an invalid embed: %v", err)
	}
	w = newEmbedWorld()
	if err := send(w, embedBot, ok); err != nil || len(w.repo.stored) != 1 {
		t.Errorf("a valid one: %v", err)
	}
	// A person's message without embeds is untouched by any of this.
	if _, err := newEmbedWorld().svc.Create(ctx, alice, mirrorRoom, &CreateMessageInput{Plaintext: "hi"}); err != nil {
		t.Errorf("plain message: %v", err)
	}
}

// An invite link in an embed is an invite link: automod's content rules read embed text.
func TestAutomodReadsEmbeds(t *testing.T) {
	w := newEmbedWorld()
	w.policy.policy = &spaces.SendPolicy{AutomodFlags: spaces.AutomodInviteLinks}
	_, err := w.svc.Create(context.Background(), embedBot, mirrorRoom, &CreateMessageInput{
		Embeds: embedsIn(Embed{Title: "Join us", Description: "https://discord.gg/abcdef"}),
	})
	var aerr *AutomodError
	if !errors.As(err, &aerr) || aerr.Rule != AutomodRuleInvite {
		t.Fatalf("err %v", err)
	}
}

// An edit is held to automod's content rules as a send is: neither new text nor new embeds
// may carry an invite link past them. The rules that count sends are a send's alone.
func TestAutomodReadsEdits(t *testing.T) {
	ctx := context.Background()
	w := newEmbedWorld()
	rdb, _ := redistest.New()
	w.svc.redis = rdb
	m, err := w.svc.Create(ctx, embedBot, mirrorRoom, &CreateMessageInput{Plaintext: "hi", Embeds: embedsIn(Embed{Title: "clean"})})
	if err != nil {
		t.Fatal(err)
	}
	w.policy.policy = &spaces.SendPolicy{
		AutomodFlags:        spaces.AutomodInviteLinks | spaces.AutomodRepeatedMessages | spaces.AutomodMassMentions,
		AutomodMentionLimit: 1,
	}
	edit := func(in *EditMessageInput) error {
		_, err := w.svc.Edit(ctx, embedBot, mirrorRoom, m.ID, in)
		return err
	}
	for name, in := range map[string]*EditMessageInput{
		"new embeds":        {Embeds: embedsIn(Embed{Description: "join discord.gg/abcdef"})},
		"a new field":       {Embeds: embedsIn(Embed{Title: "t", Fields: []EmbedField{{Name: "n", Value: "https://discord.gg/abcdef"}}})},
		"new text":          {Plaintext: "come to https://discord.gg/abcdef"},
		"text, kept embeds": {Plaintext: "hello"},
	} {
		if name == "text, kept embeds" {
			// The embeds the message keeps are read with the new text.
			w.repo.stored[m.ID].SetEmbeds([]Embed{{Title: "discord.gg/abcdef"}})
		}
		var aerr *AutomodError
		if err := edit(in); !errors.As(err, &aerr) || aerr.Rule != AutomodRuleInvite {
			t.Errorf("%s: %v", name, err)
		}
		w.repo.stored[m.ID].SetEmbeds([]Embed{{Title: "clean"}})
	}
	if got := w.repo.stored[m.ID]; got.Plaintext != "hi" || got.Embeds()[0].Title != "clean" {
		t.Fatalf("a refused edit was stored: %+v", got)
	}
	// The same text again and again, mentioning more than the cap: an edit sends nothing.
	for i := 0; i < repeatThreshold+1; i++ {
		if err := edit(&EditMessageInput{Plaintext: "the very same text once more, <@2> <@3>"}); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
	}
	// A moderator (no policy applies) is not held to it.
	w.policy.policy = nil
	if err := edit(&EditMessageInput{Plaintext: "https://discord.gg/abcdef"}); err != nil {
		t.Fatalf("exempt: %v", err)
	}

	// Over the edit route, the refusal reads as a send's does.
	w.policy.policy = &spaces.SendPolicy{AutomodFlags: spaces.AutomodInviteLinks}
	w.repo.stored[901] = &Message{ID: 901, RoomID: mirrorRoom, SenderID: embedBot, Plaintext: "hi"}
	status, body := sendBody(t, NewHandler(w.svc), &auth.User{ID: embedBot, Bot: true}, http.MethodPatch, `{"embeds":[{"description":"discord.gg/abcdef"}]}`)
	if status != http.StatusBadRequest || body["code"] != "automod_blocked" || body["rule"] != AutomodRuleInvite {
		t.Fatalf("over the route: %d %v", status, body)
	}
}

// A space voice room's text chat is held to the bits of writing there, as a text room is -
// and only to those: what was never checked there (reading, a text edit) still is not.
func TestVoiceRoomWritesAreHeldToTheirBits(t *testing.T) {
	ctx := context.Background()
	voice := func(deny int64) *embedWorld {
		w := newEmbedWorld()
		w.rooms.room.Type = rooms.TypeSpaceVoice
		w.auth.perms[embedBot] &^= deny
		return w
	}
	for name, c := range map[string]struct {
		deny int64
		in   *CreateMessageInput
		want error
	}{
		"Send Messages": {permissions.PermSendMessages, &CreateMessageInput{Plaintext: "hi"}, ErrForbidden},
		"View Room":     {permissions.PermViewRoom, &CreateMessageInput{Plaintext: "hi"}, ErrForbidden},
		"Attach Files":  {permissions.PermAttachFiles, &CreateMessageInput{Plaintext: "hi", Attachments: []string{id.Format(chartID)}}, ErrAttachForbidden},
		"Embed Links":   {permissions.PermEmbedLinks, &CreateMessageInput{Plaintext: "hi", Embeds: embedsIn(Embed{Title: "t"})}, ErrEmbedForbidden},
	} {
		w := voice(c.deny)
		if _, err := w.svc.Create(ctx, embedBot, mirrorRoom, c.in); !errors.Is(err, c.want) {
			t.Errorf("without %s: %v, want %v", name, err, c.want)
		}
		if len(w.repo.stored) != 0 {
			t.Errorf("without %s: stored %d messages", name, len(w.repo.stored))
		}
	}

	w := voice(0)
	m, err := w.svc.Create(ctx, embedBot, mirrorRoom, &CreateMessageInput{
		Plaintext: "hi", Attachments: []string{id.Format(chartID)}, Embeds: embedsIn(Embed{Title: "t"}),
	})
	if err != nil {
		t.Fatalf("with every bit: %v", err)
	}
	w.auth.perms[embedBot] &^= permissions.PermEmbedLinks
	if _, err := w.svc.Edit(ctx, embedBot, mirrorRoom, m.ID, &EditMessageInput{Embeds: embedsIn(Embed{Title: "u"})}); !errors.Is(err, ErrEmbedForbidden) {
		t.Errorf("an edit putting embeds on without Embed Links: %v", err)
	}
	w.auth.perms[embedBot] = 0
	if _, err := w.svc.Edit(ctx, embedBot, mirrorRoom, m.ID, &EditMessageInput{Plaintext: "edited"}); err != nil {
		t.Errorf("a text edit: %v", err)
	}
	if _, _, err := w.svc.ListWithReactions(ctx, embedBot, mirrorRoom, nil, 50); err != nil {
		t.Errorf("reading: %v", err)
	}
}

// Mentions written in an embed render, but never notify or count.
func TestEmbedMentionsDoNotNotify(t *testing.T) {
	w := newEmbedWorld()
	m, err := w.svc.Create(context.Background(), embedBot, mirrorRoom, &CreateMessageInput{
		Embeds: embedsIn(Embed{Description: "<@2> <@&50> @everyone", Fields: []EmbedField{{Name: "<@3>", Value: "<@4>"}}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Mentions) != 0 || len(m.MentionRoles) != 0 || m.MentionEveryone || len(w.rooms.incremented) != 0 {
		t.Fatalf("mentions %v roles %v everyone %v counted %v", m.Mentions, m.MentionRoles, m.MentionEveryone, w.rooms.incremented)
	}
}

// A mirror hands the origin the stored form: attachment:// means nothing over there.
func TestAMirrorForwardsResolvedEmbeds(t *testing.T) {
	w := newEmbedWorld()
	w.svc.federator = w.fed
	w.fed.create = func(in *CreateMessageInput, _ []Attachment) (*Message, error) {
		return &Message{ID: 901, RoomID: mirrorRoom, SenderID: embedBot}, nil
	}
	_, err := w.svc.Create(context.Background(), embedBot, mirrorRoom, &CreateMessageInput{
		Attachments: []string{id.Format(chartID)},
		Embeds:      embedsIn(Embed{Thumbnail: &EmbedImage{URL: "attachment://chart.png"}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := w.fed.created[0].Embeds.List
	if len(got) != 1 || got[0].Thumbnail.URL != chartURL {
		t.Fatalf("forwarded %+v", got)
	}
}

func TestEditingEmbeds(t *testing.T) {
	w := newEmbedWorld()
	ctx := context.Background()
	m, err := w.svc.Create(ctx, embedBot, mirrorRoom, &CreateMessageInput{Plaintext: "hi", Embeds: embedsIn(Embed{Title: "A"})})
	if err != nil {
		t.Fatal(err)
	}
	edit := func(in *EditMessageInput) (*Message, error) { return w.svc.Edit(ctx, embedBot, mirrorRoom, m.ID, in) }

	got, err := edit(&EditMessageInput{Plaintext: "hello"})
	if err != nil || got.Plaintext != "hello" || len(got.Embeds()) != 1 {
		t.Fatalf("absent keeps them: %+v %v", got, err)
	}
	got, err = edit(&EditMessageInput{Embeds: embedsIn(Embed{Title: "B"})})
	if err != nil || got.Plaintext != "hello" || got.Embeds()[0].Title != "B" {
		t.Fatalf("an embeds-only edit replaces them and keeps the text: %+v %v", got, err)
	}
	got, err = edit(&EditMessageInput{Embeds: EmbedList{Set: true}})
	if err != nil || got.Plaintext != "hello" || got.EmbedsJSON != "" {
		t.Fatalf("null clears them: %+v %v", got, err)
	}
	if _, err := w.svc.Edit(ctx, alice, mirrorRoom, m.ID, &EditMessageInput{Embeds: embedsIn(Embed{Title: "C"})}); !errors.Is(err, ErrEmbedsForBots) {
		t.Fatalf("a person editing embeds in: %v", err)
	}

	// Clearing the only thing a message has would leave it empty.
	only, err := w.svc.Create(ctx, embedBot, mirrorRoom, &CreateMessageInput{Embeds: embedsIn(Embed{Title: "A"})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Edit(ctx, embedBot, mirrorRoom, only.ID, &EditMessageInput{Embeds: EmbedList{Set: true}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("emptied a message: %v", err)
	}
}

// ---- what another instance relays --------------------------------------------------------------

func TestRelayedEmbeds(t *testing.T) {
	ctx := context.Background()
	relayed := func(e ...Embed) *Message {
		m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, Plaintext: "hi", CreatedAt: time.Now()}
		m.SetEmbeds(e)
		return m
	}

	w := newEmbedWorld()
	if err := w.svc.CreateFederated(ctx, mirrorRoom, nil, relayed(Embed{Title: "  t ", URL: "HTTPS://A.Example/#x"})); err != nil {
		t.Fatal(err)
	}
	if got := w.repo.stored[901].Embeds(); len(got) != 1 || got[0].Title != "t" || got[0].URL != "https://a.example/" {
		t.Fatalf("a relayed embed is held to the local rules: %+v", got)
	}

	w = newEmbedWorld()
	if err := w.svc.CreateFederated(ctx, mirrorRoom, nil, relayed(Embed{Image: &EmbedImage{URL: "attachment://chart.png"}})); !errors.Is(err, ErrInvalidEmbed) {
		t.Fatalf("attachment:// from a peer: %v", err)
	}

	w = newEmbedWorld()
	on := true
	w.rooms.room.E2EEEnabled = &on
	if err := w.svc.CreateFederated(ctx, mirrorRoom, nil, relayed(Embed{Title: "t"})); err != nil {
		t.Fatal(err)
	}
	if w.repo.stored[901].EmbedsJSON != "" {
		t.Fatal("a peer put embeds into an encrypted room")
	}

	w = newEmbedWorld()
	w.repo.stored[901] = relayed(Embed{Title: "t"})
	if _, err := w.svc.EditFederated(ctx, mirrorRoom, 901, "", "", &[]Embed{}); err != nil || w.repo.stored[901].EmbedsJSON != "" {
		t.Fatalf("a relayed clear: %v, %q", err, w.repo.stored[901].EmbedsJSON)
	}
	if _, err := w.svc.EditFederated(ctx, mirrorRoom, 901, "", "edited", nil); err != nil || w.repo.stored[901].Plaintext != "edited" {
		t.Fatalf("a relayed text edit: %v", err)
	}
}

// ---- refusals, push, storage --------------------------------------------------------------------

func TestEmbedRefusalsReachTheClient(t *testing.T) {
	status, body, ok := HTTPError(&EmbedError{Path: "embeds[0].fields[3].value", Reason: "can be at most 1024 characters"})
	if !ok || status != http.StatusBadRequest || body["code"] != "invalid_embed" || body["path"] != "embeds[0].fields[3].value" {
		t.Errorf("invalid embed: %d %v", status, body)
	}
	if status, body, _ := HTTPError(ErrEmbedsForBots); status != http.StatusBadRequest || body["error"] != "embeds are for bots" {
		t.Errorf("not a bot: %d %v", status, body)
	}
	if status, _, _ := HTTPError(ErrEmbedsEncrypted); status != http.StatusBadRequest {
		t.Errorf("encrypted room: %d", status)
	}
	if status, body, _ := HTTPError(ErrEmbedForbidden); status != http.StatusForbidden || body["error"] != "missing permission to embed links in this channel" {
		t.Errorf("no Embed Links: %d %v", status, body)
	}
}

// A bot's embed-only message reads as its first embed on a phone.
func TestEmbedOnlyPushText(t *testing.T) {
	push.SetProducing(true)
	defer push.SetProducing(false)
	w := newEmbedWorld()
	rdb, mem := redistest.New()
	w.svc.redis = rdb
	mem.AddToSet("push:users", id.Format(alice))
	m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: embedBot, CreatedAt: time.Now()}
	m.SetEmbeds([]Embed{{Description: "build passed"}})
	w.svc.pushMessage(context.Background(), w.rooms.room, m, []int64{alice, embedBot}, nil, false, 0)
	jobs := mem.Stream("push:jobs")
	var job struct {
		Message struct {
			Text string `json:"x"`
		} `json:"m"`
	}
	if len(jobs) != 1 || json.Unmarshal([]byte(jobs[0]), &job) != nil || job.Message.Text != "build passed" {
		t.Fatalf("jobs %v", jobs)
	}
}

// Every column the messages table lists is a field of Message, or the generic insert and
// selects bind and scan the wrong things.
func TestMessageColumnsAreFields(t *testing.T) {
	tags := map[string]bool{}
	rt := reflect.TypeOf(Message{})
	for i := 0; i < rt.NumField(); i++ {
		tags[rt.Field(i).Tag.Get("db")] = true
	}
	cols := messagesTable.Metadata().Columns
	for _, c := range cols {
		if !tags[c] {
			t.Errorf("column %q has no Message field", c)
		}
	}
	if !reflect.DeepEqual(len(cols), len(tags)) {
		t.Errorf("%d columns, %d tagged fields", len(cols), len(tags))
	}
	found := false
	for _, c := range cols {
		found = found || c == "embeds"
	}
	if !found {
		t.Error("embeds is not a column")
	}
}

// The room type a bot's PM lives in needs no Embed Links bit.
func TestEmbedsNeedNoBitOutsideASpace(t *testing.T) {
	w := newEmbedWorld()
	w.auth.perms[embedBot] = 0
	w.rooms.room = &rooms.Room{ID: mirrorRoom, Type: rooms.TypeGroupPM, E2EEEnabled: new(bool)}
	pms := participantRooms{w.rooms}
	w.svc.rooms = pms
	if _, err := w.svc.Create(context.Background(), embedBot, mirrorRoom, &CreateMessageInput{Embeds: embedsIn(Embed{Title: "t"})}); err != nil {
		t.Fatalf("a plaintext group PM: %v", err)
	}
}

// participantRooms is fakeRooms whose room has the bot and alice as participants.
type participantRooms struct{ *fakeRooms }

func (participantRooms) GetParticipants(context.Context, int64) ([]int64, error) {
	return []int64{alice, embedBot}, nil
}

// sendBody drives the create or edit handler as user with a raw JSON body.
func sendBody(t *testing.T, h *Handler, user *auth.User, method, body string) (int, map[string]any) {
	t.Helper()
	app := fiber.New()
	asUser := func(c fiber.Ctx) error {
		c.Locals(auth.LocalsKeyUser, user)
		return c.Next()
	}
	app.Post("/rooms/:id/messages", asUser, h.Create)
	app.Patch("/rooms/:id/messages/:msg_id", asUser, h.Edit)
	path := "/rooms/100/messages"
	if method == http.MethodPatch {
		path += "/901"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return res.StatusCode, out
}

func TestEmbedHandlers(t *testing.T) {
	bot := &auth.User{ID: embedBot, Bot: true}
	w := newEmbedWorld()
	h := NewHandler(w.svc)

	status, body := sendBody(t, h, bot, http.MethodPost, `{"embeds":[{"title":"ok"},{"fields":[{"name":"n","value":"`+strings.Repeat("a", 1025)+`"}]}]}`)
	if status != http.StatusBadRequest || body["code"] != "invalid_embed" || body["path"] != "embeds[1].fields[0].value" {
		t.Fatalf("over the limit: %d %v", status, body)
	}
	status, body = sendBody(t, h, bot, http.MethodPost, `{"embeds":[{"color":"red"}]}`)
	if status != http.StatusBadRequest || body["path"] != "embeds[0].color" {
		t.Fatalf("a wrong type: %d %v", status, body)
	}
	status, body = sendBody(t, h, &auth.User{ID: alice}, http.MethodPost, `{"embeds":[{"title":"x"}]}`)
	if status != http.StatusBadRequest || body["error"] != "embeds are for bots" {
		t.Fatalf("a person: %d %v", status, body)
	}
	status, body = sendBody(t, h, bot, http.MethodPost, `{"embeds":[{"title":"Deploy","image":{"url":"https://img.example/x.png"}}]}`)
	if status != http.StatusCreated {
		t.Fatalf("embeds only: %d %v", status, body)
	}
	list, _ := body["embeds"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["type"] != EmbedTypeRich {
		t.Fatalf("served %v", body["embeds"])
	}

	w.repo.stored[901] = &Message{ID: 901, RoomID: mirrorRoom, SenderID: embedBot, Plaintext: "hi"}
	status, body = sendBody(t, h, bot, http.MethodPatch, `{"embeds":[{"title":"edited"}]}`)
	if status != http.StatusOK || body["plaintext"] != "hi" {
		t.Fatalf("an embeds-only edit: %d %v", status, body)
	}
	status, body = sendBody(t, h, bot, http.MethodPatch, `{"embeds":[{"url":"https://a.example"}]}`)
	if status != http.StatusBadRequest || body["path"] != "embeds[0]" {
		t.Fatalf("an empty embed in an edit: %d %v", status, body)
	}
	w.auth.perms[embedBot] &^= permissions.PermEmbedLinks
	status, body = sendBody(t, h, bot, http.MethodPatch, `{"embeds":[{"title":"x"}]}`)
	if status != http.StatusForbidden || body["error"] != ErrEmbedForbidden.Error() {
		t.Fatalf("an edit without Embed Links: %d %v", status, body)
	}
}
