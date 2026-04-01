// Package permissions defines space/room permission bitmasks (Discord-style OR combine).
package permissions

// Room-relevant bits used in space_roles.permissions and room overrides.
const (
	PermViewRoom       int64 = 1 << 0
	PermSendMessages       int64 = 1 << 1
	PermReadMessageHistory int64 = 1 << 2
	PermAddReactions       int64 = 1 << 3
	PermUseExternalEmojis  int64 = 1 << 4
	PermMentionEveryone    int64 = 1 << 5
	PermManageMessages     int64 = 1 << 6
	PermManageRoles        int64 = 1 << 7
	PermManageRooms        int64 = 1 << 8
	PermKickMembers        int64 = 1 << 9
	PermBanMembers         int64 = 1 << 10
	PermAdministrator      int64 = 1 << 11
	PermManageSpace        int64 = 1 << 12
)

// DefaultEveryone is typical @everyone for new spaces (mention @everyone off by default).
const DefaultEveryone = PermViewRoom | PermSendMessages | PermReadMessageHistory |
	PermAddReactions | PermUseExternalEmojis

// All room is used for space owner bypass and administrator room access.
const AllRoom = PermViewRoom | PermSendMessages | PermReadMessageHistory |
	PermAddReactions | PermUseExternalEmojis | PermMentionEveryone | PermManageMessages

// AllSpace includes management bits (not all possible future bits, but practical admin).
const AllSpace = AllRoom | PermManageRoles | PermManageRooms | PermKickMembers |
	PermBanMembers | PermAdministrator | PermManageSpace

// ApplyOverwrites applies Discord-style allow/deny overwrites in order (each step: clear deny, then set allow).
func ApplyOverwrites(base int64, overwrites []struct{ Allow, Deny int64 }) int64 {
	p := base
	for _, o := range overwrites {
		p = (p &^ o.Deny) | o.Allow
	}
	return p
}

func Has(perm, bit int64) bool {
	return perm&bit == bit
}
