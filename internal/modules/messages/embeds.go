package messages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// Rich embeds (Discord's model). A bot may put up to ten embed objects on a message in a
// plaintext room: a coloured card with an author line, a title linking somewhere, a markdown
// description, a grid of fields, an image, a thumbnail and a footer. People's messages get
// their cards from link previews instead, which clients build themselves.
//
// What a sender gives is checked against Discord's limits and canonicalized once, on the way
// in (NormalizeEmbeds), and stored as JSON on the message row (messages.embeds). Images are
// never loaded by a client from wherever a bot pointed them - that would show every reader's
// IP address to that host: an image on this instance's own CDN (an attachment:// the bot
// uploaded with the message) is served as it is, and any other goes through this instance's
// image proxy, whose signed URL is added when the message is served (Service.embedsJSON).
// Nothing about the proxy is stored, so a mirror's copy is served through the mirror's own.
//
// Embeds never exist in an end-to-end encrypted room: the server would be writing plaintext
// into a room whose content it must not see. And text in an embed never notifies anyone - a
// mention written there renders, but only the message's own text is parsed for mentions.

const (
	// MaxEmbedsPerMessage caps the embeds on one message.
	MaxEmbedsPerMessage = 10
	// Per-field caps, in code points after trimming surrounding whitespace (Discord's).
	MaxEmbedTitleRunes       = 256
	MaxEmbedDescriptionRunes = 4096
	MaxEmbedFields           = 25
	MaxEmbedFieldNameRunes   = 256
	MaxEmbedFieldValueRunes  = 1024
	MaxEmbedFooterTextRunes  = 2048
	MaxEmbedAuthorNameRunes  = 256
	// MaxEmbedTotalRunes caps titles, descriptions, field names and values, footer texts and
	// author names over every embed of a message together.
	MaxEmbedTotalRunes = 6000
	// MaxEmbedURLBytes caps any URL in an embed.
	MaxEmbedURLBytes = 2048
	// MaxEmbedColor is the largest colour (0xRRGGBB).
	MaxEmbedColor = 0xFFFFFF
)

// EmbedTypeRich is the only embed type a message carries; it is added when serving.
const EmbedTypeRich = "rich"

var (
	// ErrInvalidEmbed is what an *EmbedError matches with errors.Is.
	ErrInvalidEmbed = errors.New("invalid embed")
	// ErrEmbedsForBots: a person's account sent embeds.
	ErrEmbedsForBots = errors.New("embeds are for bots")
	// ErrEmbedsEncrypted: embeds sent into an end-to-end encrypted room.
	ErrEmbedsEncrypted = errors.New("embeds cannot be sent in an end-to-end encrypted room")
	// ErrEmbedForbidden: the sender lacks Embed Links in this space room.
	ErrEmbedForbidden = errors.New("missing permission to embed links in this channel")
)

// EmbedError names the first field of a message's embeds that was refused, as a path into the
// request body ("embeds[0].fields[3].value"), and why.
type EmbedError struct {
	Path   string
	Reason string
}

func (e *EmbedError) Error() string        { return e.Path + ": " + e.Reason }
func (e *EmbedError) Is(target error) bool { return target == ErrInvalidEmbed }

