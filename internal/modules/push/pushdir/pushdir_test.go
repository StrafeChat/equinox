package pushdir

import (
	"context"
	"errors"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/push"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// memberRepo answers GetMember from a map; every other spaces.Repository method is left to
// the nil embedded interface, so calling one panics (MemberProfile must not need any).
type memberRepo struct {
	spaces.Repository
	members map[[2]int64]*spaces.SpaceMember
	err     error
}

func (r *memberRepo) GetMember(_ context.Context, spaceID, userID int64) (*spaces.SpaceMember, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.members[[2]int64{spaceID, userID}], nil
}

func TestMemberProfile(t *testing.T) {
	ctx := context.Background()
	repo := &memberRepo{members: map[[2]int64]*spaces.SpaceMember{
		{1, 2}: {SpaceID: 1, UserID: 2, Nickname: "  Turtle \t", Avatar: "https://cdn.example/v1/spaces/members/1/2/avatar-0123456789abcdef.png",
			Banner: "https://cdn.example/b.png", AboutMe: "hi", Bio: "bio", Pronouns: "they/them"},
		{1, 3}: {SpaceID: 1, UserID: 3, Nickname: ""}, // the "" old rows hold for no nickname
		{1, 4}: {SpaceID: 1, UserID: 4, Nickname: "   "},
	}}
	d := New(nil, nil, nil, repo, nil)

	cases := []struct {
		name        string
		space, user int64
		want        *push.MemberProfile
	}{
		{"nick trimmed, avatar as stored", 1, 2, &push.MemberProfile{Nick: "Turtle", Avatar: "https://cdn.example/v1/spaces/members/1/2/avatar-0123456789abcdef.png"}},
		{"no nickname", 1, 3, &push.MemberProfile{}},
		{"blank nickname", 1, 4, &push.MemberProfile{}},
		{"not a member", 1, 5, nil},
		{"member of another space", 9, 2, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.MemberProfile(ctx, tc.space, tc.user)
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("MemberProfile(%d, %d) = %+v, want %+v", tc.space, tc.user, got, tc.want)
			}
		})
	}

	repo.err = errors.New("scylla down")
	if got, err := d.MemberProfile(ctx, 1, 2); err == nil || got != nil {
		t.Errorf("a failing read: %+v, %v", got, err)
	}
}
