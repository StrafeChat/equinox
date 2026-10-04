// Package permissions defines space/room permission bitmasks (Discord-style OR combine).
//
// Bit positions are permanent once shipped: role.permissions is a stored int64, so an
// existing space's roles would silently change meaning if a bit were renumbered. Only
// append new bits at the next free position; never reuse or reorder one.
package permissions

// Text/room-relevant bits used in space_roles.permissions and per-room overrides
// (SPACE_ROLE_PERM_ROWS / ROOM_OVERRIDE_PERM_ROWS on the frontend).
const (
	PermViewRoom           int64 = 1 << 0
	PermSendMessages       int64 = 1 << 1
	PermReadMessageHistory int64 = 1 << 2
	PermAddReactions       int64 = 1 << 3
	PermUseExternalEmojis  int64 = 1 << 4
	PermMentionEveryone    int64 = 1 << 5
	PermManageMessages     int64 = 1 << 6
)

// General/membership bits: space-wide only, not applicable to a single room override.
const (
	PermManageRoles   int64 = 1 << 7
	PermManageRooms   int64 = 1 << 8 // "Manage Channels" in Discord's naming
	PermKickMembers   int64 = 1 << 9
	PermBanMembers    int64 = 1 << 10
	PermAdministrator int64 = 1 << 11
	PermManageSpace   int64 = 1 << 12 // "Manage Server" in Discord's naming
	PermCreateInvite  int64 = 1 << 13
	PermManageEmojis  int64 = 1 << 14 // upload, rename, delete the space's custom emoji
)

// Voice bits. Room-scoped (a voice room can override each of them), same meaning as
// Discord's: Connect gates joining at all, Speak gates the microphone, Video gates the
// camera and screen sharing, the three *Members bits are moderator actions on people in
// the room, UseVAD lets a member use voice activity instead of push-to-talk, and
// PrioritySpeaker lowers everyone else while they talk.
const (
	PermConnect         int64 = 1 << 15
	PermSpeak           int64 = 1 << 16
	PermVideo           int64 = 1 << 17 // camera and screen share ("Video" + "Stream" in Discord)
	PermMuteMembers     int64 = 1 << 18
	PermDeafenMembers   int64 = 1 << 19
	PermMoveMembers     int64 = 1 << 20 // also disconnect
	PermUseVAD          int64 = 1 << 21
	PermPrioritySpeaker int64 = 1 << 22
)

// Appended after voice shipped: bit positions are permanent, so a new bit goes at the end
// whatever its category. Room-scoped text bit.
const (
	// PermAttachFiles lets a member upload files and images with a message (Discord's
	// "Attach Files"). On by default for @everyone; without it a member can still post text.
	PermAttachFiles int64 = 1 << 23
)

// AllVoice is every voice bit.
const AllVoice = PermConnect | PermSpeak | PermVideo | PermMuteMembers | PermDeafenMembers |
	PermMoveMembers | PermUseVAD | PermPrioritySpeaker

// DefaultVoice is what @everyone gets in voice: join, talk, share video, use voice
// activity. The moderator bits and priority speaker are off.
const DefaultVoice = PermConnect | PermSpeak | PermVideo | PermUseVAD

// DefaultEveryone is @everyone for new spaces: Discord's defaults for a member with no
// roles - can view/talk/react/invite and use voice, cannot manage anything or mass-mention.
const DefaultEveryone = PermViewRoom | PermSendMessages | PermReadMessageHistory |
	PermAddReactions | PermUseExternalEmojis | PermCreateInvite | PermAttachFiles | DefaultVoice

// AllRoom is every room-scoped bit: space owner's implicit permissions in any room, and
// what Administrator resolves to for EffectiveChannelPermissions.
const AllRoom = PermViewRoom | PermSendMessages | PermReadMessageHistory |
	PermAddReactions | PermUseExternalEmojis | PermMentionEveryone | PermManageMessages |
	PermAttachFiles | AllVoice

// AllSpace is every space-scoped bit: space owner's implicit permissions, and what
// Administrator resolves to for SpacePermissionBase.
const AllSpace = AllRoom | PermManageRoles | PermManageRooms | PermKickMembers |
	PermBanMembers | PermAdministrator | PermManageSpace | PermCreateInvite | PermManageEmojis

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
