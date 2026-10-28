package auth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A game name is display text shown beside someone's name: the same rule as a space
// nickname - nothing that breaks the line or reorders the text around it, at least one
// visible character - with whitespace tidied rather than refused, counted in code points.
func TestSanitizeActivityName(t *testing.T) {
	family := "\U0001F468‍\U0001F469‍\U0001F467" // man ZWJ woman ZWJ girl
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", "Minecraft", "Minecraft", true},
		{"trimmed", "  Minecraft  ", "Minecraft", true},
		{"inner runs collapse", "Old   School 　RuneScape", "Old School RuneScape", true},
		{"control characters removed", "Mine\ncraft\t\x00", "Minecraft", true},
		{"line and paragraph separators removed", "Mine craft ", "Minecraft", true},
		{"bidi embeddings and overrides removed", "‮Minecraft‬", "Minecraft", true},
		{"bidi isolates removed", "⁦Minecraft⁩", "Minecraft", true},
		{"joiner kept inside an emoji sequence", family + " Sims", family + " Sims", true},
		{"variation selector kept", "❤️ Hearts", "❤️ Hearts", true},
		{"accents kept", "Pokémon", "Pokémon", true},
		{"empty", "", "", false},
		{"only spaces", "   ", "", false},
		{"only controls", "\n\t‮", "", false},
		{"only a joiner", "‍", "", false},
		{"only a Hangul filler", "ㅤ", "", false},
		{"only blank braille", "⠀⠀", "", false},
		{"only a combining mark", "́", "", false},
		{"128 code points", strings.Repeat("a", 128), strings.Repeat("a", 128), true},
		{"129 code points", strings.Repeat("a", 129), "", false},
		// Counted in code points, not bytes or UTF-16 units: 128 four-byte emoji fit.
		{"128 astral code points", strings.Repeat("\U0001F3AE", 128), strings.Repeat("\U0001F3AE", 128), true},
		{"129 astral code points", strings.Repeat("\U0001F3AE", 129), "", false},
		// Removed runes do not count toward the limit; collapsed whitespace does not either.
		{"limit counted after sanitising", strings.Repeat("a", 64) + strings.Repeat(" ", 50) + strings.Repeat("‮", 20) + strings.Repeat("b", 63), strings.Repeat("a", 64) + " " + strings.Repeat("b", 63), true},
	}
	for _, c := range cases {
		got, ok := SanitizeActivityName(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: SanitizeActivityName(%q) = %q, %v; want %q, %v", c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}

// started_at is clamped, never refused: a desktop whose clock runs fast still shares.
func TestClampActivityStart(t *testing.T) {
	now := time.UnixMilli(1_761_000_000_000)
	n := now.UnixMilli()
	oldest := n - MaxActivityAge.Milliseconds()
	cases := []struct {
		name   string
		in     int64
		expect int64
	}{
		{"zero means now", 0, n},
		{"a minute ago", n - 60_000, n - 60_000},
		{"exactly now", n, n},
		{"a few seconds ahead", n + 5_000, n},
		{"an hour ahead", n + 3_600_000, n},
		{"a year ahead", n + 365*24*3_600_000, n},
		{"exactly seven days ago", oldest, oldest},
		{"eight days ago", n - 8*24*3_600_000, oldest},
		{"negative", -1, oldest},
	}
	for _, c := range cases {
		if got := ClampActivityStart(c.in, now); got != c.expect {
			t.Errorf("%s: ClampActivityStart(%d) = %d, want %d", c.name, c.in, got, c.expect)
		}
	}
}

func TestNormalizeActivities(t *testing.T) {
	now := time.UnixMilli(1_761_000_000_000)
	start := now.UnixMilli() - 3_600_000
	ok := Activity{Type: "playing", Name: " Minecraft ", StartedAt: start}

	got, err := NormalizeActivities([]Activity{ok}, now)
	if err != nil || len(got) != 1 || got[0] != (Activity{Type: "playing", Name: "Minecraft", StartedAt: start}) {
		t.Fatalf("one valid activity: %+v, %v", got, err)
	}
	if got, err := NormalizeActivities(nil, now); err != nil || got != nil {
		t.Errorf("none: %+v, %v", got, err)
	}
	if got, err := NormalizeActivities([]Activity{}, now); err != nil || got != nil {
		t.Errorf("empty list: %+v, %v", got, err)
	}
	invalid := map[string][]Activity{
		"two activities":   {ok, ok},
		"unknown type":     {{Type: "listening", Name: "Radio"}},
		"empty type":       {{Name: "Minecraft"}},
		"type case":        {{Type: "Playing", Name: "Minecraft"}},
		"blank name":       {{Type: "playing", Name: " ​ "}},
		"name too long":    {{Type: "playing", Name: strings.Repeat("x", MaxActivityNameRunes+1)}},
		"one bad of a few": {ok, {Type: "playing"}},
	}
	for name, in := range invalid {
		if got, err := NormalizeActivities(in, now); err != ErrInvalidActivity || got != nil {
			t.Errorf("%s: %+v, %v; want ErrInvalidActivity", name, got, err)
		}
	}
	// A clamped start, not a refusal.
	got, err = NormalizeActivities([]Activity{{Type: "playing", Name: "Minecraft", StartedAt: now.UnixMilli() + 600_000}}, now)
	if err != nil || got[0].StartedAt != now.UnixMilli() {
		t.Errorf("future start: %+v, %v", got, err)
	}
}

// Nobody else sees an activity while the user reads offline to them - offline, or invisible
// (which ToPublicPresence turns into offline for others) - but the user's own view keeps it.
func TestWithActivities(t *testing.T) {
	acts := []Activity{{Type: "playing", Name: "Minecraft", StartedAt: 1}}
	cases := []struct {
		name      string
		p         UserPresence
		forOthers bool
		shown     bool
	}{
		{"online, others", UserPresence{Online: true, Status: "online"}, true, true},
		{"dnd, others", UserPresence{Online: true, Status: "dnd"}, true, true},
		{"idle, self", UserPresence{Online: true, Status: "idle"}, false, true},
		{"invisible, others", UserPresence{Online: true, Status: "invisible"}, true, false},
		{"invisible, self", UserPresence{Online: true, Status: "invisible"}, false, true},
		{"offline, others", UserPresence{Online: false, Status: "online"}, true, false},
		{"offline, self", UserPresence{Online: false, Status: "dnd"}, false, false},
	}
	for _, c := range cases {
		pub := WithActivities(ToPublicPresence(c.p, c.forOthers), acts)
		if shown := len(pub.Activities) == 1; shown != c.shown {
			t.Errorf("%s: activities %+v, want shown=%v", c.name, pub.Activities, c.shown)
		}
	}
	// The slice is the presence's own: changing the caller's does not reach it.
	pub := WithActivities(PublicPresence{Status: "online"}, acts)
	acts[0].Name = "changed"
	if pub.Activities[0].Name != "Minecraft" {
		t.Error("WithActivities shares the caller's slice")
	}
	if pub := WithActivities(PublicPresence{Status: "online", Activities: acts}, nil); pub.Activities != nil {
		t.Errorf("no activities left %+v in place", pub.Activities)
	}
}

// On the wire activities are additive: absent when there are none, so an older client or
// peer sees exactly what it saw before.
func TestPublicPresenceJSON(t *testing.T) {
	raw, _ := json.Marshal(PublicPresence{Status: "online"})
	if string(raw) != `{"status":"online"}` {
		t.Errorf("no activity: %s", raw)
	}
	raw, _ = json.Marshal(WithActivities(PublicPresence{Status: "dnd", CustomStatus: "busy"}, []Activity{{Type: "playing", Name: "Minecraft", StartedAt: 1761000000000}}))
	if want := `{"status":"dnd","custom_status":"busy","activities":[{"type":"playing","name":"Minecraft","started_at":1761000000000}]}`; string(raw) != want {
		t.Errorf("with activity: %s, want %s", raw, want)
	}
	var back PublicPresence
	if err := json.Unmarshal([]byte(`{"status":"online","custom_status":"x"}`), &back); err != nil || back.Activities != nil {
		t.Errorf("an old payload decodes to %+v, %v", back, err)
	}
}

func TestSameActivity(t *testing.T) {
	a := &Activity{Type: "playing", Name: "Minecraft", StartedAt: 5}
	b := &Activity{Type: "playing", Name: "Minecraft", StartedAt: 5}
	if !SameActivity(a, b) || !SameActivity(nil, nil) {
		t.Error("equal activities differ")
	}
	if SameActivity(a, nil) || SameActivity(nil, b) {
		t.Error("an activity equals none")
	}
	for _, c := range []*Activity{
		{Type: "playing", Name: "Minecraft", StartedAt: 6},
		{Type: "playing", Name: "minecraft", StartedAt: 5},
		{Type: "watching", Name: "Minecraft", StartedAt: 5},
	} {
		if SameActivity(a, c) {
			t.Errorf("%+v equals %+v", a, c)
		}
	}
}
