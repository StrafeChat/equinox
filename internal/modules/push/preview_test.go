package push

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

type fakeNames struct {
	users   map[int64]string
	roles   map[int64]string
	rooms   map[int64]string
	lookups int
}

func (f *fakeNames) userName(_ context.Context, id int64) (string, bool) {
	f.lookups++
	n, ok := f.users[id]
	return n, ok
}
func (f *fakeNames) roleName(_ context.Context, id int64) (string, bool) {
	n, ok := f.roles[id]
	return n, ok
}
func (f *fakeNames) roomName(_ context.Context, id int64) (string, bool) {
	n, ok := f.rooms[id]
	return n, ok
}

func TestRenderPreview(t *testing.T) {
	names := &fakeNames{
		users: map[int64]string{11: "Bob", 12: "Carol"},
		roles: map[int64]string{21: "Moderators", 22: "@everyone"},
		rooms: map[int64]string{31: "general"},
	}
	cases := map[string]string{
		"hey <@11> look":                     "hey @Bob look",
		"<@!12> and <@11>":                   "@Carol and @Bob",
		"who is <@99>?":                      "who is ?",
		"ping <@&21> now":                    "ping @Moderators now",
		"ping <@&22>":                        "ping @everyone",
		"ping <@&404>":                       "ping @role",
		"see <#31> and <#32>":                "see #general and #room",
		"nice <:pog:123> <a:dance:456>":      "nice :pog: :dance:",
		"from afar <@@123:other.example> hi": "from afar hi",
		"  lots \n\n of\t\twhitespace  ":     "lots of whitespace",
		"**bold** _kept_":                    "**bold** _kept_",
		"":                                   "",
		"<:not an emoji:12>":                 "<:not an emoji:12>",
		"<@11><@11><@11>":                    "@Bob@Bob@Bob",
	}
	for in, want := range cases {
		if got := renderPreview(context.Background(), in, names); got != want {
			t.Errorf("renderPreview(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderPreviewLimits(t *testing.T) {
	long := strings.Repeat("ab ", 200) // 600 runes
	got := renderPreview(context.Background(), long, &fakeNames{})
	if n := utf8.RuneCountInString(got); n != 300 {
		t.Errorf("preview is %d runes, want 300", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated preview has no ellipsis: %q", got[len(got)-10:])
	}
	// Multi-byte text is cut on rune boundaries.
	got = renderPreview(context.Background(), strings.Repeat("é", 400), &fakeNames{})
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 300 {
		t.Errorf("multi-byte truncation broke the text")
	}
	// At most ten distinct people are looked up; the rest are dropped like unknowns.
	names := &fakeNames{users: map[int64]string{}}
	var b strings.Builder
	for i := int64(1); i <= 15; i++ {
		names.users[i] = "u"
		b.WriteString("<@" + formatID(i) + "> ")
	}
	b.WriteString("<@1>")
	got = renderPreview(context.Background(), b.String(), names)
	if names.lookups != 10 {
		t.Errorf("%d user lookups, want 10", names.lookups)
	}
	if strings.Count(got, "@u") != 11 { // ten looked up, plus the repeat of the first
		t.Errorf("preview %q", got)
	}
}
