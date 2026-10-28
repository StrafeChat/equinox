package spaces

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// Co-members see what a member is playing - on the member list, in one read for the whole
// list, and on the single member - unless the member reads offline to them.
func TestMemberPayloadsCarryActivities(t *testing.T) {
	ctx := context.Background()
	store := stargate.NewMemoryActivityStore()
	prev := stargate.CurrentActivityStore()
	stargate.SetActivityStore(store)
	t.Cleanup(func() { stargate.SetActivityStore(prev) })

	r := profileFixture()
	users := &profileUsers{users: map[int64]*auth.User{}}
	presence := map[int64]auth.UserPresence{
		pfOwner:  {Online: true, Status: "online"},
		pfAdmin:  {Online: true, Status: "invisible"},
		pfMod:    {Online: false, Status: "dnd"},
		pfMember: {Online: true, Status: "idle"},
		pfPlain:  {Online: true, Status: "online"},
	}
	for uid := range r.members {
		users.users[uid] = &auth.User{ID: uid, Username: "user", Presence: presence[uid]}
		_, _ = store.SetConn(ctx, uid, uid*100, auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: 1}, time.Now().UnixMilli())
	}
	as := int64(pfPlain)
	app := profileApp(r, users, &as)

	get := func(path string, out any) {
		t.Helper()
		res, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, res.StatusCode)
		}
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	type member struct {
		ID       string              `json:"id"`
		Presence auth.PublicPresence `json:"presence"`
	}
	shown := map[string]bool{"1": true, "2": false, "3": false, "4": true, "5": true}

	before := store.Reads()
	var list []member
	get("/spaces/100/members", &list)
	if len(list) != 5 {
		t.Fatalf("%d members", len(list))
	}
	for _, m := range list {
		if got := len(m.Presence.Activities) == 1; got != shown[m.ID] {
			t.Errorf("member %s (%s): activities %+v", m.ID, m.Presence.Status, m.Presence.Activities)
		}
	}
	if n := store.Reads() - before; n != 1 {
		t.Errorf("the member list cost %d activity reads, want 1", n)
	}

	var one member
	get("/spaces/100/members/4", &one)
	if len(one.Presence.Activities) != 1 || one.Presence.Activities[0].Name != "Minecraft" {
		t.Errorf("single member: %+v", one.Presence)
	}
	one = member{}
	get("/spaces/100/members/2", &one)
	if len(one.Presence.Activities) != 0 || one.Presence.Status != "offline" {
		t.Errorf("an invisible member: %+v", one.Presence)
	}

	// The payload builder itself attaches only what it is given, and only while visible.
	s := NewService(r, nil, users, nil, nil)
	acts := []auth.Activity{{Type: "playing", Name: "Factorio"}}
	if p := s.memberUserFields(users.users[pfOwner], acts)["presence"].(auth.PublicPresence); len(p.Activities) != 1 {
		t.Errorf("visible member with an activity: %+v", p)
	}
	if p := s.memberUserFields(users.users[pfMod], acts)["presence"].(auth.PublicPresence); len(p.Activities) != 0 {
		t.Errorf("offline member: %+v", p)
	}
	if p := s.memberUserFields(users.users[pfOwner], nil)["presence"].(auth.PublicPresence); p.Activities != nil {
		t.Errorf("no activity given: %+v", p)
	}
}