// Embed is one rich embed. The same type is the request body, the stored form and the served
// form: the stored form never has Type or any proxy URL (SetEmbeds drops them), and only the
// server sets image sizes and proxy URLs - whatever a sender put there is ignored.
type Embed struct {
	Type        string `json:"type,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	// Timestamp is RFC 3339, stored in UTC.
	Timestamp string `json:"timestamp,omitempty"`
	// Color is 0xRRGGBB; nil for none (0 is black).
	Color     *int         `json:"color,omitempty"`
	Footer    *EmbedFooter `json:"footer,omitempty"`
	Image     *EmbedImage  `json:"image,omitempty"`
	Thumbnail *EmbedImage  `json:"thumbnail,omitempty"`
	Author    *EmbedAuthor `json:"author,omitempty"`
	Fields    []EmbedField `json:"fields,omitempty"`
}

// EmbedFooter is an embed's footer line.
type EmbedFooter struct {
	Text         string `json:"text"`
	IconURL      string `json:"icon_url,omitempty"`
	ProxyIconURL string `json:"proxy_icon_url,omitempty"`
}

// EmbedImage is an embed's image or thumbnail. Width/Height are what the server learned of it.
type EmbedImage struct {
	URL      string `json:"url"`
	ProxyURL string `json:"proxy_url,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// EmbedAuthor is an embed's author line.
type EmbedAuthor struct {
	Name         string `json:"name"`
	URL          string `json:"url,omitempty"`
	IconURL      string `json:"icon_url,omitempty"`
	ProxyIconURL string `json:"proxy_icon_url,omitempty"`
}

// EmbedField is one name/value pair of an embed's field grid.
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// MediaSigner is the instance's image proxy as the message serializers see it (implemented
// by internal/modules/media). ProxyURL signs src into its address on the proxy, "" when the
// proxy will not serve it; IsLocal says a URL is already this instance's own (its CDN) and is
// served as it is. With no signer set there are no proxy URLs at all, and a client offers to
// load an external image itself rather than loading it unasked.
type MediaSigner interface {
	ProxyURL(src string) string
	IsLocal(url string) bool
}

// requireBot refuses embeds from anyone but a bot account (Discord's rule: a person's cards
// come from link previews). A member of another instance writing here is judged by their
// shadow account's flag, which their own instance set.
func (s *Service) requireBot(ctx context.Context, userID int64) error {
	if s.userRepo == nil {
		return ErrEmbedsForBots
	}
	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil || !u.Bot {
		return ErrEmbedsForBots
	}
	return nil
}

// EmbedsAllowedIn says whether a room can carry embeds at all: only a plaintext one can. What
// another instance relays is held to it as a local send is.
func EmbedsAllowedIn(room *rooms.Room) bool { return room != nil && roomE2EEOff(room) }

// SetMediaSigner wires the image proxy in.
func (s *Service) SetMediaSigner(m MediaSigner) { s.media = m }

// mediaURL is the address a client loads an embed image from: the image itself when it is on
// this instance's own CDN, else its signed address on this instance's proxy, else "".
func (s *Service) mediaURL(src string) string {
	if src == "" {
		return ""
	}
	if s.ownsAttachment(src) || (s.media != nil && s.media.IsLocal(src)) {
		return src
	}
	if s.media == nil {
		return ""
	}
	return s.media.ProxyURL(src)
}

// embedsJSON is a message's embeds as clients get them, the one helper both serializers use:
// type "rich", and every image with the address it is loaded from. Always a list - empty when
// there are none - so a client applying an edit drops embeds the message no longer has. An
// end-to-end encrypted message never has any, whatever its row says.
func (s *Service) embedsJSON(m *Message) []Embed {
	if m == nil || m.Ciphertext != "" {
		return []Embed{}
	}
	list := m.Embeds()
	out := make([]Embed, 0, len(list))
	for _, e := range list {
		e = e.withProxies(s.mediaURL)
		e.Type = EmbedTypeRich
		out = append(out, e)
	}
	return out
}

// withProxies copies e with each image's proxy URL set by proxy ("" clears it).
func (e Embed) withProxies(proxy func(string) string) Embed {
	img := func(in *EmbedImage) *EmbedImage {
		if in == nil {
			return nil
		}
		out := *in
		out.ProxyURL = proxy(out.URL)
		return &out
	}
	icon := func(u string) string {
		if u == "" {
			return ""
		}
		return proxy(u)
	}
	e.Image = img(e.Image)
	e.Thumbnail = img(e.Thumbnail)
	if e.Author != nil {
		a := *e.Author
		a.ProxyIconURL = icon(a.IconURL)
		e.Author = &a
	}
	if e.Footer != nil {
		f := *e.Footer
		f.ProxyIconURL = icon(f.IconURL)
		e.Footer = &f
	}
	return e
}

// encodeEmbeds is the stored JSON of a list ("" for none).
func encodeEmbeds(list []Embed) string {
	if len(list) == 0 {
		return ""
	}
	stored := make([]Embed, len(list))
	for i, e := range list {
		e = e.withProxies(func(string) string { return "" })
		e.Type = ""
		stored[i] = e
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return ""
	}
	return string(b)
}

// withEmbedsText is a message's text followed by its embeds', what automod's content rules
// read: an invite link must not get past them by sitting in an embed.
func withEmbedsText(text string, list []Embed) string {
	et := embedsText(list)
	if et == "" {
		return text
	}
	if text == "" {
		return et
	}
	return text + "\n" + et
}

// embedsText is the text a reader sees in the embeds - titles, descriptions, links, fields,
// author and footer lines - one piece per line, for automod's content rules.
func embedsText(list []Embed) string {
	var b strings.Builder
	add := func(s string) {
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	for _, e := range list {
		add(e.Title)
		add(e.URL)
		add(e.Description)
		if e.Author != nil {
			add(e.Author.Name)
			add(e.Author.URL)
		}
		for _, f := range e.Fields {
			add(f.Name)
			add(f.Value)
		}
		if e.Footer != nil {
			add(e.Footer.Text)
		}
	}
	return b.String()
}

// embedPushText is what a push notification says for a message with no text of its own: the
// first embed's title, else its description.
func embedPushText(list []Embed) string {
	if len(list) == 0 {
		return ""
	}
	if list[0].Title != "" {
		return list[0].Title
	}
	return list[0].Description
}

// ---- the request body ----------------------------------------------------------------------

// EmbedList is the `embeds` field of a create or edit body. Set records that the field was
// there at all, null included: an edit leaves the embeds alone when it is absent and clears
// them for null or []. Decoding checks the shape - a string where a string goes - so that a
// refusal can name the field; NormalizeEmbeds checks the limits. Tag it `omitzero` where it
// is encoded, so an absent list stays absent.
type EmbedList struct {
	Set  bool
	List []Embed
}

func (l *EmbedList) UnmarshalJSON(b []byte) error {
	l.Set, l.List = true, nil
	b = bytes.TrimSpace(b)
	if isJSONNull(b) {
		return nil
	}
	var raws []json.RawMessage
	if len(b) == 0 || b[0] != '[' || json.Unmarshal(b, &raws) != nil {
		return &EmbedError{Path: "embeds", Reason: "must be an array of embed objects"}
	}
	if len(raws) > MaxEmbedsPerMessage {
		return tooManyEmbeds()
	}
	d := &embedDecoder{}
	list := make([]Embed, 0, len(raws))
	for i, raw := range raws {
		list = append(list, d.embed(raw, "embeds["+strconv.Itoa(i)+"]"))
		if d.err != nil {
			return d.err
		}
	}
	l.List = list
	return nil
}

func (l EmbedList) MarshalJSON() ([]byte, error) {
	if l.List == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(l.List)
}

func tooManyEmbeds() *EmbedError {
	return &EmbedError{Path: "embeds", Reason: "a message can have at most " + strconv.Itoa(MaxEmbedsPerMessage) + " embeds"}
}

func isJSONNull(b []byte) bool { return string(b) == "null" }

// embedDecoder reads embed objects field by field and keeps the first refusal. Keys the
// contract does not name (type, provider, video, sizes, proxy URLs) are never read.
type embedDecoder struct{ err error }

func (d *embedDecoder) fail(path, reason string) {
	if d.err == nil {
		d.err = &EmbedError{Path: path, Reason: reason}
	}
}

// object is the JSON object at path, nil when it is absent or null (or refused).
func (d *embedDecoder) object(raw json.RawMessage, path string) map[string]json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if d.err != nil || len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var out map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &out) != nil {
		d.fail(path, "must be an object")
		return nil
	}
	return out
}

