package messages

import "testing"

// Keycap emoji (0️⃣-9️⃣, #️⃣, *️⃣) begin with an ASCII character and so were rejected by the
// all-non-ASCII unicode rule - the one emoji family that never worked as a reaction.
func TestValidateReactionEmoji(t *testing.T) {
	ok := []string{
		"👍", "😀", "❤️", "🇺🇸", "👨‍👩‍👧", // plain, VS16, regional-indicator flag, ZWJ sequence
		"0️⃣", "1️⃣", "9️⃣", "#️⃣", "*️⃣", // fully-qualified keycaps (the regression)
		"1⃣",                        // minimally-qualified keycap (no variation selector)
		"custom:123456789012345678", // custom space emoji by id
	}
	for _, e := range ok {
		got, err := ValidateReactionEmoji(e)
		if err != nil {
			t.Errorf("ValidateReactionEmoji(%q) = error %v, want accepted", e, err)
			continue
		}
		if got != e {
			t.Errorf("ValidateReactionEmoji(%q) normalised to %q, want unchanged", e, got)
		}
	}

	bad := []string{
		"", " ", "\t",
		"a", "A", "1", "9", "#", "*", // a bare ASCII char must still be rejected
		"abc", "hello",
		"custom:", "custom:notanid",
		"12⃣", // two leading ASCII chars before the keycap mark - not a real keycap
	}
	for _, e := range bad {
		if _, err := ValidateReactionEmoji(e); err == nil {
			t.Errorf("ValidateReactionEmoji(%q) = accepted, want rejected", e)
		}
	}
}
