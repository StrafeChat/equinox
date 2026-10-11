package push

import (
	"testing"
	"time"
)

func TestMessageDecision(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	bob := func() *UserInfo { return &UserInfo{ID: 2, Username: "bob", Status: "online"} }
	const alice = 1
	prefs := func(pm, space string) Prefs { return Prefs{Preview: true, PM: pm, Space: space, Calls: true} }

	cases := []struct {
		name string
		mod  func(d *messageDecision)
		want bool
	}{
		{"PM, default prefs", func(d *messageDecision) {}, true},
		{"own message", func(d *messageDecision) { d.senderID = 2 }, false},
		{"no such recipient", func(d *messageDecision) { d.recipient = nil }, false},
		{"recipient blocked the sender", func(d *messageDecision) { d.recipient.Blocks = []int64{9, alice} }, false},
		{"do not disturb", func(d *messageDecision) { d.recipient.Status = "dnd" }, false},
		{"idle still notifies", func(d *messageDecision) { d.recipient.Status = "idle" }, true},
		{"muted", func(d *messageDecision) { d.settings = &RoomSettings{Muted: true} }, false},
		{"timed mute running", func(d *messageDecision) { d.settings = &RoomSettings{MutedUntil: &future} }, false},
		{"timed mute expired", func(d *messageDecision) { d.settings = &RoomSettings{MutedUntil: &past} }, true},
		{"PM pref none", func(d *messageDecision) { d.prefs = prefs(ModeNone, ModeAll) }, false},
		{"PM room set to mentions, not mentioned", func(d *messageDecision) { d.settings = &RoomSettings{NotifyMode: notifyModeMentions} }, false},
		{"PM room set to mentions, mentioned", func(d *messageDecision) {
			d.settings = &RoomSettings{NotifyMode: notifyModeMentions}
			d.mentioned = true
		}, true},
		{"PM room set to nothing", func(d *messageDecision) { d.settings = &RoomSettings{NotifyMode: notifyModeNone} }, false},
		{"PM room set to all beats pref none", func(d *messageDecision) {
			d.prefs = prefs(ModeNone, ModeNone)
			d.settings = &RoomSettings{NotifyMode: notifyModeAll}
		}, true},
		{"group PM follows the PM pref", func(d *messageDecision) { d.roomType = roomTypeGroupPM; d.prefs = prefs(ModeNone, ModeAll) }, false},
		{"space room, default mentions, not mentioned", func(d *messageDecision) { d.roomType = roomTypeSpaceText }, false},
		{"space room, mentioned directly", func(d *messageDecision) { d.roomType = roomTypeSpaceText; d.mentioned = true }, true},
		{"space room, pref all", func(d *messageDecision) { d.roomType = roomTypeSpaceText; d.prefs = prefs(ModeAll, ModeAll) }, true},
		{"space room, pref all, space defaults to mentions", func(d *messageDecision) {
			d.roomType = roomTypeSpaceText
			d.prefs = prefs(ModeAll, ModeAll)
			d.spaceDefaultMentionsOnly = true
		}, false},
		{"space default mentions, but the room is set to all", func(d *messageDecision) {
			d.roomType = roomTypeSpaceText
			d.spaceDefaultMentionsOnly = true
			d.settings = &RoomSettings{NotifyMode: notifyModeAll}
		}, true},
		{"space room, pref none, mentioned", func(d *messageDecision) {
			d.roomType = roomTypeSpaceText
			d.prefs = prefs(ModeAll, ModeNone)
			d.mentioned = true
		}, false},
		{"thread uses the space pref", func(d *messageDecision) { d.roomType = roomTypeThread; d.prefs = prefs(ModeAll, ModeAll) }, true},
		{"voice room text uses the space pref", func(d *messageDecision) { d.roomType = roomTypeSpaceVoice; d.prefs = prefs(ModeAll, ModeNone) }, false},
		{"muted beats a mention", func(d *messageDecision) {
			d.roomType = roomTypeSpaceText
			d.mentioned = true
			d.settings = &RoomSettings{Muted: true}
		}, false},
		{"DND beats a mention", func(d *messageDecision) {
			d.roomType = roomTypeSpaceText
			d.mentioned = true
			d.recipient.Status = "dnd"
		}, false},
	}
	for _, c := range cases {
		d := messageDecision{
			recipient: bob(), senderID: alice, roomType: roomTypePM,
			prefs: DefaultPrefs(), now: now,
		}
		c.mod(&d)
		if got := d.notify(); got != c.want {
			t.Errorf("%s: notify = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCallDecision(t *testing.T) {
	bob := &UserInfo{ID: 2, Status: "dnd"}
	p := DefaultPrefs()
	// The web client rings through Do Not Disturb; so does the phone.
	if !notifyCall(bob, 1, p) {
		t.Error("call not rung for a DND recipient")
	}
	p.Calls = false
	if notifyCall(bob, 1, p) {
		t.Error("calls switched off but rung")
	}
	if notifyCall(&UserInfo{ID: 2, Blocks: []int64{1}}, 1, DefaultPrefs()) {
		t.Error("blocked caller rung")
	}
	if notifyCall(&UserInfo{ID: 1}, 1, DefaultPrefs()) {
		t.Error("caller rung themselves")
	}
}

func TestSubscriptionPrefsDefaults(t *testing.T) {
	// A row with unset (legacy/garbled) modes reads as the defaults.
	s := Subscription{PrefPreview: false, PrefCalls: true, PrefPM: "", PrefSpace: "weird"}
	p := s.Prefs()
	if p.PM != ModeAll || p.Space != ModeMentions || p.Preview || !p.Calls {
		t.Errorf("prefs = %+v", p)
	}
	if _, err := applyPrefs(DefaultPrefs(), PrefsPatch{PM: strPtr(ModeMentions)}); err == nil {
		t.Error("PM mode 'mentions' accepted (PMs are all or none)")
	}
	got, err := applyPrefs(DefaultPrefs(), PrefsPatch{Space: strPtr(ModeAll), Preview: boolPtr(false)})
	if err != nil || got.Space != ModeAll || got.Preview || got.PM != ModeAll || !got.Calls {
		t.Errorf("partial prefs = %+v, %v", got, err)
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