func (d *embedDecoder) str(o map[string]json.RawMessage, path, key string) string {
	raw := bytes.TrimSpace(o[key])
	if d.err != nil || len(raw) == 0 || isJSONNull(raw) {
		return ""
	}
	var s string
	if raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		d.fail(path+"."+key, "must be a string")
	}
	return s
}

func (d *embedDecoder) boolean(o map[string]json.RawMessage, path, key string) bool {
	raw := bytes.TrimSpace(o[key])
	if d.err != nil || len(raw) == 0 || isJSONNull(raw) {
		return false
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		d.fail(path+"."+key, "must be true or false")
	}
	return v
}

func (d *embedDecoder) color(o map[string]json.RawMessage, path, key string) *int {
	raw := bytes.TrimSpace(o[key])
	if d.err != nil || len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n < 0 || n > MaxEmbedColor {
		d.fail(path+"."+key, "must be an integer from 0 to "+strconv.Itoa(MaxEmbedColor))
		return nil
	}
	v := int(n)
	return &v
}

func (d *embedDecoder) array(o map[string]json.RawMessage, path, key string) []json.RawMessage {
	raw := bytes.TrimSpace(o[key])
	if d.err != nil || len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var out []json.RawMessage
	if raw[0] != '[' || json.Unmarshal(raw, &out) != nil {
		d.fail(path+"."+key, "must be an array")
		return nil
	}
	return out
}

