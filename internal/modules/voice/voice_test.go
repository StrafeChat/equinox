package voice

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/StrafeChat/equinox/internal/config"
)

func TestIdentityRoundTrip(t *testing.T) {
	st := &State{UserID: 2102223172425220096, SessionID: "0a1b2c3d4e5f"}
	uid, session, ok := parseIdentity(st.Identity())
	if !ok || uid != st.UserID || session != st.SessionID {
		t.Fatalf("parseIdentity(%q) = %d %q %v", st.Identity(), uid, session, ok)
	}
	for _, bad := range []string{"", "abc", ".x", "12.", "notanumber.abc"} {
		if _, _, ok := parseIdentity(bad); ok && bad != "12." {
			t.Errorf("parseIdentity(%q) accepted", bad)
		}
	}
}

func TestRoomNameRoundTrip(t *testing.T) {
	rid, ok := roomIDFromName(RoomName(2102226824804171777))
	if !ok || rid != 2102226824804171777 {
		t.Fatalf("roomIDFromName = %d %v", rid, ok)
	}
	if _, ok := roomIDFromName("other_1"); ok {
		t.Error("foreign room name accepted")
	}
}

// The publish sources are what LiveKit enforces: a server mute must drop the
// microphone even for someone allowed to speak, and no Video permission means no
// camera or screen (with its audio) at all.
func TestSourcesFor(t *testing.T) {
	cases := []struct {
		speak, video, muted bool
		want                []livekit.TrackSource
	}{
		{true, true, false, []livekit.TrackSource{livekit.TrackSource_MICROPHONE, livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO}},
		{true, true, true, []livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO}},
		{false, true, false, []livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO}},
		{true, false, false, []livekit.TrackSource{livekit.TrackSource_MICROPHONE}},
		{false, false, false, nil},
	}
	for _, c := range cases {
		got := sourcesFor(c.speak, c.video, c.muted)
		if len(got) != len(c.want) {
			t.Fatalf("sourcesFor(%v,%v,%v) = %v, want %v", c.speak, c.video, c.muted, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("sourcesFor(%v,%v,%v) = %v, want %v", c.speak, c.video, c.muted, got, c.want)
			}
		}
	}
}

// A token must be scoped to exactly one room and carry the publish restrictions.
func TestTokenGrants(t *testing.T) {
	lk := NewLiveKit(config.VoiceConfig{
		Enabled: true, PublicURL: "ws://localhost:7880", InternalURL: "http://localhost:7880",
		APIKey: "devkey", APISecret: "secretsecretsecretsecretsecretsecret",
	})
	tok, err := lk.Token("1.abc", "Alice", RoomName(42), false, sourcesFor(true, false, false))
	if err != nil {
		t.Fatal(err)
	}
	// JWT payload is the middle segment.
	parts := splitJWT(tok)
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %s", tok)
	}
	var claims struct {
		Sub   string `json:"sub"`
		Name  string `json:"name"`
		Video struct {
			Room              string   `json:"room"`
			RoomJoin          bool     `json:"roomJoin"`
			CanSubscribe      bool     `json:"canSubscribe"`
			CanPublish        bool     `json:"canPublish"`
			CanPublishSources []string `json:"canPublishSources"`
		} `json:"video"`
	}
	if err := json.Unmarshal(parts[1], &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Sub != "1.abc" || claims.Name != "Alice" || claims.Video.Room != "strafe_42" || !claims.Video.RoomJoin {
		t.Errorf("unexpected claims: %+v", claims)
	}
	if claims.Video.CanSubscribe {
		t.Error("deafened participant may subscribe")
	}
	if !claims.Video.CanPublish || len(claims.Video.CanPublishSources) != 1 || claims.Video.CanPublishSources[0] != "microphone" {
		t.Errorf("publish sources = %v", claims.Video.CanPublishSources)
	}
}

func TestCallRinging(t *testing.T) {
	c := &Call{RoomID: 1, StartedBy: 2, StartedAt: time.Now(), Ringing: []int64{3, 4}}
	if !c.isRinging(3) || c.isRinging(2) {
		t.Fatal("isRinging wrong")
	}
	if !c.stopRinging(3) || c.stopRinging(3) {
		t.Fatal("stopRinging should remove once")
	}
	if got := c.ringingStrings(); len(got) != 1 || got[0] != "4" {
		t.Fatalf("ringing = %v", got)
	}
}

// A call's ringing list is int64 in Redis but must be strings on the wire.
func TestCallViewIDs(t *testing.T) {
	c := &Call{RoomID: 2102225374388682752, StartedBy: 2102223172425220096, Ringing: []int64{2102223763486539776}}
	raw, err := json.Marshal(c.View())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	ring, _ := m["ringing"].([]interface{})
	if len(ring) != 1 || ring[0] != "2102223763486539776" {
		t.Errorf("ringing = %v, want one string id", m["ringing"])
	}
	if m["started_by"] != "2102223172425220096" || m["room_id"] != "2102225374388682752" {
		t.Errorf("ids = %v / %v", m["started_by"], m["room_id"])
	}
	var nilCall *Call
	if nilCall.View() != nil {
		t.Error("nil call should view as nil")
	}
}

// Ids cross the wire as strings (JavaScript numbers lose precision on snowflakes).
func TestStateWireIDs(t *testing.T) {
	raw, err := json.Marshal(&State{UserID: 2102223172425220096, RoomID: 2102226824804171777, SpaceID: 2102226824774811648})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"user_id", "room_id", "space_id"} {
		if _, ok := m[k].(string); !ok {
			t.Errorf("%s is %T, want string", k, m[k])
		}
	}
}
