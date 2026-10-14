package voice

import (
	"context"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// usersByID answers GetByID from a map; nothing else in the repository is used here.
type usersByID struct {
	auth.UserRepository
	m map[int64]*auth.User
}

func (u usersByID) GetByID(_ context.Context, id int64) (*auth.User, error) {
	return u.m[id], nil
}

func TestOfficialPMHoldsNoCall(t *testing.T) {
	const me, official, friend int64 = 1, 2, 3
	s := &Service{users: usersByID{m: map[int64]*auth.User{
		me:       {ID: me},
		official: {ID: official, System: true},
		friend:   {ID: friend},
	}}}
	pm := func(typ int, ids ...int64) *rooms.RoomWithParticipants {
		r := &rooms.RoomWithParticipants{ParticipantIDs: ids}
		r.Type = typ
		return r
	}
	ctx := context.Background()
	if !s.isOfficialPM(ctx, pm(rooms.TypePM, me, official), me) {
		t.Fatal("a PM with the official account must refuse a call")
	}
	if s.isOfficialPM(ctx, pm(rooms.TypePM, me, friend), me) {
		t.Fatal("an ordinary PM must allow a call")
	}
	if s.isOfficialPM(ctx, pm(rooms.TypeGroupPM, me, friend, official), me) {
		t.Fatal("only the one-way 1:1 PM is refused")
	}
}
