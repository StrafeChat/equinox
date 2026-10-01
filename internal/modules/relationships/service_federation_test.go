package relationships

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// In-memory stand-ins for the request tables and the users table, enough to exercise the
// state machine around remote (shadow) users without Scylla.

type memRepo struct {
	requests  map[[2]int64]time.Time
	nicknames map[[2]int64]string
}

func newMemRepo() *memRepo {
	return &memRepo{requests: map[[2]int64]time.Time{}, nicknames: map[[2]int64]string{}}
}

func (r *memRepo) CreateRequest(_ context.Context, from, to int64) error {
	r.requests[[2]int64{from, to}] = time.Now()
	return nil
}

func (r *memRepo) GetIncoming(_ context.Context, to int64) ([]Request, error) {
	var out []Request
	for k, at := range r.requests {
		if k[1] == to {
			out = append(out, Request{FromUserID: k[0], ToUserID: k[1], CreatedAt: at})
		}
	}
	return out, nil
}

func (r *memRepo) GetOutgoing(_ context.Context, from int64) ([]Request, error) {
	var out []Request
	for k, at := range r.requests {
		if k[0] == from {
			out = append(out, Request{FromUserID: k[0], ToUserID: k[1], CreatedAt: at})
		}
	}
	return out, nil
}

func (r *memRepo) HasRequest(_ context.Context, from, to int64) (bool, error) {
	_, ok := r.requests[[2]int64{from, to}]
	return ok, nil
}

func (r *memRepo) DeleteRequest(_ context.Context, from, to int64) error {
	delete(r.requests, [2]int64{from, to})
	return nil
}

func (r *memRepo) SetNickname(_ context.Context, userID, targetID int64, nickname string) error {
	r.nicknames[[2]int64{userID, targetID}] = nickname
	return nil
}

func (r *memRepo) DeleteNickname(_ context.Context, userID, targetID int64) error {
	delete(r.nicknames, [2]int64{userID, targetID})
	return nil
}

func (r *memRepo) GetNicknames(_ context.Context, userID int64, targetIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for _, t := range targetIDs {
		if n, ok := r.nicknames[[2]int64{userID, t}]; ok {
			out[t] = n
		}
	}
	return out, nil
}

type memUsers struct {
	auth.UserRepository // unimplemented methods panic, which is the right test outcome
	byID map[int64]*auth.User
}

func (m *memUsers) GetByID(_ context.Context, id int64) (*auth.User, error) { return m.byID[id], nil }

func (m *memUsers) GetByIDs(_ context.Context, ids []int64) ([]*auth.User, error) {
	out := make([]*auth.User, len(ids))
	for i, id := range ids {
		out[i] = m.byID[id]
	}
	return out, nil
}

func (m *memUsers) UpdateRelationships(_ context.Context, userID int64, add, remove []int64) error {
	u := m.byID[userID]
	for _, a := range add {
		if !isFriend(u, a) {
			u.Relationships = append(u.Relationships, a)
		}
	}
	for _, r := range remove {
		keep := u.Relationships[:0]
		for _, id := range u.Relationships {
			if id != r {
				keep = append(keep, id)
			}
		}
		u.Relationships = keep
	}
	return nil
}

func (m *memUsers) UpdateBlocks(_ context.Context, userID int64, add, remove []int64) error {
	u := m.byID[userID]
	u.Blocks = append(u.Blocks, add...)
	for _, r := range remove {
		keep := u.Blocks[:0]
		for _, id := range u.Blocks {
			if id != r {
				keep = append(keep, id)
			}
		}
		u.Blocks = keep
	}
	return nil
}

type quietRedisLogger struct{}

func (quietRedisLogger) Printf(context.Context, string, ...interface{}) {}

type recFed struct{ calls []string }

func (f *recFed) rec(kind string, a, t *auth.User) {
	f.calls = append(f.calls, kind+":"+a.Username+">"+t.Username)
}
func (f *recFed) AfterRelationshipRequested(_ context.Context, a, t *auth.User) { f.rec("request", a, t) }
func (f *recFed) AfterRelationshipAccepted(_ context.Context, a, t *auth.User)  { f.rec("accept", a, t) }
func (f *recFed) AfterRelationshipRemoved(_ context.Context, a, t *auth.User)   { f.rec("remove", a, t) }

const (
	aliceID = 1 // local
	bobID   = 2 // shadow of a b.local user
	carolID = 3 // local
	botID   = 4 // local bot
)