func (d *embedDecoder) embed(raw json.RawMessage, path string) Embed {
	var e Embed
	o := d.object(raw, path)
	if o == nil {
		d.fail(path, "must be an object")
		return e
	}
	e.Title = d.str(o, path, "title")
	e.Description = d.str(o, path, "description")
	e.URL = d.str(o, path, "url")
	e.Timestamp = d.str(o, path, "timestamp")
	e.Color = d.color(o, path, "color")
	if f := d.object(o["footer"], path+".footer"); f != nil {
		p := path + ".footer"
		e.Footer = &EmbedFooter{Text: d.str(f, p, "text"), IconURL: d.str(f, p, "icon_url")}
	}
	if i := d.object(o["image"], path+".image"); i != nil {
		e.Image = &EmbedImage{URL: d.str(i, path+".image", "url")}
	}
	if t := d.object(o["thumbnail"], path+".thumbnail"); t != nil {
		e.Thumbnail = &EmbedImage{URL: d.str(t, path+".thumbnail", "url")}
	}
	if a := d.object(o["author"], path+".author"); a != nil {
		p := path + ".author"
		e.Author = &EmbedAuthor{Name: d.str(a, p, "name"), URL: d.str(a, p, "url"), IconURL: d.str(a, p, "icon_url")}
	}
	for j, raw := range d.array(o, path, "fields") {
		p := path + ".fields[" + strconv.Itoa(j) + "]"
		f := d.object(raw, p)
		if f == nil {
			d.fail(p, "must be an object")
			break
		}
		e.Fields = append(e.Fields, EmbedField{Name: d.str(f, p, "name"), Value: d.str(f, p, "value"), Inline: d.boolean(f, p, "inline")})
	}
	return e
}

// ---- validation and the stored form --------------------------------------------------------

// NormalizeEmbeds checks a message's embeds against the limits and returns their stored form:
// text trimmed, URLs canonicalized, the timestamp in UTC, server-set fields dropped, and each
// attachment://<filename or id> resolved to the attachment's own URL (and size). attachments
// are the message's own files that may be named that way - nil where none may: a peer only
// ever sends resolved URLs. The first field that fails is the *EmbedError returned. It is the
// one validator, for a local send and an edit as for what another instance relays.
func NormalizeEmbeds(list []Embed, attachments []Attachment) ([]Embed, error) {
	if len(list) == 0 {
		return nil, nil
	}
	if len(list) > MaxEmbedsPerMessage {
		return nil, tooManyEmbeds()
	}
	out := make([]Embed, 0, len(list))
	total := 0
	for i := range list {
		n := &embedNormalizer{path: "embeds[" + strconv.Itoa(i) + "]", attachments: attachments}
		e := n.embed(list[i])
		if n.err != nil {
			return nil, n.err
		}
		total += n.runes
		out = append(out, e)
	}
	if total > MaxEmbedTotalRunes {
		return nil, tooMuchEmbedText()
	}
	return out, nil
}

