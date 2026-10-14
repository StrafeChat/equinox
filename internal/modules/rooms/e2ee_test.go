package rooms

import "testing"

func boolp(b bool) *bool { return &b }

// A null e2ee_enabled means "the default for this kind of room": encrypted for PMs and
// group PMs, plaintext for anything in a space. An explicit value always wins.
func TestE2EEOnDefaults(t *testing.T) {
	space := int64(7)
	cases := []struct {
		name  string
		room  Room
		wantE bool
	}{
		{"pm null", Room{Type: TypePM}, true},
		{"group pm null", Room{Type: TypeGroupPM}, true},
		{"pm off", Room{Type: TypePM, E2EEEnabled: boolp(false)}, false},
		{"group pm off", Room{Type: TypeGroupPM, E2EEEnabled: boolp(false)}, false},
		{"space text null", Room{Type: TypeSpaceText, SpaceID: &space}, false},
		{"space voice null", Room{Type: TypeSpaceVoice, SpaceID: &space}, false},
		{"thread null", Room{Type: TypeThread, SpaceID: &space}, false},
		{"section null", Room{Type: TypeRoomSection, SpaceID: &space}, false},
		{"space text on", Room{Type: TypeSpaceText, SpaceID: &space, E2EEEnabled: boolp(true)}, true},
		{"thread on", Room{Type: TypeThread, SpaceID: &space, E2EEEnabled: boolp(true)}, true},
		{"space text off", Room{Type: TypeSpaceText, SpaceID: &space, E2EEEnabled: boolp(false)}, false},
	}
	for _, c := range cases {
		if got := c.room.E2EEOn(); got != c.wantE {
			t.Errorf("%s: E2EEOn() = %v, want %v", c.name, got, c.wantE)
		}
	}
	if (*Room)(nil).E2EEOn() {
		t.Error("nil room reported as encrypted")
	}
}

// GET /rooms and the ROOM_* events used to report every null-flag room as encrypted,
// space rooms and threads included.
func TestRoomWireFormsUseRoomDefault(t *testing.T) {
	space := int64(7)
	thread := RoomWithParticipants{Room: Room{ID: 1, Type: TypeThread, SpaceID: &space}}
	pm := RoomWithParticipants{Room: Room{ID: 2, Type: TypePM}, ParticipantIDs: []int64{3, 4}}

	if got := roomToJSON(thread)["e2ee_enabled"]; got != false {
		t.Errorf("GET /rooms thread with null flag: e2ee_enabled = %v, want false", got)
	}
	if got := roomToJSON(pm)["e2ee_enabled"]; got != true {
		t.Errorf("GET /rooms PM with null flag: e2ee_enabled = %v, want true", got)
	}
	s := &Service{}
	if got := s.RoomEventPayload(&thread)["e2ee_enabled"]; got != false {
		t.Errorf("room event thread with null flag: e2ee_enabled = %v, want false", got)
	}
	if got := s.RoomEventPayload(&pm)["e2ee_enabled"]; got != true {
		t.Errorf("room event PM with null flag: e2ee_enabled = %v, want true", got)
	}
}
