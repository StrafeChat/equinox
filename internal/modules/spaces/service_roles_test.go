package spaces

import (
	"errors"
	"testing"
)

// roleFixture: @everyone at 0, then member(1) < mod(2) < admin(3).
func roleFixture() (everyoneID int64, byID map[int64]*SpaceRole) {
	everyoneID = 10
	byID = map[int64]*SpaceRole{
		10: {ID: 10, Name: EveryoneRoleName, Position: 0},
		11: {ID: 11, Name: "member", Position: 1},
		12: {ID: 12, Name: "mod", Position: 2},
		13: {ID: 13, Name: "admin", Position: 3},
	}
	return
}

func TestResolveMemberRoles(t *testing.T) {
	everyoneID, byID := roleFixture()

	cases := []struct {
		name         string
		requested    []int64
		current      []int64
		isOwner      bool
		actorHighest int
		want         []int64
		wantErr      error
	}{
		{
			// The bug this fixes: the owner giving themselves a role.
			name:      "owner grants themselves the top role",
			requested: []int64{13},
			current:   []int64{everyoneID},
			isOwner:   true,
			want:      []int64{everyoneID, 13},
		},
		{
			name:         "actor grants a role below their own rank",
			requested:    []int64{11},
			current:      []int64{everyoneID, 12},
			actorHighest: 2,
			// mod (2) is at the actor's own rank: carried over, not dropped.
			want: []int64{everyoneID, 12, 11},
		},
		{
			name:         "actor cannot grant a role at their own rank",
			requested:    []int64{12},
			current:      []int64{everyoneID},
			actorHighest: 2,
			wantErr:      ErrRoleHierarchy,
		},
		{
			name:         "actor cannot grant a role above their own rank",
			requested:    []int64{13},
			current:      []int64{everyoneID},
			actorHighest: 2,
			wantErr:      ErrRoleHierarchy,
		},
		{
			// Omitting a role you may not touch must not strip it.
			name:         "roles above the actor survive a request that omits them",
			requested:    []int64{11},
			current:      []int64{everyoneID, 13},
			actorHighest: 2,
			want:         []int64{everyoneID, 13, 11},
		},
		{
			// Same role in both lists: kept once, and not an escalation error.
			name:         "re-requesting a role the member already holds above the actor is a no-op",
			requested:    []int64{13, 11},
			current:      []int64{everyoneID, 13},
			actorHighest: 2,
			want:         []int64{everyoneID, 13, 11},
		},
		{
			name:         "owner has no ceiling and no carry-over",
			requested:    []int64{11},
			current:      []int64{everyoneID, 13},
			isOwner:      true,
			actorHighest: -1,
			want:         []int64{everyoneID, 11},
		},
		{
			name:         "removing every custom role leaves @everyone",
			requested:    nil,
			current:      []int64{everyoneID, 11},
			actorHighest: 2,
			want:         []int64{everyoneID},
		},
		{
			name:         "@everyone in the request is ignored, never duplicated",
			requested:    []int64{everyoneID, 11},
			current:      []int64{everyoneID},
			actorHighest: 2,
			want:         []int64{everyoneID, 11},
		},
		{
			name:         "duplicates in the request collapse",
			requested:    []int64{11, 11},
			current:      []int64{everyoneID},
			actorHighest: 2,
			want:         []int64{everyoneID, 11},
		},
		{
			name:         "unknown role id",
			requested:    []int64{99},
			current:      []int64{everyoneID},
			actorHighest: 2,
			wantErr:      ErrRoleNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMemberRoles(tc.requested, tc.current, everyoneID, byID, tc.isOwner, tc.actorHighest)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want error %v, got %v (roles %v)", tc.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("want %v, got %v", tc.want, got)
				}
			}
			if got[0] != everyoneID {
				t.Fatalf("@everyone must come first, got %v", got)
			}
		})
	}
}
