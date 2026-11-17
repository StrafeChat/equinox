package relationships

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/redistest"
)

const (
	daveID = 5 // local, sent alice a request
	erinID = 6 // shadow of a c.local user alice sent a request to
)

func newPurgeService(t *testing.T) (*Service, *memRepo, *memUsers, *recFed, *redistest.Redis) {
	t.Helper()
	bobRemote, erinRemote := int64(20), int64(21)
	users := &memUsers{byID: map[int64]*auth.User{
		aliceID: {ID: aliceID, Username: "alice", Relationships: []int64{carolID, bobID}},
		bobID:   {ID: bobID, Username: "bob", HomeDomain: "b.local", RemoteID: &bobRemote, Relationships: []int64{aliceID}},
		carolID: {ID: carolID, Username: "carol", Relationships: []int64{aliceID}},
		daveID:  {ID: daveID, Username: "dave"},
		erinID:  {ID: erinID, Username: "erin", HomeDomain: "c.local", RemoteID: &erinRemote},
	}}
	repo := newMemRepo()
	repo.requests[[2]int64{daveID, aliceID}] = time.Now()
	repo.requests[[2]int64{aliceID, erinID}] = time.Now()
	repo.nicknames[[2]int64{carolID, aliceID}] = "ally"
	repo.nicknames[[2]int64{carolID, daveID}] = "dd" // someone else's: stays
	fed := &recFed{}
	rdb, published := redistest.New()
	svc := NewService(repo, users, rdb, &config.Config{})
	svc.SetFederator(fed)
	return svc, repo, users, fed, published
}

func TestPurgeUserEndsEveryRelationship(t *testing.T) {
	svc, repo, users, fed, published := newPurgeService(t)
	ctx := context.Background()
	alice := *users.byID[aliceID]

	counts, err := svc.PurgeUser(ctx, &alice)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Friends != 2 || counts.Requests != 2 {
		t.Errorf("counts %+v", counts)
	}
	for _, uid := range []int64{aliceID, bobID, carolID} {
		if slices.Contains(users.byID[uid].Relationships, aliceID) || (uid == aliceID && len(users.byID[uid].Relationships) != 0) {
			t.Errorf("%d still lists the friendship: %v", uid, users.byID[uid].Relationships)
		}
	}
	if len(repo.requests) != 0 {
		t.Errorf("requests left %v", repo.requests)
	}
	if _, ok := repo.nicknames[[2]int64{carolID, aliceID}]; ok {
		t.Error("carol's nickname for alice survived")
	}
	if _, ok := repo.nicknames[[2]int64{carolID, daveID}]; !ok {
		t.Error("an unrelated nickname went")
	}
	// The remote counterparts' homes hear `remove`; local ones get the gateway event.
	wantCalls(t, fed, "remove:alice>bob", "remove:alice>erin")
	got := map[string]int{}
	for _, ev := range published.Published() {
		if ev.Type == "RELATIONSHIP_REMOVE" {
			got[ev.UserID]++
		}
	}
	for _, uid := range []int64{carolID, daveID} {
		if got[id.Format(uid)] != 1 {
			t.Errorf("RELATIONSHIP_REMOVE to %d: %d", uid, got[id.Format(uid)])
		}
	}
	if got[id.Format(bobID)] != 0 || got[id.Format(erinID)] != 0 {
		t.Errorf("a shadow was published to: %v", got)
	}
}

// On a peer, the shadow of an account its home deleted is purged the same way, but nothing
// is relayed: the home already knows.
func TestPurgeUserOfAShadowRelaysNothing(t *testing.T) {
	svc, _, users, fed, _ := newPurgeService(t)
	bob := *users.byID[bobID]
	if _, err := svc.PurgeUser(context.Background(), &bob); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(users.byID[aliceID].Relationships, bobID) {
		t.Error("alice still lists bob")
	}
	wantCalls(t, fed)
}

type failingIDs struct{ *memUsers }

func (failingIDs) GetByIDs(context.Context, []int64) ([]*auth.User, error) {
	return nil, errors.New("down")
}

func TestListRelationshipsPrunesDeletedIDs(t *testing.T) {
	svc, _, users, _, _ := newPurgeService(t)
	ctx := context.Background()
	users.byID[aliceID].Relationships = []int64{carolID, 99}
	users.byID[aliceID].Blocks = []int64{98, daveID}

	// A failed read is not a deletion: nothing is pruned.
	svc.user = failingIDs{users}
	if _, err := svc.ListRelationships(ctx, aliceID); err == nil {
		t.Fatal("expected the read error")
	}
	if !slices.Contains(users.byID[aliceID].Relationships, 99) || !slices.Contains(users.byID[aliceID].Blocks, 98) {
		t.Fatal("pruned on a failed read")
	}

	svc.user = users
	if _, err := svc.ListRelationships(ctx, aliceID); err != nil {
		t.Fatal(err)
	}
	if got := users.byID[aliceID].Relationships; !slices.Equal(got, []int64{carolID}) {
		t.Errorf("friends %v, want [carol]", got)
	}
	if got := users.byID[aliceID].Blocks; !slices.Equal(got, []int64{daveID}) {
		t.Errorf("blocks %v, want [dave]", got)
	}
}

// A peer's tombstone (its home deleted the account) still has a row, so a request by id
// found it and was stored and relayed - the home answers 404 and the request stayed pending
// on this side forever. It is refused like a missing account.
func TestSendRequestRefusesATombstone(t *testing.T) {
	svc, repo, users, fed, _ := newPurgeService(t)
	now := time.Now()
	users.byID[erinID].DeletedAt = &now
	delete(repo.requests, [2]int64{aliceID, erinID})
	if err := svc.SendRequestByID(context.Background(), carolID, erinID); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("request to a tombstone: %v", err)
	}
	if len(repo.requests) != 1 { // dave -> alice only
		t.Errorf("requests %v", repo.requests)
	}
	wantCalls(t, fed)
}
