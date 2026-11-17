package messages

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// A deleted account's messages stay and render as Deleted User; a PM with it refuses sends.

// deletionUsers is a user table where some ids have no row (a deleted local account) and
// some are tombstoned shadows. err fails every read.
type deletionUsers struct {
	auth.UserRepository
	rows map[int64]*auth.User
	err  error
}

func (f deletionUsers) GetByID(_ context.Context, uid int64) (*auth.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[uid], nil
}

func (f deletionUsers) GetByIDs(_ context.Context, ids []int64) ([]*auth.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*auth.User, len(ids))
	for i, uid := range ids {
		out[i] = f.rows[uid]
	}
	return out, nil
}

const (
	liveUser   = int64(10)
	goneUser   = int64(11) // no row: deleted here
	tombUser   = int64(12) // tombstoned shadow of b.test's 99
	officialID = int64(13)
)

func deletionRows() map[int64]*auth.User {
	at := time.Now().UTC()
	rid := int64(99)
	return map[int64]*auth.User{
		liveUser:   {ID: liveUser, Username: "live", DisplayName: "Live"},
		tombUser:   {ID: tombUser, HomeDomain: "b.test", RemoteID: &rid, DeletedAt: &at},
		officialID: {ID: officialID, Username: "official", System: true},
	}
}

func TestUserObjectsRenderDeletedAccounts(t *testing.T) {
	cfg := &config.Config{}
	cfg.Federation.Domain = "a.test"
	s := &Service{userRepo: deletionUsers{rows: deletionRows()}, cfg: cfg}
	got := s.userObjects(context.Background(), []int64{liveUser, goneUser, tombUser, goneUser})

	if got[liveUser]["username"] != "live" || got[liveUser]["deleted"] != nil {
		t.Errorf("live user = %v", got[liveUser])
	}
	for _, uid := range []int64{goneUser, tombUser} {
		o := got[uid]
		if o == nil || o["deleted"] != true || o["username"] != "" || o["display_name"] != "" || o["avatar"] != "" || o["id"] != id.Format(uid) {
			t.Errorf("deleted %d = %v", uid, o)
		}
	}
	if got[goneUser]["home_domain"] != "a.test" || got[goneUser]["origin_id"] != id.Format(goneUser) {
		t.Errorf("a deleted local account keeps this instance's identity: %v", got[goneUser])
	}
	if got[tombUser]["home_domain"] != "b.test" || got[tombUser]["origin_id"] != "99" {
		t.Errorf("a tombstone keeps its federated identity: %v", got[tombUser])
	}

	// A failed read is not a deletion: nothing is embedded rather than a placeholder.
	s.userRepo = deletionUsers{err: errors.New("scylla down")}
	if out := s.userObjects(context.Background(), []int64{liveUser, goneUser}); len(out) != 0 {
		t.Errorf("a failed read embedded %v", out)
	}
}

// pmRooms is a 1:1 PM between liveUser and other.
type pmRooms struct {
	*fakeRooms
	other int64
}

func (f pmRooms) GetParticipants(context.Context, int64) ([]int64, error) {
	return []int64{liveUser, f.other}, nil
}

func TestPMToDeletedAccountIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		other int64
		want  error
	}{
		{"deleted here", goneUser, ErrRecipientDeleted},
		{"tombstoned shadow", tombUser, ErrRecipientDeleted},
		// A live partner passes the deletion check and meets the next rule (one-way official PM).
		{"live partner", officialID, ErrSystemReadOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := pmRooms{fakeRooms: &fakeRooms{room: &rooms.Room{ID: 500, Type: rooms.TypePM}}, other: tc.other}
			s := &Service{userRepo: deletionUsers{rows: deletionRows()}, rooms: rs, cfg: &config.Config{}}
			_, err := s.Create(ctx, liveUser, 500, &CreateMessageInput{Ciphertext: "x"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHTTPErrorRecipientDeleted(t *testing.T) {
	status, body, ok := HTTPError(ErrRecipientDeleted)
	if !ok || status != 403 || body["code"] != "recipient_deleted" || body["error"] != ErrRecipientDeleted.Error() {
		t.Fatalf("HTTPError(ErrRecipientDeleted) = %d %v %v", status, body, ok)
	}
}

// Reactions stay after an account is deleted. The reactor list (the hover tooltip) answered
// null for a deleted reactor - GetByIDs has no row for it - and the web client's label code
// read .id off it and threw. A deleted reactor, and a tombstoned one, is the placeholder.
func TestReactorsRenderDeletedAccounts(t *testing.T) {
	rows := deletionRows()
	ids := []int64{liveUser, goneUser, tombUser}
	got := reactorUsers(ids, []*auth.User{rows[liveUser], nil, rows[tombUser]})
	if len(got) != 3 {
		t.Fatalf("reactors %v", got)
	}
	for i, u := range got {
		if u == nil || u.ID != ids[i] {
			t.Fatalf("reactor %d = %+v, want id %d", i, u, ids[i])
		}
	}
	if got[0].IsDeleted() || !got[1].IsDeleted() || !got[2].IsDeleted() {
		t.Errorf("deleted flags %v %v %v", got[0].IsDeleted(), got[1].IsDeleted(), got[2].IsDeleted())
	}
	live := userSummary(got[0])
	if live["username"] != "live" || live["deleted"] != nil {
		t.Errorf("live reactor %v", live)
	}
	for _, u := range got[1:] {
		m := userSummary(u)
		if m["deleted"] != true || m["username"] != "" || m["display_name"] != "" || m["id"] != id.Format(u.ID) {
			t.Errorf("deleted reactor %v", m)
		}
	}
}
