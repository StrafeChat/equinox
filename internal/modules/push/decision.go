package push

import "time"

// The notify decision for a message mirrors what the web client does when a message
// arrives while it is open (web.strafe.chat/src/lib/notifications.ts maybeNotifyMessage
// and lib/roomNotify.ts), so a phone with the app closed rings for exactly the messages
// the open app would have: never your own, not from someone you blocked, nothing while
// you are Do Not Disturb, nothing from a muted room, and otherwise per the room's own
// notify mode, falling back to the space/PM default.

// isSpaceRoom: rooms whose default comes from the space preference (web: any room listed
// under a space - text channels, voice rooms' text, threads).
func isSpaceRoom(roomType int) bool {
	return roomType == roomTypeSpaceText || roomType == roomTypeSpaceVoice || roomType == roomTypeThread
}

// messageMode resolves which messages of a room notify: the room's own override wins; with
// none, a space room uses the device's space preference - narrowed from "all" to
// "mentions" when the space defaults to only mentions - and a PM or group PM the PM one.
func messageMode(roomType int, settings *RoomSettings, prefs Prefs, spaceDefaultMentionsOnly bool) string {
	if settings != nil {
		switch settings.NotifyMode {
		case notifyModeAll:
			return ModeAll
		case notifyModeMentions:
			return ModeMentions
		case notifyModeNone:
			return ModeNone
		}
	}
	if isSpaceRoom(roomType) {
		mode := prefs.Space
		if mode == ModeAll && spaceDefaultMentionsOnly {
			mode = ModeMentions
		}
		return mode
	}
	return prefs.PM
}

// messageDecision is everything the per-recipient message decision looks at.
type messageDecision struct {
	recipient *UserInfo
	senderID  int64
	roomType  int
	settings  *RoomSettings
	prefs     Prefs
	// spaceDefaultMentionsOnly: the space's default notification level is "only mentions".
	spaceDefaultMentionsOnly bool
	// mentioned: the message mentions the recipient directly, through a role, or with an
	// @everyone/@here that was allowed to notify.
	mentioned bool
	now       time.Time
}

// recipientWants reports the user-level part: the parts that hold for every device.
func (d *messageDecision) recipientWants() bool {
	if d.recipient == nil || d.recipient.ID == d.senderID {
		return false
	}
	if d.recipient.blocked(d.senderID) {
		return false
	}
	// Do Not Disturb silences message notifications (web: notificationsSuppressedByStatus);
	// unread counts still move, so the activity is there when they look.
	if d.recipient.Status == "dnd" {
		return false
	}
	return !d.settings.mutedAt(d.now)
}

// notify is the full decision for one subscription.
func (d *messageDecision) notify() bool {
	if !d.recipientWants() {
		return false
	}
	switch messageMode(d.roomType, d.settings, d.prefs, d.spaceDefaultMentionsOnly) {
	case ModeAll:
		return true
	case ModeMentions:
		return d.mentioned
	default:
		return false
	}
}

// notifyCall is the decision for an incoming call. The web client rings whatever the
// status (its incoming-call prompt does not consult Do Not Disturb), so this only honours
// the device's own calls switch and a block - a call can only be placed in a PM or group
// PM the recipient is in, and a muted conversation still rings, as it does on the web.
func notifyCall(recipient *UserInfo, callerID int64, prefs Prefs) bool {
	if recipient == nil || recipient.ID == callerID || !prefs.Calls {
		return false
	}
	return !recipient.blocked(callerID)
}
