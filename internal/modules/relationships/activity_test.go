package relationships

import (
	"context"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// What someone is playing is shown to their friends, never to a person they merely have a
// pending request with (in either direction) or have blocked: sending someone a friend
// request is not their consent to be watched.

func presenceActivities(t *testing.T, user interface{}) []auth.Activity {
	t.Helper()
	m, ok := user.(map[string]interface{})
	if !ok {
		t.Fatalf("user is %T", user)
	}
	p, ok := m["presence"].(auth.PublicPresence)
	if !ok {
		t.Fatalf("presence is %T", m["presence"])
	}
	return p.Activities
}

func TestListRelationshipsActivitiesForFriendsOnly(t *testing.T) {
	svc, repo, users, _ := newTestService(t)
	ctx := context.Background()
	const (
		friendID   = 10
		blockedID  = 11
		incomingID = 12
		outgoingID = 13
		hiddenID   = 14 // a friend who is invisible
	)
	online := auth.UserPresence{Online: true, Status: "online"}
	for _, id := range []int64{friendID, blockedID, incomingID, outgoingID} {
		users.byID[id] = &auth.User{ID: id, Username: "u", Presence: online}
	}
	users.byID[hiddenID] = &auth.User{ID: hiddenID, Username: "h", Presence: auth.UserPresence{Online: true, Status: "invisible"}}
	alice := users.byID[aliceID]
	alice.Relationships = []int64{friendID, hiddenID}
	alice.Blocks = []int64{blockedID}
	_ = repo.CreateRequest(ctx, incomingID, aliceID)
	_ = repo.CreateRequest(ctx, aliceID, outgoingID)

	store := stargate.NewMemoryActivityStore()
	prev := stargate.CurrentActivityStore()
	stargate.SetActivityStore(store)
	t.Cleanup(func() { stargate.SetActivityStore(prev) })
	for _, id := range []int64{friendID, blockedID, incomingID, outgoingID, hiddenID} {
		_, _ = store.SetConn(ctx, id, id*10, auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: 1}, time.Now().UnixMilli())
	}

	list, err := svc.ListRelationships(ctx, aliceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("%d relationships, want 5", len(list))
	}
	for _, r := range list {
		acts := presenceActivities(t, r.User)
		switch {
		case r.ID == "10":
			if r.Type != TypeFriend || len(acts) != 1 || acts[0].Name != "Minecraft" {
				t.Errorf("friend: type %d, activities %+v", r.Type, acts)
			}
		case len(acts) != 0:
			t.Errorf("relationship %s (type %d) carries %+v", r.ID, r.Type, acts)
		}
	}
	// One read for the whole list, and only for friends who do not look offline.
	if store.Reads() != 1 {
		t.Errorf("%d activity reads for one list, want 1", store.Reads())
	}
}

// RELATIONSHIP_ADD for a new friend carries what they play; a block or a request never does.
func TestRelationshipEventsActivities(t *testing.T) {
	ctx := context.Background()
	store := stargate.NewMemoryActivityStore()
	prev := stargate.CurrentActivityStore()
	stargate.SetActivityStore(store)
	t.Cleanup(func() { stargate.SetActivityStore(prev) })
	u := &auth.User{ID: 20, Username: "gamer", Presence: auth.UserPresence{Online: true, Status: "dnd"}}
	_, _ = store.SetConn(ctx, u.ID, 200, auth.Activity{Type: "playing", Name: "Factorio", StartedAt: 1}, time.Now().UnixMilli())

	if acts := presenceActivities(t, relationshipAddPayload(ctx, u, TypeFriend)["user"]); len(acts) != 1 || acts[0].Name != "Factorio" {
		t.Errorf("a new friend: %+v", acts)
	}
	for _, typ := range []int{TypeBlocked, TypeIncomingRequest, TypeOutgoingRequest} {
		if acts := presenceActivities(t, relationshipAddPayload(ctx, u, typ)["user"]); len(acts) != 0 {
			t.Errorf("type %d: %+v", typ, acts)
		}
	}
	if acts := presenceActivities(t, partialUser(u)); len(acts) != 0 {
		t.Errorf("RELATIONSHIP_REQUEST's user carries %+v", acts)
	}
	// An invisible friend's game stays theirs.
	u.Presence.Status = "invisible"
	if acts := presenceActivities(t, relationshipAddPayload(ctx, u, TypeFriend)["user"]); len(acts) != 0 {
		t.Errorf("an invisible friend: %+v", acts)
	}
}
