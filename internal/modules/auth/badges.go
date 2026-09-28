package auth

// Profile badges are stored in User.Flags as a bitfield - the same shape as Discord's
// public_flags. Only an instance administrator assigns them (see the instance module);
// the Bot badge is not stored here, it is derived from the account's bot flag.
//
// Bits are append-only: never reuse a value, or an account keeps a badge that now means
// something else. New badges take the next free bit.
const (
	BadgeFounder      = 1 << 0 // the person who set the instance up
	BadgeStaff        = 1 << 1 // instance staff / administrator
	BadgeSupport      = 1 << 2 // helps run support
	BadgeContributor  = 1 << 3 // contributed code
	BadgeTranslator   = 1 << 4 // contributed translations
	BadgeBugDiscloser = 1 << 5 // responsibly disclosed a security issue
	BadgeAlphaTester  = 1 << 6 // was here for the alpha
)

// AllBadges is the mask of every assignable badge bit. Anything outside it is rejected on
// assignment and masked off on display, so a stray bit can never render as a badge.
const AllBadges = BadgeFounder | BadgeStaff | BadgeSupport | BadgeContributor |
	BadgeTranslator | BadgeBugDiscloser | BadgeAlphaTester

// PublicFlags is the badge bitfield to show for a user: their stored flags with any
// non-badge bits stripped.
func PublicFlags(u *User) int {
	if u == nil {
		return 0
	}
	return u.Flags & AllBadges
}
