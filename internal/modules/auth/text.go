package auth

import "unicode"

// The rules for short display text that is shown in place of, or next to, someone's name:
// a space nickname, the name of the game someone is playing. They live here, in a leaf
// both the spaces module and the gateway import, so every such string obeys one rule.

// ForbiddenTextRune is what such a string may never hold anywhere in it: control
// characters, U+2028/U+2029 (which break the line as a newline does) and bidi controls.
func ForbiddenTextRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) || IsBidiControl(r)
}

// IsBidiControl is U+202A-202E (embeddings and overrides) and U+2066-2069 (isolates).
func IsBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// IsBlankFiller: letters and symbols that render as nothing (the Hangul fillers, the blank
// braille pattern) - the usual way to fake an empty name once whitespace is refused.
func IsBlankFiller(r rune) bool {
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800:
		return true
	}
	return false
}

// IsVisibleRune reports whether r shows something on its own: not a blank, not an
// invisible formatting character (a joiner, a variation selector) and not a combining mark,
// which needs something to combine with. Joiners and variation selectors stay allowed
// beside visible characters - emoji sequences need them - but cannot make a name alone.
func IsVisibleRune(r rune) bool {
	return !unicode.IsSpace(r) && !unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) && !IsBlankFiller(r)
}
