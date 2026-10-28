package auth

import (
	"errors"
	"strings"
	"time"
	"unicode"
)

// An activity is what someone is doing right now - in v1 only the game they are playing,
// reported by the desktop app (or a bot) over the gateway. It is presence, not profile: it
// lives in Redis for as long as the connection that reported it (see internal/stargate's
// activity store), never in ScyllaDB, never in UserPresence or the user cache, and it is
// shown to exactly the people who see the user's status, never while they look offline.

// ActivityTypePlaying is the only activity type in v1.
const ActivityTypePlaying = "playing"

const (
	// MaxActivities is how many activities one report or relay may carry.
	MaxActivities = 1
	// MaxActivityNameRunes bounds a name, counted in code points (JS [...s].length, Rust
	// chars().count()) after sanitising.
	MaxActivityNameRunes = 128
	// MaxActivityAge is how far back started_at may reach; older is clamped to it.
	MaxActivityAge = 7 * 24 * time.Hour
)

// ErrInvalidActivity: a report or relay that breaks the rules below.
var ErrInvalidActivity = errors.New("invalid activity")

// Activity is one entry of a presence's activities. StartedAt is unix milliseconds.
type Activity struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	StartedAt int64  `json:"started_at,omitempty"`
}

// SanitizeActivityName applies the display-text rule to a game name: forbidden runes
// (control characters, line/paragraph separators, bidi controls) are removed, runs of
// whitespace collapse to one space and the ends are trimmed. What is left must hold at
// least one visible rune and at most MaxActivityNameRunes runes. Joiners (U+200D) and
// variation selectors (U+FE0F) are kept, so emoji sequences survive. ok is false when the
// name cannot be used.
func SanitizeActivityName(s string) (name string, ok bool) {
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	space := false
	visible := false
	for _, r := range s {
		if ForbiddenTextRune(r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = n > 0 // a leading run is trimmed; an inner one becomes one space
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
		if IsVisibleRune(r) {
			visible = true
		}
		if n > MaxActivityNameRunes {
			return "", false
		}
	}
	if !visible {
		return "", false
	}
	return b.String(), true
}

// ClampActivityStart puts started_at (unix ms) into [now - MaxActivityAge, now]. 0 means
// "now", and a time in the future - a desktop whose clock runs fast - becomes now rather
// than being refused, so a skewed clock costs a few minutes of "for 1 h 2 min", not the
// whole activity.
func ClampActivityStart(v int64, now time.Time) int64 {
	n := now.UnixMilli()
	if v == 0 || v > n {
		return n
	}
	if oldest := n - MaxActivityAge.Milliseconds(); v < oldest {
		return oldest
	}
	return v
}

// NormalizeActivity checks one activity and returns it sanitised: the type must be
// "playing", the name must survive SanitizeActivityName, started_at is clamped.
func NormalizeActivity(a Activity, now time.Time) (Activity, error) {
	if a.Type != ActivityTypePlaying {
		return Activity{}, ErrInvalidActivity
	}
	name, ok := SanitizeActivityName(a.Name)
	if !ok {
		return Activity{}, ErrInvalidActivity
	}
	return Activity{Type: a.Type, Name: name, StartedAt: ClampActivityStart(a.StartedAt, now)}, nil
}

// NormalizeActivities is NormalizeActivity over a whole list: empty means none, more than
// MaxActivities is invalid, and one bad entry makes the whole list invalid. The same rule
// serves the gateway's op 9 and federation's POST /users/presence.
func NormalizeActivities(in []Activity, now time.Time) ([]Activity, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxActivities {
		return nil, ErrInvalidActivity
	}
	out := make([]Activity, 0, len(in))
	for _, a := range in {
		n, err := NormalizeActivity(a, now)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// WithActivities attaches acts to a presence that is not offline. ToPublicPresence with
// forOthers already turned invisible into offline, so this one rule keeps an invisible or
// offline user's activity from everyone else, while the self view (forOthers false) keeps it
// for an invisible user's own devices. The slice is copied, never shared.
func WithActivities(pub PublicPresence, acts []Activity) PublicPresence {
	if pub.Status == "offline" || len(acts) == 0 {
		pub.Activities = nil
		return pub
	}
	pub.Activities = append([]Activity(nil), acts...)
	return pub
}

// SameActivity compares what others see of two activities - type, name and start - and
// nothing else (never when or from where it was reported). nil is "no activity".
func SameActivity(a, b *Activity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Type == b.Type && a.Name == b.Name && a.StartedAt == b.StartedAt
}
