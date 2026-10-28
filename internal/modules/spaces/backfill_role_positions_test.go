package spaces

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
)

func TestBackfillRolePositions(t *testing.T) {
	ctx := context.Background()
	setup := func() (*orderRepo, *Service, *recordingFed) {
		r := roleFixtureRepo()
		// Space 500 (hosted here): 60 tied with mod at 2, and 61 at 0 beside @everyone.
		r.addRole(rfSpace, SpaceRole{ID: 60, Name: "tied", Position: 2})
		r.addRole(rfSpace, SpaceRole{ID: 61, Name: "floor", Position: 0})
		// 600: a mirror, tied too - its origin normalizes it and relays the result.
		// 700: role rows with no space row (left behind by a deletion).
		// 800: a space from before everyone_role_id, whose @everyone cannot be told apart.
		// 900: already in order.
		r.spaces[600] = &Space{ID: 600, OwnerID: 1, EveryoneRoleID: 601}
		r.spaces[800] = &Space{ID: 800, OwnerID: 1}
		r.spaces[900] = &Space{ID: 900, OwnerID: 1, EveryoneRoleID: 901}
		r.roles[600] = map[int64]SpaceRole{601: {ID: 601, Name: EveryoneRoleName}, 602: {ID: 602, Position: 1}, 603: {ID: 603, Position: 1}}
		r.roles[700] = map[int64]SpaceRole{702: {ID: 702, Position: 1}, 703: {ID: 703, Position: 1}}
		r.roles[800] = map[int64]SpaceRole{801: {ID: 801, Name: EveryoneRoleName}, 802: {ID: 802, Position: 0}}
		r.roles[900] = map[int64]SpaceRole{901: {ID: 901, Name: EveryoneRoleName}, 902: {ID: 902, Position: 1}, 903: {ID: 903, Position: 4}}
		s, fed := roleService(r)
		fed.mirrors[600] = "b.example"
		return r, s, fed
	}

	t.Run("normalizes the spaces hosted here, once", func(t *testing.T) {
		r, s, fed := setup()
		if err := s.BackfillRolePositions(ctx); err != nil {
			t.Fatal(err)
		}
		// 61 (0) -> 1, member 1 -> 2, then the tie at 2: 60 is newer, so it goes below mod.
		want := map[int64]int{rfEveryone: 0, 61: 1, rfMember: 2, 60: 3, rfMod: 4, rfAdmin: 5}
		if got := r.positions(rfSpace); !reflect.DeepEqual(got, want) {
			t.Errorf("space 500: %v, want %v", got, want)
		}
		assertUniquePositions(t, r, rfSpace)
		for sid, want := range map[int64]map[int64]int{
			600: {601: 0, 602: 1, 603: 1},
			700: {702: 1, 703: 1},
			800: {801: 0, 802: 0},
			900: {901: 0, 902: 1, 903: 4},
		} {
			if got := r.positions(sid); !reflect.DeepEqual(got, want) {
				t.Errorf("space %d touched: %v", sid, got)
			}
		}
		relayed := fed.relayed()
		sort.Slice(relayed, func(i, j int) bool { return relayed[i] < relayed[j] })
		if want := []int64{rfMember, rfMod, rfAdmin, 60, 61}; !reflect.DeepEqual(relayed, want) {
			t.Errorf("relayed %v, want %v", relayed, want)
		}
		if len(r.audits) != 0 {
			t.Errorf("a data fix is not a moderator's action: %+v", r.audits)
		}
		writes := len(r.roleUpdates)
		if err := s.BackfillRolePositions(ctx); err != nil || len(r.roleUpdates) != writes {
			t.Errorf("second run: err %v, %d more writes", err, len(r.roleUpdates)-writes)
		}
	})

	t.Run("a failure hands the claim back", func(t *testing.T) {
		r, s, _ := setup()
		r.failList = rfSpace
		if err := s.BackfillRolePositions(ctx); !errors.Is(err, errListFailed) {
			t.Fatalf("got %v", err)
		}
		if r.claimed[dataMigrationRolePositions] {
			t.Fatal("the claim was kept after a failure")
		}
		r.failList = 0
		if err := s.BackfillRolePositions(ctx); err != nil {
			t.Fatal(err)
		}
		assertUniquePositions(t, r, rfSpace)
	})
}
