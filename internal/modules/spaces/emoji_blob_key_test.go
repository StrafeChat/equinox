package spaces

import "testing"

// The key an emoji's blob is deleted under must be one this instance minted for that very
// emoji: the URL on the row can come from a peer, and a key derived from it could name
// anything in the store.
func TestEmojiBlobKeyFor(t *testing.T) {
	const space, emoji = int64(42), int64(7)
	ok := []string{"emojis/42/7.png", "emojis/42/7.gif", "emojis/42/7"}
	for _, k := range ok {
		if !emojiBlobKeyFor(space, emoji, k) {
			t.Errorf("%q should be the emoji's own key", k)
		}
	}
	bad := []string{
		"emojis/42/71.png",         // a different emoji with a longer id
		"emojis/43/7.png",          // another space
		"avatars/42/7.png",         // another kind of object
		"emojis/42/7.png/../x.png", // a path under it
		"emojis/42/7../other",      // dots in the extension
		"emojis/42/7.png.sig",      // two extensions
		"",
	}
	for _, k := range bad {
		if emojiBlobKeyFor(space, emoji, k) {
			t.Errorf("%q must not be treated as the emoji's key", k)
		}
	}
}