// CheckEmbedsWithin is a cheap first look at embeds whose text is still to change before
// NormalizeEmbeds sees it - a peer's, whose mentions are translated first - and shrink by at
// most slack code points in all. It refuses only what NormalizeEmbeds would refuse however
// that came out: too many embeds or fields, or more text than MaxEmbedTotalRunes plus slack.
func CheckEmbedsWithin(list []Embed, slack int) error {
	if len(list) > MaxEmbedsPerMessage {
		return tooManyEmbeds()
	}
	total := 0
	for i, e := range list {
		if len(e.Fields) > MaxEmbedFields {
			return &EmbedError{Path: "embeds[" + strconv.Itoa(i) + "].fields", Reason: tooManyEmbedFields()}
		}
		// What the normalizer counts toward the total, as it counts it.
		for _, t := range []string{e.Title, e.Description} {
			total += embedRunes(t)
		}
		if e.Author != nil {
			total += embedRunes(e.Author.Name)
		}
		for _, f := range e.Fields {
			total += embedRunes(f.Name) + embedRunes(f.Value)
		}
		if e.Footer != nil {
			total += embedRunes(e.Footer.Text)
		}
	}
	if total > MaxEmbedTotalRunes+slack {
		return tooMuchEmbedText()
	}
	return nil
}

func embedRunes(s string) int { return utf8.RuneCountInString(strings.TrimSpace(s)) }

func tooMuchEmbedText() *EmbedError {
	return &EmbedError{Path: "embeds", Reason: "the embeds' text can total at most " + strconv.Itoa(MaxEmbedTotalRunes) + " characters"}
}

func tooManyEmbedFields() string {
	return "an embed can have at most " + strconv.Itoa(MaxEmbedFields) + " fields"
}

// embedNormalizer normalizes one embed, counting its text and keeping the first refusal.
type embedNormalizer struct {
	path        string
	attachments []Attachment
	runes       int
	err         error
}

func (n *embedNormalizer) fail(field, reason string) {
	if n.err == nil {
		n.err = &EmbedError{Path: n.path + field, Reason: reason}
	}
}

// text trims s and checks it against max code points, counting it toward the total.
func (n *embedNormalizer) text(s, field string, max int, required bool) string {
	s = strings.TrimSpace(s)
	c := embedRunes(s)
	switch {
	case required && c == 0:
		n.fail(field, "is required")
	case c > max:
		n.fail(field, "can be at most "+strconv.Itoa(max)+" characters")
	}
	n.runes += c
	return s
}

func (n *embedNormalizer) link(raw, field string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, reason := canonicalEmbedURL(raw)
	if reason != "" {
		n.fail(field, reason)
	}
	return u
}

// image is an image source: an http(s) URL, or attachment://<filename or id> naming one of
// the message's own images, resolved to it. The attachment is returned for its size.
func (n *embedNormalizer) image(raw, field string, required bool) (string, *Attachment) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			n.fail(field, "is required")
		}
		return "", nil
	}
	const scheme = "attachment://"
	if len(raw) >= len(scheme) && strings.EqualFold(raw[:len(scheme)], scheme) {
		a, reason := embedAttachment(n.attachments, raw[len(scheme):])
		if reason != "" {
			n.fail(field, reason)
			return "", nil
		}
		return a.URL, a
	}
	return n.link(raw, field), nil
}

