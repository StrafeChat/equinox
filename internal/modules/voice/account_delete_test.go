package voice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// Nobody is there to call in a PM whose other account was deleted.

type deletedPartnerUsers struct {
	auth.UserRepository
	rows map[int64]*auth.User
	err  error
}

func (u deletedPartnerUsers) GetByID(_ context.Context, uid int64) (*auth.User, error) {
	return u.rows[uid], u.err
}

func TestPMPartnerDeleted(t *testing.T) {
	ctx := context.Background()
	at := time.Now()
	rid := int64(9)
	rows := map[int64]*auth.User{
		1: {ID: 1, Username: "me"},
		2: {ID: 2, Username: "friend"},
		3: {ID: 3, HomeDomain: "b.test", RemoteID: &rid, DeletedAt: &at},
	}
	s := &Service{users: deletedPartnerUsers{rows: rows}}
	pm := func(typ int, ids ...int64) *rooms.RoomWithParticipants {
		return &rooms.RoomWithParticipants{Room: rooms.Room{ID: 50, Type: typ}, ParticipantIDs: ids}
	}
	cases := []struct {
		name string
		room *rooms.RoomWithParticipants
		want bool
	}{
		{"live partner", pm(rooms.TypePM, 1, 2), false},
		{"deleted here", pm(rooms.TypePM, 1, 99), true},
		{"tombstoned shadow", pm(rooms.TypePM, 1, 3), true},
		{"a group is never refused for one member", pm(rooms.TypeGroupPM, 1, 99, 2), false},
	}
	for _, tc := range cases {
		if got := s.pmPartnerDeleted(ctx, tc.room, 1); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// A failed read is not a deletion.
	s.users = deletedPartnerUsers{err: errors.New("down")}
	if s.pmPartnerDeleted(ctx, pm(rooms.TypePM, 1, 99), 1) {
		t.Error("a failed read refused the call")
	}
}