func newTestService(t *testing.T) (*Service, *memRepo, *memUsers, *recFed) {
	t.Helper()
	remote := int64(20)
	users := &memUsers{byID: map[int64]*auth.User{
		aliceID: {ID: aliceID, Username: "alice"},
		bobID:   {ID: bobID, Username: "bob", HomeDomain: "b.local", RemoteID: &remote},
		carolID: {ID: carolID, Username: "carol"},
		botID:   {ID: botID, Username: "bot", Bot: true},
	}}
	repo := newMemRepo()
	fed := &recFed{}
	// Gateway publishes go to Redis unconditionally; an unroutable client makes them fail
	// fast instead of panicking on nil, and the client's own logger is silenced so the
	// expected dial failures don't drown the test output.
	redis.SetLogger(quietRedisLogger{})
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 5 * time.Millisecond, MaxRetries: -1, PoolSize: 1})
	svc := NewService(repo, users, rdb, &config.Config{})
	svc.SetFederator(fed)
	return svc, repo, users, fed
}

func wantCalls(t *testing.T, fed *recFed, want ...string) {
	t.Helper()
	if len(fed.calls) != len(want) {
		t.Fatalf("federator calls = %v, want %v", fed.calls, want)
	}
	for i := range want {
		if fed.calls[i] != want[i] {
			t.Fatalf("federator calls = %v, want %v", fed.calls, want)
		}
	}
}

func TestSendRequestToRemoteRelays(t *testing.T) {
	svc, repo, users, fed := newTestService(t)
	ctx := context.Background()
	if err := svc.SendRequestTo(ctx, aliceID, users.byID[bobID]); err != nil {
		t.Fatal(err)
	}
	if ok, _ := repo.HasRequest(ctx, aliceID, bobID); !ok {
		t.Fatal("request not stored")
	}
	wantCalls(t, fed, "request:alice>bob")
	if err := svc.SendRequestTo(ctx, aliceID, users.byID[bobID]); err != ErrRequestExists {
		t.Fatalf("second send = %v, want ErrRequestExists", err)
	}
}

func TestSendRequestToLocalDoesNotRelay(t *testing.T) {
	svc, _, users, fed := newTestService(t)
	if err := svc.SendRequestTo(context.Background(), aliceID, users.byID[carolID]); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, fed)
}

func TestApplyRemoteAcceptBefriendsWithoutRelaying(t *testing.T) {
	svc, repo, users, fed := newTestService(t)
	ctx := context.Background()
	_ = repo.CreateRequest(ctx, aliceID, bobID)
	if err := svc.ApplyRemoteAccept(ctx, users.byID[bobID], users.byID[aliceID]); err != nil {
		t.Fatal(err)
	}
	if !isFriend(users.byID[aliceID], bobID) || !isFriend(users.byID[bobID], aliceID) {
		t.Fatal("not friends on both rows")
	}
	if ok, _ := repo.HasRequest(ctx, aliceID, bobID); ok {
		t.Fatal("request still pending")
	}
	wantCalls(t, fed)
}

func TestApplyRemoteAcceptWithoutRequest(t *testing.T) {
	svc, _, users, _ := newTestService(t)
	if err := svc.ApplyRemoteAccept(context.Background(), users.byID[bobID], users.byID[aliceID]); err != ErrRequestNotFound {
		t.Fatalf("err = %v, want ErrRequestNotFound", err)
	}
}

func TestApplyRemoteRequestCrossedAutoAcceptsAndRelays(t *testing.T) {
	svc, repo, users, fed := newTestService(t)
	ctx := context.Background()
	_ = repo.CreateRequest(ctx, aliceID, bobID)
	if err := svc.ApplyRemoteRequest(ctx, users.byID[bobID], users.byID[aliceID]); err != nil {
		t.Fatal(err)
	}
	if !isFriend(users.byID[aliceID], bobID) || !isFriend(users.byID[bobID], aliceID) {
		t.Fatal("crossed requests should become a friendship")
	}
	wantCalls(t, fed, "accept:alice>bob")
}

func TestApplyRemoteRequestStoresAndIsIdempotent(t *testing.T) {
	svc, repo, users, fed := newTestService(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := svc.ApplyRemoteRequest(ctx, users.byID[bobID], users.byID[aliceID]); err != nil {
			t.Fatal(err)
		}
	}
	if in, _ := repo.GetIncoming(ctx, aliceID); len(in) != 1 || in[0].FromUserID != bobID {
		t.Fatalf("incoming = %+v, want one from bob", in)
	}
	wantCalls(t, fed)
}