func (n *embedNormalizer) embed(in Embed) Embed {
	var e Embed
	e.Title = n.text(in.Title, ".title", MaxEmbedTitleRunes, false)
	e.Description = n.text(in.Description, ".description", MaxEmbedDescriptionRunes, false)
	e.URL = n.link(in.URL, ".url")
	if ts := strings.TrimSpace(in.Timestamp); ts != "" {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			n.fail(".timestamp", "must be an RFC 3339 timestamp")
		} else {
			e.Timestamp = t.UTC().Format(time.RFC3339Nano)
		}
	}
	if in.Color != nil {
		if *in.Color < 0 || *in.Color > MaxEmbedColor {
			n.fail(".color", "must be an integer from 0 to "+strconv.Itoa(MaxEmbedColor))
		}
		c := *in.Color
		e.Color = &c
	}
	if in.Author != nil {
		a := &EmbedAuthor{Name: n.text(in.Author.Name, ".author.name", MaxEmbedAuthorNameRunes, true)}
		a.URL = n.link(in.Author.URL, ".author.url")
		a.IconURL, _ = n.image(in.Author.IconURL, ".author.icon_url", false)
		e.Author = a
	}
	if len(in.Fields) > MaxEmbedFields {
		n.fail(".fields", tooManyEmbedFields())
	}
	for j, f := range in.Fields {
		if n.err != nil {
			break
		}
		p := ".fields[" + strconv.Itoa(j) + "]"
		e.Fields = append(e.Fields, EmbedField{
			Name:   n.text(f.Name, p+".name", MaxEmbedFieldNameRunes, true),
			Value:  n.text(f.Value, p+".value", MaxEmbedFieldValueRunes, true),
			Inline: f.Inline,
		})
	}
	sized := func(in *EmbedImage, field string) *EmbedImage {
		if in == nil {
			return nil
		}
		u, a := n.image(in.URL, field+".url", true)
		out := &EmbedImage{URL: u}
		if a != nil {
			out.Width, out.Height = a.Width, a.Height
		} else if learnedSizeOK(in.Width, in.Height) {
			// What a server learned of an image it fetched (the proxy's warm-up, or the origin
			// of a relayed message): kept, as it is no more than a layout hint.
			out.Width, out.Height = in.Width, in.Height
		}
		return out
	}
	e.Image = sized(in.Image, ".image")
	e.Thumbnail = sized(in.Thumbnail, ".thumbnail")
	if in.Footer != nil {
		f := &EmbedFooter{Text: n.text(in.Footer.Text, ".footer.text", MaxEmbedFooterTextRunes, true)}
		f.IconURL, _ = n.image(in.Footer.IconURL, ".footer.icon_url", false)
		e.Footer = f
	}
	if n.err == nil && e.Title == "" && e.Description == "" && len(e.Fields) == 0 && e.Author == nil &&
		e.Footer == nil && e.Image == nil && e.Thumbnail == nil {
		n.fail("", "has nothing to show: give it a title, description, fields, author, footer, image or thumbnail")
	}
	return e
}

// maxEmbedImageSide and maxEmbedImagePixels bound a learned image size, a side and the area,
// as the proxy bounds what it serves.
const (
	maxEmbedImageSide   = 16384
	maxEmbedImagePixels = 50_000_000
)

// learnedSizeOK says whether a width and height a server learned of an image are worth
// keeping: both there, and within what the proxy would serve.
func learnedSizeOK(w, h int) bool {
	return w > 0 && h > 0 && w <= maxEmbedImageSide && h <= maxEmbedImageSide && int64(w)*int64(h) <= maxEmbedImagePixels
}

// canonicalEmbedURL checks an http(s) URL and returns its canonical form - scheme and host in
// lower case, no fragment - or why it was refused.
func canonicalEmbedURL(raw string) (string, string) {
	if len(raw) > MaxEmbedURLBytes {
		return "", "can be at most " + strconv.Itoa(MaxEmbedURLBytes) + " bytes"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "must be an http or https URL"
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Opaque != "" || u.Hostname() == "" {
		return "", "must be an http or https URL"
	}
	if u.User != nil {
		return "", "must not contain a username or password"
	}
	u.Scheme = scheme
	u.Host = strings.ToLower(u.Host)
	u.Fragment, u.RawFragment = "", ""
	out := u.String()
	if len(out) > MaxEmbedURLBytes {
		return "", "can be at most " + strconv.Itoa(MaxEmbedURLBytes) + " bytes"
	}
	return out, ""
}

// embedImageTypes are the attachments an embed may show (Discord's set).
var embedImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// embedAttachment finds the attachment an attachment:// reference names: by id, else by file
// name (as uploaded, or as the server stored it), which must then be unique on the message.
func embedAttachment(list []Attachment, name string) (*Attachment, string) {
	name = strings.TrimSpace(name)
	var found *Attachment
	for i := range list {
		if list[i].ID == name {
			found = &list[i]
			break
		}
	}
	if found == nil && name != "" {
		for _, want := range []string{name, safeFilename(name)} {
			matches := 0
			for i := range list {
				if list[i].Filename == want {
					matches++
					found = &list[i]
				}
			}
			if matches > 1 {
				return nil, "names more than one attachment of this message; name it by its id"
			}
			if found != nil {
				break
			}
		}
	}
	if found == nil {
		return nil, "names no file attached to this message"
	}
	if found.Encrypted || !embedImageTypes[strings.ToLower(found.ContentType)] {
		return nil, "must name a png, jpeg, gif or webp image"
	}
	return found, ""
}