func TestApplyRemoteRequestFromBlockedSenderIsSilent(t *testing.T) {
	svc, repo, users, _ := newTestService(t)
	ctx := context.Background()
	users.byID[aliceID].Blocks = []int64{bobID}
	if err := svc.ApplyRemoteRequest(ctx, users.byID[bobID], users.byID[aliceID]); err != nil {
		t.Fatalf("blocked request should be dropped silently, got %v", err)
	}
	if ok, _ := repo.HasRequest(ctx, bobID, aliceID); ok {
		t.Fatal("request from a blocked sender was stored")
	}
}

func TestApplyRemoteRequestToBot(t *testing.T) {
	svc, _, users, _ := newTestService(t)
	if err := svc.ApplyRemoteRequest(context.Background(), users.byID[bobID], users.byID[botID]); err != ErrBotTarget {
		t.Fatalf("err = %v, want ErrBotTarget", err)
	}
}

func TestRemoveFriendRemoteRelays(t *testing.T) {
	svc, _, users, fed := newTestService(t)
	users.byID[aliceID].Relationships = []int64{bobID}
	users.byID[bobID].Relationships = []int64{aliceID}
	if err := svc.RemoveFriend(context.Background(), aliceID, bobID); err != nil {
		t.Fatal(err)
	}
	if len(users.byID[aliceID].Relationships) != 0 || len(users.byID[bobID].Relationships) != 0 {
		t.Fatal("friendship not removed from both rows")
	}
	wantCalls(t, fed, "remove:alice>bob")
}

func TestRejectAndCancelRemoteRelay(t *testing.T) {
	svc, repo, _, fed := newTestService(t)
	ctx := context.Background()
	_ = repo.CreateRequest(ctx, bobID, aliceID)
	if err := svc.RejectRequest(ctx, aliceID, bobID); err != nil {
		t.Fatal(err)
	}
	_ = repo.CreateRequest(ctx, aliceID, bobID)
	if err := svc.CancelRequest(ctx, aliceID, bobID); err != nil {
		t.Fatal(err)
	}
	if len(repo.requests) != 0 {
		t.Fatalf("requests left: %v", repo.requests)
	}
	wantCalls(t, fed, "remove:alice>bob", "remove:alice>bob")
}

func TestBlockRemoteWithPendingRequestRelaysRemoveOnce(t *testing.T) {
	svc, repo, users, fed := newTestService(t)
	ctx := context.Background()
	users.byID[aliceID].Relationships = []int64{bobID}
	users.byID[bobID].Relationships = []int64{aliceID}
	_ = repo.CreateRequest(ctx, bobID, aliceID)
	if err := svc.Block(ctx, aliceID, bobID); err != nil {
		t.Fatal(err)
	}
	if len(repo.requests) != 0 || len(users.byID[aliceID].Relationships) != 0 {
		t.Fatal("block should tear down the friendship and the request")
	}
	if users.byID[aliceID].Blocks[0] != bobID {
		t.Fatal("block not recorded")
	}
	wantCalls(t, fed, "remove:alice>bob")
}

func TestBlockRemoteWithNothingBetweenDoesNotRelay(t *testing.T) {
	svc, _, _, fed := newTestService(t)
	if err := svc.Block(context.Background(), aliceID, bobID); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, fed)
}

func TestApplyRemoteRemoveTearsDownWhateverStands(t *testing.T) {
	svc, repo, users, fed := newTestService(t)
	ctx := context.Background()
	if err := svc.ApplyRemoteRemove(ctx, users.byID[bobID], users.byID[aliceID]); err != nil {
		t.Fatalf("nothing to remove should be fine, got %v", err)
	}
	_ = repo.CreateRequest(ctx, aliceID, bobID)
	if err := svc.ApplyRemoteRemove(ctx, users.byID[bobID], users.byID[aliceID]); err != nil {
		t.Fatal(err)
	}
	if len(repo.requests) != 0 {
		t.Fatal("outgoing request should be withdrawn")
	}
	users.byID[aliceID].Relationships = []int64{bobID}
	users.byID[bobID].Relationships = []int64{aliceID}
	if err := svc.ApplyRemoteRemove(ctx, users.byID[bobID], users.byID[aliceID]); err != nil {
		t.Fatal(err)
	}
	if len(users.byID[aliceID].Relationships) != 0 {
		t.Fatal("friendship should be ended")
	}
	wantCalls(t, fed)
}
