package spaces

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

// ---- placeRoles ----------------------------------------------------------------------------

func TestPlaceRoles(t *testing.T) {
	const ev = 1
	everyone := SpaceRole{ID: ev, Name: EveryoneRoleName, Position: 0}
	r := func(id int64, pos int) SpaceRole { return SpaceRole{ID: id, Position: pos} }
	const a, b, c, d, n = 10, 11, 12, 13, 99 // n: a role being created (the caller adds it at 1)

	cases := []struct {
		name  string
		roles []SpaceRole
		moves map[int64]int
		want  map[int64]int // only the roles that move
	}{
		{
			name:  "create at 1 pushes up the contiguous block above it",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 3), r(n, 1)},
			moves: map[int64]int{n: 1},
			want:  map[int64]int{a: 2, b: 3, c: 4},
		},
		{
			name:  "a gap absorbs the cascade",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 5), r(n, 1)},
			moves: map[int64]int{n: 1},
			want:  map[int64]int{a: 2, b: 3},
		},
		{
			name:  "a free position 1 moves nothing",
			roles: []SpaceRole{everyone, r(a, 2), r(b, 3), r(n, 1)},
			moves: map[int64]int{n: 1},
			want:  map[int64]int{},
		},
		{
			name:  "legacy tie: the newer role (higher id) goes below",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 1), r(c, 2)},
			want:  map[int64]int{a: 2, c: 3},
		},
		{
			name:  "three-way tie, newest lowest",
			roles: []SpaceRole{everyone, r(a, 2), r(b, 2), r(c, 2)},
			want:  map[int64]int{b: 3, a: 4},
		},
		{
			name:  "a custom role at 0 is lifted above @everyone",
			roles: []SpaceRole{everyone, r(d, 0), r(a, 1)},
			want:  map[int64]int{d: 1, a: 2},
		},
		{
			name:  "a moved role takes an occupied slot and the occupant goes above it",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 3)},
			moves: map[int64]int{c: 1},
			want:  map[int64]int{c: 1, a: 2, b: 3},
		},
		{
			name:  "moving up into an occupied slot",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 3)},
			moves: map[int64]int{a: 3},
			want:  map[int64]int{a: 3, c: 4},
		},
		{
			name:  "a move into a free slot touches nothing else",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 5)},
			moves: map[int64]int{a: 4},
			want:  map[int64]int{a: 4},
		},
		{
			name:  "positions above a move keep their gaps",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 5), r(c, 9)},
			moves: map[int64]int{b: 2},
			want:  map[int64]int{b: 2},
		},
		{
			name:  "several moves at once: a swap",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 3), r(d, 4)},
			moves: map[int64]int{a: 4, d: 1},
			want:  map[int64]int{a: 4, d: 1},
		},
		{
			name:  "slot reuse, as clients send it, writes exactly the moved roles",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 3), r(d, 4)},
			moves: map[int64]int{b: 3, c: 2},
			want:  map[int64]int{b: 3, c: 2},
		},
		{
			name:  "two moves into one slot: both take it, the newer lower, the occupant above",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 3)},
			moves: map[int64]int{a: 3, b: 3},
			want:  map[int64]int{b: 3, a: 4, c: 5},
		},
		{
			name:  "a move settles a tie it does not touch",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 3), r(c, 3)},
			moves: map[int64]int{a: 2},
			want:  map[int64]int{a: 2, b: 4},
		},
		{
			name:  "@everyone never moves, even when asked",
			roles: []SpaceRole{everyone, r(a, 1)},
			moves: map[int64]int{ev: 5},
			want:  map[int64]int{},
		},
		{
			name:  "a move for a role that is not there is ignored",
			roles: []SpaceRole{everyone, r(a, 1)},
			moves: map[int64]int{77: 3},
			want:  map[int64]int{},
		},
		{
			name:  "unique positions and no moves: nothing to write",
			roles: []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 7)},
			want:  map[int64]int{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := placeRoles(tc.roles, ev, tc.moves)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("placeRoles = %v, want %v", got, tc.want)
			}
			final := finalPositions(tc.roles, got)
			assertDistinct(t, final, ev)
		})
	}
}

// finalPositions applies placeRoles' answer to roles.
func finalPositions(roles []SpaceRole, placed map[int64]int) map[int64]int {
	out := make(map[int64]int, len(roles))
	for _, r := range roles {
		out[r.ID] = r.Position
		if p, ok := placed[r.ID]; ok {
			out[r.ID] = p
		}
	}
	return out
}

func assertDistinct(t *testing.T, final map[int64]int, everyoneID int64) {
	t.Helper()
	seen := map[int]int64{}
	for id, p := range final {
		if id == everyoneID {
			if p != 0 {
				t.Errorf("@everyone at %d", p)
			}
			continue
		}
		if p < 1 {
			t.Errorf("role %d at %d", id, p)
		}
		if other, dup := seen[p]; dup {
			t.Errorf("roles %d and %d both at %d", other, id, p)
		}
		seen[p] = id
	}
}

// lowerFirst orders roles as the hierarchy reads ties: by position, and at one position the
// newer (higher id) role lower.
func lowerFirst(roles []SpaceRole) []SpaceRole {
	out := append([]SpaceRole(nil), roles...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// randomRoles is @everyone (id 1) plus n custom roles (ids from 100), at distinct positions
// from 1 to 3n, or - with ties - at positions from 0 to n+1 that may repeat.
func randomRoles(rng *rand.Rand, n int, ties bool) []SpaceRole {
	roles := []SpaceRole{{ID: 1, Name: EveryoneRoleName}}
	used := map[int]bool{}
	for i := 0; i < n; i++ {
		var p int
		if ties {
			p = rng.IntN(n + 2)
		} else {
			for p = 1 + rng.IntN(3*n); used[p]; p = 1 + rng.IntN(3*n) {
			}
			used[p] = true
		}
		roles = append(roles, SpaceRole{ID: int64(100 + i), Position: p})
	}
	rng.Shuffle(len(roles), func(i, j int) { roles[i], roles[j] = roles[j], roles[i] })
	return roles
}

func customOnly(roles []SpaceRole) []SpaceRole {
	var out []SpaceRole
	for _, r := range roles {
		if r.ID != 1 {
			out = append(out, r)
		}
	}
	return out
}

func TestPlaceRolesInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(51, 1))
	for iter := 0; iter < 3000; iter++ {
		n := 1 + rng.IntN(12)
		roles := randomRoles(rng, n, iter%2 == 0)
		moves := map[int64]int{}
		for _, r := range customOnly(roles) {
			if rng.IntN(3) == 0 {
				moves[r.ID] = 1 + rng.IntN(3*n)
			}
		}
		placed := placeRoles(roles, 1, moves)
		final := finalPositions(roles, placed)
		assertDistinct(t, final, 1)

		var unmoved []SpaceRole
		for _, r := range customOnly(roles) {
			key := r.Position
			if p, ok := moves[r.ID]; ok {
				key = p
			} else {
				unmoved = append(unmoved, r)
			}
			// Nothing ends below the position it holds or asked for.
			if final[r.ID] < key {
				t.Fatalf("role %d placed at %d below its key %d (roles %v, moves %v)", r.ID, final[r.ID], key, roles, moves)
			}
			if _, ok := placed[r.ID]; ok && final[r.ID] == r.Position {
				t.Fatalf("role %d reported as moved but stayed at %d", r.ID, r.Position)
			}
		}
		// The roles nobody moved keep their order.
		ordered := lowerFirst(unmoved)
		for i := 1; i < len(ordered); i++ {
			if final[ordered[i-1].ID] >= final[ordered[i].ID] {
				t.Fatalf("unmoved roles %d and %d swapped (roles %v, moves %v, placed %v)", ordered[i-1].ID, ordered[i].ID, roles, moves, placed)
			}
		}
		// Normalized positions place to themselves.
		after := append([]SpaceRole(nil), roles...)
		for i := range after {
			after[i].Position = final[after[i].ID]
		}
		if again := placeRoles(after, 1, nil); len(again) != 0 {
			t.Fatalf("not a fixed point: %v moved again from %v", again, final)
		}
	}
}

// TestPlaceRolesNonOwnerInvariants: a move a non-owner may make (every listed role below the
// actor's highest role R, every requested position below R's) never lifts a role to or past R,
// never brings one down from above it, and keeps the roles at and above R in their order. A
// slot-reusing move - what clients send - writes nothing but the moved roles.
func TestPlaceRolesNonOwnerInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(51, 2))
	for iter := 0; iter < 3000; iter++ {
		n := 2 + rng.IntN(12)
		roles := randomRoles(rng, n, false)
		custom := lowerFirst(customOnly(roles))
		actor := custom[1+rng.IntN(len(custom)-1)] // not the lowest, so something is below it
		h := actor.Position
		var below []SpaceRole
		for _, r := range custom {
			if r.Position < h {
				below = append(below, r)
			}
		}

		moves := map[int64]int{}
		for _, r := range below {
			if rng.IntN(2) == 0 {
				moves[r.ID] = 1 + rng.IntN(h-1)
			}
		}
		final := finalPositions(roles, placeRoles(roles, 1, moves))
		assertDistinct(t, final, 1)
		for _, r := range custom {
			wasBelow := r.Position < h
			isBelow := final[r.ID] < final[actor.ID]
			if r.ID != actor.ID && wasBelow != isBelow {
				t.Fatalf("role %d crossed the actor's role (%d -> %d, actor %d -> %d; moves %v)", r.ID, r.Position, final[r.ID], h, final[actor.ID], moves)
			}
		}
		var above []SpaceRole
		for _, r := range custom {
			if r.Position >= h {
				above = append(above, r)
			}
		}
		for i := 1; i < len(above); i++ {
			if final[above[i-1].ID] >= final[above[i].ID] {
				t.Fatalf("roles at or above the actor reordered: %d, %d", above[i-1].ID, above[i].ID)
			}
		}

		// Slot reuse: the roles below the actor trade positions among themselves.
		perm := rng.Perm(len(below))
		reuse := map[int64]int{}
		for i, j := range perm {
			reuse[below[i].ID] = below[j].Position
		}
		placed := placeRoles(roles, 1, reuse)
		for id, p := range placed {
			if want, ok := reuse[id]; !ok || p != want {
				t.Fatalf("slot reuse moved role %d to %d (asked %v, %v)", id, p, want, ok)
			}
		}
		for id, want := range reuse {
			if got := finalPositions(roles, placed)[id]; got != want {
				t.Fatalf("slot reuse: role %d at %d, asked %d", id, got, want)
			}
		}
	}
}

// ---- the writer ----------------------------------------------------------------------------

// TestApplyRoleOrderWriteOrder replays the writes applyRoleOrder makes, one at a time, and
// checks that no moment between them puts one role strictly below another unless the order
// before or the order after does too: a permission check running meanwhile can only ever see
// a refusal neither state would give, never a grant.
func TestApplyRoleOrderWriteOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(51, 3))
	ctx := context.Background()
	for iter := 0; iter < 1500; iter++ {
		n := 2 + rng.IntN(10)
		roles := randomRoles(rng, n, iter%3 == 0)
		moves := map[int64]int{}
		for _, r := range customOnly(roles) {
			if rng.IntN(2) == 0 {
				moves[r.ID] = 1 + rng.IntN(3*n)
			}
		}
		before := map[int64]int{}
		for _, r := range roles {
			before[r.ID] = r.Position
		}
		repo := &orderRepo{
			spaces: map[int64]*Space{9: {ID: 9, OwnerID: 1, EveryoneRoleID: 1}},
			roles:  map[int64]map[int64]SpaceRole{9: {}},
		}
		for i := range roles {
			roles[i].SpaceID = 9
			repo.roles[9][roles[i].ID] = roles[i]
		}
		s := NewService(repo, nil, nil, nil, nil)
		placed := placeRoles(roles, 1, moves)
		if _, err := s.applyRoleOrder(ctx, repo.spaces[9], 1, roles, placed, 0, false, true); err != nil {
			t.Fatal(err)
		}
		after := repo.positions(9)
		if len(repo.roleUpdates) != len(placed) {
			t.Fatalf("%d writes for %d moved roles", len(repo.roleUpdates), len(placed))
		}
		cur := map[int64]int{}
		for id, p := range before {
			cur[id] = p
		}
		for step, id := range repo.roleUpdates {
			cur[id] = after[id]
			for x := range cur {
				for y := range cur {
					if x == y || x == 1 || y == 1 || cur[x] >= cur[y] {
						continue
					}
					if before[x] >= before[y] && after[x] >= after[y] {
						t.Fatalf("after write %d (role %d), role %d is below %d (%d < %d) though it is not before (%d, %d) or after (%d, %d)",
							step, id, x, y, cur[x], cur[y], before[x], before[y], after[x], after[y])
					}
				}
			}
		}
	}
}

func TestApplyRoleOrderCreate(t *testing.T) {
	r := roleFixtureRepo()
	s, fed := roleService(r)
	ctx := context.Background()
	roles, _ := r.ListSpaceRoles(ctx, rfSpace)
	fresh := SpaceRole{SpaceID: rfSpace, ID: 900, Name: "new", Position: 1}
	roles = append(roles, fresh)
	placed := placeRoles(roles, rfEveryone, map[int64]int{900: 1})
	all, err := s.applyRoleOrder(ctx, r.spaces[rfSpace], rfEveryone, roles, placed, 900, true, true)
	if err != nil {
		t.Fatal(err)
	}
	// The run above 1 moves up from the top down, then the new role goes in: never a tie.
	if want := []int64{rfAdmin, rfMod, rfMember}; !reflect.DeepEqual(r.roleUpdates, want) {
		t.Errorf("updates %v, want %v", r.roleUpdates, want)
	}
	if !reflect.DeepEqual(r.roleInserts, []int64{900}) {
		t.Errorf("inserts %v", r.roleInserts)
	}
	if want := []int64{rfAdmin, rfMod, rfMember, 900}; !reflect.DeepEqual(fed.relayed(), want) {
		t.Errorf("relayed %v, want %v", fed.relayed(), want)
	}
	assertUniquePositions(t, r, rfSpace)
	if got := r.positions(rfSpace); got[900] != 1 || got[rfMember] != 2 || got[rfMod] != 3 || got[rfAdmin] != 4 {
		t.Errorf("positions %v", got)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Position >= all[i].Position {
			t.Fatalf("returned roles not by position: %v", all)
		}
	}
}

// ---- the role lock -------------------------------------------------------------------------

// TestRoleWritesSerialized races creates, reorders and edits on one space through a
// repository that yields between every read and write. Without the per-space lock two of them
// read the same positions and leave roles tied.
func TestRoleWritesSerialized(t *testing.T) {
	r := roleFixtureRepo()
	r.yield = true
	s, _ := roleService(r)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := s.CreateSpaceRole(ctx, rfOwnerID, rfSpace, &CreateSpaceRoleInput{Name: "made"}); err != nil {
				errs <- err
			}
		}()
		go func(i int) {
			defer wg.Done()
			moves := []RolePosition{{RoleID: rfMember, Position: 1 + i%4}, {RoleID: rfAdmin, Position: 2 + i%3}}
			if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, moves); err != nil {
				errs <- err
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			p := 1 + i%5
			if _, err := s.UpdateSpaceRole(ctx, rfOwnerID, rfSpace, rfMod, &UpdateSpaceRoleInput{Position: &p}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assertUniquePositions(t, r, rfSpace)
	if n := len(r.positions(rfSpace)); n != 4+16 {
		t.Errorf("%d roles, want %d", n, 4+16)
	}
	roleLocks.mu.Lock()
	defer roleLocks.mu.Unlock()
	if _, left := roleLocks.locks[rfSpace]; left {
		t.Error("the space's lock entry outlived its last user")
	}
}

func TestRoleLockReleaseIsIdempotent(t *testing.T) {
	unlock := lockRoles(4242)
	unlock()
	unlock()
	again := lockRoles(4242) // would deadlock if the first lock were still held
	again()
	roleLocks.mu.Lock()
	defer roleLocks.mu.Unlock()
	if _, left := roleLocks.locks[4242]; left {
		t.Error("lock entry left behind")
	}
}

// ---- the services --------------------------------------------------------------------------

func TestCreateSpaceRolePosition(t *testing.T) {
	ctx := context.Background()

	t.Run("goes in at 1, pushing the block above it up", func(t *testing.T) {
		r := roleFixtureRepo()
		s, fed := roleService(r)
		role, err := s.CreateSpaceRole(ctx, rfOwnerID, rfSpace, &CreateSpaceRoleInput{Name: "Verified"})
		if err != nil {
			t.Fatal(err)
		}
		if role.Position != 1 {
			t.Errorf("created at %d, want 1", role.Position)
		}
		got := r.positions(rfSpace)
		if got[rfMember] != 2 || got[rfMod] != 3 || got[rfAdmin] != 4 || got[rfEveryone] != 0 {
			t.Errorf("positions %v", got)
		}
		assertUniquePositions(t, r, rfSpace)
		if len(fed.relayed()) != 4 {
			t.Errorf("relayed %v: the three moved roles and the new one", fed.relayed())
		}
		if len(r.audits) != 1 || r.audits[0].ActionType != AuditRoleCreate {
			t.Errorf("audits %+v", r.audits)
		}
	})

	t.Run("a moderator can grant the role they just made", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		role, err := s.CreateSpaceRole(ctx, rfModID, rfSpace, &CreateSpaceRoleInput{Name: "Helper"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddMemberRole(ctx, rfModID, rfSpace, rfPlainID, role.ID); err != nil {
			t.Fatalf("grant the new role: %v", err)
		}
		if err := s.SetMemberRoles(ctx, rfModID, rfSpace, rfMemberID, []int64{rfMember, role.ID}); err != nil {
			t.Fatalf("grant through the full list: %v", err)
		}
	})

	t.Run("Manage Roles without a role of your own", func(t *testing.T) {
		r := roleFixtureRepo()
		r.mu.Lock()
		ev := r.roles[rfSpace][rfEveryone]
		ev.Permissions |= permissions.PermManageRoles
		r.roles[rfSpace][rfEveryone] = ev
		r.mu.Unlock()
		s, _ := roleService(r)
		if _, err := s.CreateSpaceRole(ctx, rfPlainID, rfSpace, &CreateSpaceRoleInput{Name: "x"}); !errors.Is(err, ErrRoleHierarchy) {
			t.Errorf("got %v, want %v (the role would sit above its creator)", err, ErrRoleHierarchy)
		}
		if len(r.roleInserts) != 0 {
			t.Error("a role was written")
		}
	})

	t.Run("without Manage Roles", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		if _, err := s.CreateSpaceRole(ctx, rfMemberID, rfSpace, &CreateSpaceRoleInput{Name: "x"}); !errors.Is(err, ErrMissingPerm) {
			t.Errorf("got %v", err)
		}
	})

	t.Run("the role cap", func(t *testing.T) {
		r := roleFixtureRepo()
		for i := 0; i < MaxRolesPerSpace-4; i++ {
			r.addRole(rfSpace, SpaceRole{ID: int64(1000 + i), Name: "filler", Position: 10 + i})
		}
		s, _ := roleService(r)
		if _, err := s.CreateSpaceRole(ctx, rfOwnerID, rfSpace, &CreateSpaceRoleInput{Name: "x"}); !errors.Is(err, ErrTooManyRoles) {
			t.Errorf("got %v", err)
		}
	})
}

func TestUpdateSpaceRolePosition(t *testing.T) {
	ctx := context.Background()
	pos := func(p int) *int { return &p }

	t.Run("taking an occupied slot swaps with the occupant, never a tie", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 60, Name: "helper", Position: 4})
		r.addRole(rfSpace, SpaceRole{ID: 61, Name: "top", Position: 5})
		s, fed, events := roleServiceWithEvents(r)
		// The owner moves "top" (5) onto "mod" (2): mod takes 5, nothing else moves.
		role, err := s.UpdateSpaceRole(ctx, rfOwnerID, rfSpace, 61, &UpdateSpaceRoleInput{Position: pos(2)})
		if err != nil {
			t.Fatal(err)
		}
		if role.Position != 2 {
			t.Errorf("moved role at %d", role.Position)
		}
		want := map[int64]int{rfEveryone: 0, rfMember: 1, 61: 2, rfAdmin: 3, 60: 4, rfMod: 5}
		if got := r.positions(rfSpace); !reflect.DeepEqual(got, want) {
			t.Errorf("positions %v, want %v", got, want)
		}
		assertUniquePositions(t, r, rfSpace)
		// The two roles of the swap are written and relayed: mod (moving up) after top.
		if !reflect.DeepEqual(r.roleUpdates, []int64{61, rfMod}) || !reflect.DeepEqual(fed.relayed(), []int64{61, rfMod}) {
			t.Errorf("updates %v, relayed %v", r.roleUpdates, fed.relayed())
		}
		// Only the role that was asked to move is audited.
		if len(r.audits) != 1 || r.audits[0].TargetID != "61" {
			t.Errorf("audits %+v", r.audits)
		}
		// Another role moved too, so the whole list goes out as well as the role.
		if ups := events.of("SPACE_UPDATE"); len(ups) != 1 || !reflect.DeepEqual(rolePositions(t, ups[0]), want) {
			t.Errorf("SPACE_UPDATE %+v", ups)
		}
		if len(events.of("SPACE_ROLE_UPDATE")) != 1 {
			t.Errorf("events %+v", events.events)
		}
	})

	t.Run("a free position is written as asked", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _, events := roleServiceWithEvents(r)
		if _, err := s.UpdateSpaceRole(ctx, rfOwnerID, rfSpace, rfMember, &UpdateSpaceRoleInput{Position: pos(7)}); err != nil {
			t.Fatal(err)
		}
		want := map[int64]int{rfEveryone: 0, rfMod: 2, rfAdmin: 3, rfMember: 7}
		if got := r.positions(rfSpace); !reflect.DeepEqual(got, want) {
			t.Errorf("positions %v, want %v", got, want)
		}
		if !reflect.DeepEqual(r.roleUpdates, []int64{rfMember}) {
			t.Errorf("updates %v", r.roleUpdates)
		}
		// Only the role itself moved: its own event says so.
		if len(events.of("SPACE_UPDATE")) != 0 || len(events.of("SPACE_ROLE_UPDATE")) != 1 {
			t.Errorf("events %+v", events.events)
		}
	})

	t.Run("its own position is no move", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		if _, err := s.UpdateSpaceRole(ctx, rfModID, rfSpace, rfMember, &UpdateSpaceRoleInput{Position: pos(1)}); err != nil {
			t.Fatal(err)
		}
		if got := r.positions(rfSpace); got[rfMember] != 1 || got[rfMod] != 2 || got[rfAdmin] != 3 {
			t.Errorf("positions %v", got)
		}
		if len(r.audits) != 0 {
			t.Errorf("audits %+v", r.audits)
		}
	})

	t.Run("a moderator swaps two roles below them, and the displaced one stays below them", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 60, Name: "helper", Position: 1})
		r.mu.Lock()
		for rid, p := range map[int64]int{rfMember: 2, rfMod: 3, rfAdmin: 4} {
			role := r.roles[rfSpace][rid]
			role.Position = p
			r.roles[rfSpace][rid] = role
		}
		r.mu.Unlock()
		s, _ := roleService(r)
		if _, err := s.UpdateSpaceRole(ctx, rfModID, rfSpace, 60, &UpdateSpaceRoleInput{Position: pos(2)}); err != nil {
			t.Fatal(err)
		}
		want := map[int64]int{rfEveryone: 0, rfMember: 1, 60: 2, rfMod: 3, rfAdmin: 4}
		if got := r.positions(rfSpace); !reflect.DeepEqual(got, want) {
			t.Errorf("positions %v, want %v", got, want)
		}
	})

	t.Run("legacy ties go through placeRoles, which settles them", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 60, Name: "helper", Position: 2}) // tied with mod
		s, _ := roleService(r)
		if _, err := s.UpdateSpaceRole(ctx, rfOwnerID, rfSpace, rfAdmin, &UpdateSpaceRoleInput{Position: pos(1)}); err != nil {
			t.Fatal(err)
		}
		// admin takes 1 and the run above it goes up; the tie resolves newer-lower (60 < 52 by
		// rank since 60 is the higher id): admin 1, member 2, helper 3, mod 4.
		want := map[int64]int{rfEveryone: 0, rfAdmin: 1, rfMember: 2, 60: 3, rfMod: 4}
		if got := r.positions(rfSpace); !reflect.DeepEqual(got, want) {
			t.Errorf("positions %v, want %v", got, want)
		}
		assertUniquePositions(t, r, rfSpace)
	})

	t.Run("a moderator moves a role below them", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 60, Name: "helper", Position: 1})
		r.addRole(rfSpace, SpaceRole{ID: 62, Name: "x", Position: 0}) // broken legacy data
		s, _ := roleService(r)
		if _, err := s.UpdateSpaceRole(ctx, rfModID, rfSpace, 60, &UpdateSpaceRoleInput{Position: pos(1)}); err != nil {
			t.Fatal(err)
		}
		assertUniquePositions(t, r, rfSpace)
	})

	t.Run("the existing checks still apply", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		cases := []struct {
			actor, role int64
			in          UpdateSpaceRoleInput
			want        error
		}{
			{rfModID, rfMod, UpdateSpaceRoleInput{Position: pos(1)}, ErrRoleHierarchy},      // their own rank
			{rfModID, rfAdmin, UpdateSpaceRoleInput{Position: pos(1)}, ErrRoleHierarchy},    // above them
			{rfModID, rfMember, UpdateSpaceRoleInput{Position: pos(2)}, ErrRoleHierarchy},   // to their rank
			{rfModID, rfMember, UpdateSpaceRoleInput{Position: pos(9)}, ErrRoleHierarchy},   // past it
			{rfAdminID, rfMember, UpdateSpaceRoleInput{Position: pos(4)}, ErrRoleHierarchy}, // Administrator does not lift it
			{rfOwnerID, rfMember, UpdateSpaceRoleInput{Position: pos(0)}, ErrInvalidRolePosition},
			{rfOwnerID, rfMember, UpdateSpaceRoleInput{Position: pos(MaxRolePosition + 1)}, ErrInvalidRolePosition},
			{rfOwnerID, rfEveryone, UpdateSpaceRoleInput{Position: pos(3)}, ErrCannotMoveEveryone},
			{rfOwnerID, 77, UpdateSpaceRoleInput{Position: pos(3)}, ErrRoleNotFound},
		}
		for _, tc := range cases {
			if _, err := s.UpdateSpaceRole(ctx, tc.actor, rfSpace, tc.role, &tc.in); !errors.Is(err, tc.want) {
				t.Errorf("actor %d role %d %+v: got %v, want %v", tc.actor, tc.role, tc.in, err, tc.want)
			}
		}
		if len(r.roleUpdates) != 0 {
			t.Errorf("refused updates wrote %v", r.roleUpdates)
		}
	})

	t.Run("an edit without a position writes only the role", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		name := "members"
		if _, err := s.UpdateSpaceRole(ctx, rfModID, rfSpace, rfMember, &UpdateSpaceRoleInput{Name: &name}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r.roleUpdates, []int64{rfMember}) {
			t.Errorf("updates %v", r.roleUpdates)
		}
	})
}

func TestSwapRoles(t *testing.T) {
	const ev = 1
	everyone := SpaceRole{ID: ev, Name: EveryoneRoleName, Position: 0}
	r := func(id int64, pos int) SpaceRole { return SpaceRole{ID: id, Position: pos} }
	const a, b, c, d = 10, 11, 12, 13
	cases := []struct {
		name  string
		roles []SpaceRole
		role  int64
		to    int
		want  map[int64]int
	}{
		{"onto an occupied slot: the two trade", []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 3), r(d, 4)}, d, 1, map[int64]int{d: 1, a: 4}},
		{"downwards just the same", []SpaceRole{everyone, r(a, 1), r(b, 2), r(c, 3), r(d, 4)}, a, 3, map[int64]int{a: 3, c: 1}},
		{"a free slot, as asked", []SpaceRole{everyone, r(a, 1), r(b, 5)}, a, 3, map[int64]int{a: 3}},
		{"its own slot", []SpaceRole{everyone, r(a, 1), r(b, 2)}, b, 2, map[int64]int{}},
		{"@everyone is never the occupant", []SpaceRole{everyone, r(a, 1), r(b, 2)}, a, 3, map[int64]int{a: 3}},
		{"ties: placeRoles", []SpaceRole{everyone, r(a, 1), r(b, 1), r(c, 2)}, c, 1, placeRoles([]SpaceRole{everyone, r(a, 1), r(b, 1), r(c, 2)}, ev, map[int64]int{c: 1})},
		{"a role below 1: placeRoles", []SpaceRole{everyone, r(a, 0), r(b, 1)}, b, 3, map[int64]int{a: 1, b: 3}},
	}
	for _, tc := range cases {
		got := swapRoles(tc.roles, ev, tc.role, tc.to)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		assertDistinct(t, finalPositions(tc.roles, got), ev)
	}
}

// permutations is every ordering of ids.
func permutations(ids []int64) [][]int64 {
	if len(ids) <= 1 {
		return [][]int64{append([]int64(nil), ids...)}
	}
	var out [][]int64
	for i := range ids {
		rest := make([]int64, 0, len(ids)-1)
		rest = append(rest, ids[:i]...)
		rest = append(rest, ids[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]int64{ids[i]}, p...))
		}
	}
	return out
}

// TestSingleRolePatchSequences replays the reorders clients make with one PATCH
// /spaces/:id/roles/:roleId {position} per role, over every order of four roles, from
// consecutive positions and from positions with gaps:
//   - older web builds renumber every custom role 1..N, lowest first;
//   - the mobile app's releases (and the desktop app) send only the roles that change, reusing
//     the positions the moved roles hold, lowest requested position first - and since every one
//     of these is a permutation of slots, the reverse order must land the same.
//
// Each must end with the roles in the order asked for, every position unique, and (for a
// moderator) nothing at or above them touched. With insert-and-bump the second request already
// names a slot that holds something else (A1 B2 C3 D4, mobile sends C->1 then A->3: C,A,B,D).
func TestSingleRolePatchSequences(t *testing.T) {
	ctx := context.Background()
	const (
		space    = 900
		everyone = 90
		a, b, c  = 91, 92, 93
		d        = 94
		top      = 95 // the moderator's role, above the four
		owner    = rfOwnerID
		mod      = rfModID
	)
	four := []int64{a, b, c, d}
	setup := func(start map[int64]int) (*orderRepo, *Service) {
		r := &orderRepo{
			spaces: map[int64]*Space{space: {ID: space, OwnerID: owner, EveryoneRoleID: everyone, Name: "order"}},
			roles: map[int64]map[int64]SpaceRole{space: {
				everyone: {SpaceID: space, ID: everyone, Name: EveryoneRoleName, Position: 0, Permissions: permissions.DefaultEveryone},
				top:      {SpaceID: space, ID: top, Name: "top", Position: start[top], Permissions: permissions.PermManageRoles},
			}},
			members: map[int64]map[int64]*SpaceMember{space: {
				owner: {SpaceID: space, UserID: owner, RoleIDs: []int64{everyone}, JoinedAt: time.Now()},
				mod:   {SpaceID: space, UserID: mod, RoleIDs: []int64{everyone, top}, JoinedAt: time.Now()},
			}},
		}
		for _, rid := range four {
			r.roles[space][rid] = SpaceRole{SpaceID: space, ID: rid, Name: id.Format(rid), Position: start[rid]}
		}
		s, _ := roleService(r)
		return r, s
	}
	patch := func(t *testing.T, s *Service, actor, roleID int64, p int) {
		t.Helper()
		if _, err := s.UpdateSpaceRole(ctx, actor, space, roleID, &UpdateSpaceRoleInput{Position: &p}); err != nil {
			t.Fatalf("role %d -> %d: %v", roleID, p, err)
		}
	}
	// check: the four roles rank in want's order (lowest first), positions unique, and the
	// moderator's role where it started when they made the moves.
	check := func(t *testing.T, r *orderRepo, want []int64, topAt int) {
		t.Helper()
		assertUniquePositions(t, r, space)
		got := r.positions(space)
		for i := 1; i < len(want); i++ {
			if got[want[i-1]] >= got[want[i]] {
				t.Fatalf("want order %v (lowest first), got positions %v", want, got)
			}
		}
		if topAt != 0 && got[top] != topAt {
			t.Fatalf("the moderator's role moved: %v", got)
		}
	}
	starts := map[string]map[int64]int{
		"consecutive": {a: 1, b: 2, c: 3, d: 4, top: 5},
		"with gaps":   {a: 2, b: 5, c: 6, d: 9, top: 12},
	}
	for name, start := range starts {
		slots := []int{start[a], start[b], start[c], start[d]} // ascending, as listed
		for _, want := range permutations(four) {
			label := fmt.Sprintf("%s %v", name, want)

			// Older web: every custom role, 1..N lowest first, the owner's role list
			// including the top role above the four.
			t.Run("web renumbering "+label, func(t *testing.T) {
				r, s := setup(start)
				for i, rid := range append(append([]int64(nil), want...), top) {
					patch(t, s, owner, rid, i+1)
				}
				check(t, r, want, 0)
				if got := r.positions(space); got[want[0]] != 1 || got[top] != 5 {
					t.Errorf("not renumbered 1..5: %v", got)
				}
			})

			// Mobile: the roles that change, each given the slot its new rank reuses.
			target := map[int64]int{}
			var changed []int64
			for i, rid := range want {
				target[rid] = slots[i]
				if start[rid] != slots[i] {
					changed = append(changed, rid)
				}
			}
			sort.Slice(changed, func(i, j int) bool { return target[changed[i]] < target[changed[j]] })
			for _, actor := range []int64{owner, mod} {
				for _, reverse := range []bool{false, true} {
					seq := append([]int64(nil), changed...)
					if reverse {
						for i, j := 0, len(seq)-1; i < j; i, j = i+1, j-1 {
							seq[i], seq[j] = seq[j], seq[i]
						}
					}
					t.Run(fmt.Sprintf("slot reuse %s actor %d reverse %v", label, actor, reverse), func(t *testing.T) {
						r, s := setup(start)
						for _, rid := range seq {
							patch(t, s, actor, rid, target[rid])
						}
						check(t, r, want, start[top])
						got := r.positions(space)
						for _, rid := range four {
							if got[rid] != target[rid] {
								t.Fatalf("role %d at %d, want the slot %d: %v", rid, got[rid], target[rid], got)
							}
						}
					})
				}
			}
		}
	}

	t.Run("the reported sequence", func(t *testing.T) {
		r, s := setup(starts["consecutive"])
		patch(t, s, owner, c, 1)
		patch(t, s, owner, a, 3)
		if got := r.positions(space); got[c] != 1 || got[b] != 2 || got[a] != 3 || got[d] != 4 {
			t.Errorf("positions %v, want C1 B2 A3 D4", got)
		}
	})
}

func TestReorderSpaceRoles(t *testing.T) {
	ctx := context.Background()

	t.Run("the owner swaps two roles and gets every role back", func(t *testing.T) {
		r := roleFixtureRepo()
		s, fed := roleService(r)
		all, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{rfMember, 3}, {rfAdmin, 1}})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 4 || all[0].ID != rfEveryone || all[1].ID != rfAdmin || all[2].ID != rfMod || all[3].ID != rfMember {
			t.Errorf("returned %+v", all)
		}
		assertUniquePositions(t, r, rfSpace)
		if len(r.audits) != 2 {
			t.Errorf("audits %+v", r.audits)
		}
		if len(fed.relayed()) != 2 {
			t.Errorf("relayed %v", fed.relayed())
		}
	})

	t.Run("roles pushed up to make room are not audited", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		// member (1) onto admin's slot (3): admin goes to 4, member to 3; mod stays.
		if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{rfMember, 3}}); err != nil {
			t.Fatal(err)
		}
		got := r.positions(rfSpace)
		if got[rfMember] != 3 || got[rfAdmin] != 4 || got[rfMod] != 2 {
			t.Errorf("positions %v", got)
		}
		if len(r.audits) != 1 || r.audits[0].TargetID != "51" || r.audits[0].Changes != `{"position":{"old":1,"new":3}}` {
			t.Errorf("audits %+v", r.audits)
		}
	})

	t.Run("a bot's role can be moved, @everyone cannot", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 70, Name: "RoleBot", Position: 4, BotID: 700})
		s, _ := roleService(r)
		if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{70, 1}}); err != nil {
			t.Errorf("move a managed role: %v", err)
		}
		if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{rfEveryone, 1}}); !errors.Is(err, ErrCannotMoveEveryone) {
			t.Errorf("move @everyone: %v", err)
		}
	})

	t.Run("a non-owner moves only below themselves, or nothing at all", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 60, Name: "helper", Position: 5})
		s, _ := roleService(r)
		// mod is at 2: only member (1) is theirs to move, and only to 1.
		for _, moves := range [][]RolePosition{
			{{rfMember, 1}, {rfMod, 1}},   // their own role
			{{rfMember, 1}, {rfAdmin, 1}}, // a role above them
			{{rfMember, 2}},               // to their own rank
			{{rfMember, 5}},               // past it
		} {
			if _, err := s.ReorderSpaceRoles(ctx, rfModID, rfSpace, moves); !errors.Is(err, ErrRoleHierarchy) {
				t.Errorf("%v: got %v", moves, err)
			}
		}
		if _, err := s.ReorderSpaceRoles(ctx, rfAdminID, rfSpace, []RolePosition{{rfMember, 3}}); !errors.Is(err, ErrRoleHierarchy) {
			t.Errorf("Administrator to their own rank: %v", err)
		}
		if len(r.roleUpdates) != 0 || len(r.audits) != 0 {
			t.Errorf("a refused reorder wrote %v / audited %v", r.roleUpdates, r.audits)
		}
		// Administrator (3) below themselves: fine.
		if _, err := s.ReorderSpaceRoles(ctx, rfAdminID, rfSpace, []RolePosition{{rfMember, 2}, {rfMod, 1}}); err != nil {
			t.Fatal(err)
		}
		if got := r.positions(rfSpace); got[rfMod] != 1 || got[rfMember] != 2 || got[rfAdmin] != 3 {
			t.Errorf("positions %v", got)
		}
	})

	t.Run("malformed requests and unknown roles", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		many := make([]RolePosition, MaxRolesPerSpace+1)
		for i := range many {
			many[i] = RolePosition{RoleID: int64(i + 1), Position: 1}
		}
		cases := []struct {
			moves []RolePosition
			want  error
		}{
			{nil, ErrInvalidRolePositions},
			{many, ErrInvalidRolePositions},
			{[]RolePosition{{rfMember, 2}, {rfMember, 3}}, ErrInvalidRolePositions},
			{[]RolePosition{{rfMember, 0}}, ErrInvalidRolePosition},
			{[]RolePosition{{rfMember, -4}}, ErrInvalidRolePosition},
			{[]RolePosition{{rfMember, MaxRolePosition + 1}}, ErrInvalidRolePosition},
			{[]RolePosition{{77, 2}}, ErrRoleNotFound},
		}
		for _, tc := range cases {
			if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, tc.moves); !errors.Is(err, tc.want) {
				t.Errorf("%d entries: got %v, want %v", len(tc.moves), err, tc.want)
			}
		}
		if _, err := s.ReorderSpaceRoles(ctx, rfMemberID, rfSpace, []RolePosition{{rfMember, 1}}); !errors.Is(err, ErrMissingPerm) {
			t.Errorf("without Manage Roles: %v", err)
		}
	})

	t.Run("a mirror asks its origin", func(t *testing.T) {
		r := roleFixtureRepo()
		s, fed := roleService(r)
		fed.mirrors[rfSpace] = "origin.example"
		moves := []RolePosition{{rfMember, 2}}
		if _, err := s.ReorderSpaceRoles(ctx, rfModID, rfSpace, moves); err != nil {
			t.Fatal(err)
		}
		if len(fed.reorders) != 1 || !reflect.DeepEqual(fed.reorders[0], moves) || len(r.roleUpdates) != 0 {
			t.Errorf("forwarded %v, wrote %v", fed.reorders, r.roleUpdates)
		}
	})
}

// ---- the reported bot flow -----------------------------------------------------------------

// TestBotGivesRoles is the report, end to end through the service: a bot installed with Manage
// Roles can hand out what is below its managed role - which starts at the bottom - and the
// member's own rank, the owner's included, never matters.
func TestBotGivesRoles(t *testing.T) {
	ctx := context.Background()
	const (
		botID = 700
		alice = rfPlainID
	)
	r := roleFixtureRepo()
	s, fed := roleService(r)
	users := s.userRepo.(*profileUsers)
	users.users[botID] = &auth.User{ID: botID, Username: "RoleBot", Bot: true}

	verified, err := s.CreateSpaceRole(ctx, rfOwnerID, rfSpace, &CreateSpaceRoleInput{Name: "Verified"})
	if err != nil {
		t.Fatal(err)
	}
	granted, err := s.InstallBot(ctx, rfOwnerID, rfSpace, botID, permissions.PermManageRoles)
	if err != nil || granted != permissions.PermManageRoles {
		t.Fatalf("install: %d %v", granted, err)
	}
	var botRole SpaceRole
	for _, role := range r.roles[rfSpace] {
		if role.BotID == botID {
			botRole = role
		}
	}
	if botRole.ID == 0 || botRole.Position != 1 {
		t.Fatalf("bot role %+v, want it at 1", botRole)
	}
	assertUniquePositions(t, r, rfSpace)
	if got := r.positions(rfSpace)[verified.ID]; got != 2 {
		t.Errorf("Verified at %d after the install, want 2 (pushed up by one)", got)
	}
	if relayed := fed.relayed(); len(relayed) == 0 || relayed[len(relayed)-1] != botRole.ID {
		t.Errorf("relays %v should end with the bot role", relayed)
	}

	// A: right after the install the bot's role is the lowest - nothing to hand out yet.
	if err := s.AddMemberRole(ctx, botID, rfSpace, alice, verified.ID); !errors.Is(err, ErrRoleHierarchy) {
		t.Fatalf("A: got %v, want %v", err, ErrRoleHierarchy)
	}
	// C: the owner drags the bot's role to the top.
	if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{botRole.ID, 99}}); err != nil {
		t.Fatal(err)
	}
	// B: Discord's single-role endpoint.
	if err := s.AddMemberRole(ctx, botID, rfSpace, alice, verified.ID); err != nil {
		t.Fatalf("B/C: %v", err)
	}
	if !hasRoleID(r.roleIDsOf(rfSpace, alice), verified.ID) {
		t.Fatal("alice lacks Verified")
	}
	// D: a role the bot creates is below it, so the bot can give it out.
	reaction, err := s.CreateSpaceRole(ctx, botID, rfSpace, &CreateSpaceRoleInput{Name: "Reaction role"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberRoles(ctx, botID, rfSpace, alice, []int64{verified.ID, reaction.ID}); err != nil {
		t.Fatalf("D: %v", err)
	}
	// E: the owner can be given a role below the bot.
	if err := s.AddMemberRole(ctx, botID, rfSpace, rfOwnerID, verified.ID); err != nil {
		t.Fatalf("E (single): %v", err)
	}
	if err := s.SetMemberRoles(ctx, botID, rfSpace, rfOwnerID, []int64{verified.ID, reaction.ID}); err != nil {
		t.Fatalf("E (list): %v", err)
	}
	// F: alice holds a role above the bot; the bot still gives her one below it, and the role
	// above it stays.
	vip, err := s.CreateSpaceRole(ctx, rfOwnerID, rfSpace, &CreateSpaceRoleInput{Name: "VIP"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{vip.ID, 200}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMemberRole(ctx, rfOwnerID, rfSpace, alice, vip.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberRoles(ctx, botID, rfSpace, alice, []int64{verified.ID}); err != nil {
		t.Fatalf("F: %v", err)
	}
	held := r.roleIDsOf(rfSpace, alice)
	if !hasRoleID(held, vip.ID) || !hasRoleID(held, verified.ID) || hasRoleID(held, reaction.ID) {
		t.Errorf("F: alice holds %v (VIP %d carried over, Verified %d kept, Reaction %d removed)", held, vip.ID, verified.ID, reaction.ID)
	}
	if err := s.RemoveMemberRole(ctx, botID, rfSpace, alice, vip.ID); !errors.Is(err, ErrRoleHierarchy) {
		t.Errorf("the bot removed a role above it: %v", err)
	}
	// The bot's own role stays unassignable.
	if err := s.AddMemberRole(ctx, rfOwnerID, rfSpace, alice, botRole.ID); !errors.Is(err, ErrManagedRole) {
		t.Errorf("managed role granted: %v", err)
	}
	assertUniquePositions(t, r, rfSpace)

	// The bot leaves: its role goes and the gap closes.
	r.mu.Lock()
	delete(r.members[rfSpace], botID)
	r.mu.Unlock()
	s.cleanupBotRoles(ctx, rfSpace, botID)
	if _, ok := r.positions(rfSpace)[botRole.ID]; ok {
		t.Error("bot role survived its bot")
	}
	assertUniquePositions(t, r, rfSpace)
}

func TestCleanupBotRolesClosesTheGap(t *testing.T) {
	ctx := context.Background()
	r := roleFixtureRepo()
	r.mu.Lock()
	for _, rid := range []int64{rfMember, rfMod, rfAdmin} {
		role := r.roles[rfSpace][rid]
		role.Position++
		r.roles[rfSpace][rid] = role
	}
	r.mu.Unlock()
	r.addRole(rfSpace, SpaceRole{ID: 70, Name: "RoleBot", Position: 1, BotID: 700})
	s, fed := roleService(r)
	s.cleanupBotRoles(ctx, rfSpace, 700)
	got := r.positions(rfSpace)
	if _, ok := got[70]; ok || got[rfMember] != 1 || got[rfMod] != 2 || got[rfAdmin] != 3 {
		t.Errorf("positions %v", got)
	}
	// Moving down, so lowest first.
	if want := []int64{rfMember, rfMod, rfAdmin}; !reflect.DeepEqual(r.roleUpdates, want) {
		t.Errorf("updates %v, want %v", r.roleUpdates, want)
	}
	if len(fed.relayed()) != 3 {
		t.Errorf("relayed %v", fed.relayed())
	}
}

func TestCloseRoleGaps(t *testing.T) {
	const ev = 1
	everyone := SpaceRole{ID: ev, Name: EveryoneRoleName, Position: 0}
	r := func(id int64, pos int) SpaceRole { return SpaceRole{ID: id, Position: pos} }
	const a, c, d, x = 10, 12, 13, 14
	cases := []struct {
		name string
		kept []SpaceRole
		gone []int
		want map[int64]int
	}{
		{"the usual: the bot's role at 1", []SpaceRole{everyone, r(a, 2), r(c, 3)}, []int{1}, map[int64]int{a: 1, c: 2}},
		{"tied with the role below it", []SpaceRole{everyone, r(a, 2), r(c, 3)}, []int{2}, map[int64]int{}},
		{"tied with a role above another", []SpaceRole{everyone, r(a, 2), r(c, 3), r(d, 4)}, []int{3}, map[int64]int{}},
		{"two gone, one of them tied", []SpaceRole{everyone, r(a, 2), r(c, 4), r(d, 6)}, []int{1, 4}, map[int64]int{a: 1, c: 3, d: 4}},
		// x is the newer of the tied two, so it ranks lower and is the one that moves down.
		{"a tie among the kept settles in rank order", []SpaceRole{everyone, r(a, 3), r(x, 3), r(d, 4)}, []int{1}, map[int64]int{x: 2}},
		{"a removed role tied at 1 frees nothing", []SpaceRole{everyone, r(a, 1), r(c, 2)}, []int{1}, map[int64]int{}},
		{"never below 1", []SpaceRole{everyone, r(a, 1), r(c, 3)}, []int{1, 2}, map[int64]int{c: 2}},
	}
	for _, tc := range cases {
		got := closeRoleGaps(tc.kept, ev, tc.gone)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		final := finalPositions(tc.kept, got)
		assertDistinct(t, final, ev)
		// The order of the kept roles never changes (ties by id, newer lower).
		ranked := lowerFirst(tc.kept)
		for i := 1; i < len(ranked); i++ {
			if ranked[i-1].ID == ev {
				continue
			}
			if final[ranked[i-1].ID] >= final[ranked[i].ID] {
				t.Errorf("%s: %d and %d swapped ranks: %v", tc.name, ranked[i-1].ID, ranked[i].ID, final)
			}
		}
	}
}

// TestCleanupBotRolesKeepsTheOrder: a role tied with the departing bot's role (legacy data, or
// a mirror's copy) is not moved past the role it was above or below.
func TestCleanupBotRolesKeepsTheOrder(t *testing.T) {
	ctx := context.Background()
	const (
		roleA = 60
		roleC = 61
		roleD = 62
		bot   = 70
		botID = 700
	)
	cases := []struct {
		name  string
		start map[int64]int // roles besides the fixture's, the bot's included
		want  map[int64]int // every role's position after
	}{
		{
			name:  "A=2, bot=2, C=3",
			start: map[int64]int{roleA: 2, bot: 2, roleC: 3},
			want:  map[int64]int{rfEveryone: 0, rfMember: 1, roleA: 2, roleC: 3},
		},
		{
			name:  "A=2, C=3 tied with bot at 3, D=4",
			start: map[int64]int{roleA: 2, roleC: 3, bot: 3, roleD: 4},
			want:  map[int64]int{rfEveryone: 0, rfMember: 1, roleA: 2, roleC: 3, roleD: 4},
		},
		{
			name:  "the usual: bot at 1, then 2 and 3",
			start: map[int64]int{bot: 1, roleA: 2, roleC: 3},
			want:  map[int64]int{rfEveryone: 0, roleA: 1, roleC: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := roleFixtureRepo()
			r.mu.Lock()
			// Only member (moved out of the way above when the case has a 1) and @everyone stay.
			delete(r.roles[rfSpace], rfMod)
			delete(r.roles[rfSpace], rfAdmin)
			if tc.start[bot] == 1 {
				delete(r.roles[rfSpace], rfMember)
			}
			r.mu.Unlock()
			for rid, p := range tc.start {
				role := SpaceRole{ID: rid, Name: id.Format(rid), Position: p}
				if rid == bot {
					role.BotID = botID
				}
				r.addRole(rfSpace, role)
			}
			s, _ := roleService(r)
			s.cleanupBotRoles(ctx, rfSpace, botID)
			if got := r.positions(rfSpace); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("positions %v, want %v", got, tc.want)
			}
			assertUniquePositions(t, r, rfSpace)
		})
	}
}

// ---- writes that fail part-way, and what the space is told -------------------------------

func TestApplyRoleOrderPartialFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("what was written is announced, read back", func(t *testing.T) {
		r := roleFixtureRepo()
		r.failUpdateAt = 2
		s, fed, events := roleServiceWithEvents(r)
		// admin (3) to the bottom: member and mod move up (written top down: mod, then member,
		// which fails), then admin would go to 1.
		_, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{rfAdmin, 1}})
		if !errors.Is(err, errUpdateFailed) {
			t.Fatalf("got %v, want the write's error", err)
		}
		stored := r.positions(rfSpace)
		if reflect.DeepEqual(stored, map[int64]int{rfEveryone: 0, rfMember: 1, rfMod: 2, rfAdmin: 3}) {
			t.Fatal("nothing was written before the failure; the test needs a partial write")
		}
		ups := events.of("SPACE_UPDATE")
		if len(ups) != 1 {
			t.Fatalf("SPACE_UPDATE x%d, want 1 (events %+v)", len(ups), events.events)
		}
		if got := rolePositions(t, ups[0]); !reflect.DeepEqual(got, stored) {
			t.Errorf("announced %v, stored %v", got, stored)
		}
		// The mirrors were told exactly what was written.
		if len(fed.relayed()) != 1 || len(r.audits) != 0 {
			t.Errorf("relayed %v, audits %+v", fed.relayed(), r.audits)
		}
	})

	t.Run("a role's own edit failing part-way still tells the space", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 61, Name: "top", Position: 5})
		r.failUpdateAt = 2
		s, _, events := roleServiceWithEvents(r)
		p, name := 2, "renamed"
		if _, err := s.UpdateSpaceRole(ctx, rfOwnerID, rfSpace, 61, &UpdateSpaceRoleInput{Position: &p, Name: &name}); !errors.Is(err, errUpdateFailed) {
			t.Fatalf("got %v", err)
		}
		ups := events.of("SPACE_UPDATE")
		if len(ups) != 1 || !reflect.DeepEqual(rolePositions(t, ups[0]), r.positions(rfSpace)) {
			t.Errorf("SPACE_UPDATE %+v, stored %v", ups, r.positions(rfSpace))
		}
		if len(events.of("SPACE_ROLE_UPDATE")) != 0 {
			t.Error("the failed edit was announced as done")
		}
	})

	t.Run("nothing written, nothing announced", func(t *testing.T) {
		r := roleFixtureRepo()
		r.failUpdateAt = 1
		s, fed, events := roleServiceWithEvents(r)
		if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{rfAdmin, 1}}); !errors.Is(err, errUpdateFailed) {
			t.Fatalf("got %v", err)
		}
		if len(events.events) != 0 || len(fed.relayed()) != 0 {
			t.Errorf("events %+v, relayed %v", events.events, fed.relayed())
		}
	})

	t.Run("a write that does not announce leaves it to its caller", func(t *testing.T) {
		r := roleFixtureRepo()
		r.failUpdateAt = 2
		s, _, events := roleServiceWithEvents(r)
		roles, _ := r.ListSpaceRoles(ctx, rfSpace)
		placed := placeRoles(roles, rfEveryone, map[int64]int{rfAdmin: 1})
		if _, err := s.applyRoleOrder(ctx, r.spaces[rfSpace], rfEveryone, roles, placed, 0, false, false); !errors.Is(err, errUpdateFailed) {
			t.Fatalf("got %v", err)
		}
		if len(events.events) != 0 {
			t.Errorf("events %+v", events.events)
		}
	})
}

// TestPublishRoleOrderReadsTheSpace: the SPACE_UPDATE a reorder sends is a whole space, so it
// must be the space as it is now - not the copy the request read before a rename landed.
func TestPublishRoleOrderReadsTheSpace(t *testing.T) {
	ctx := context.Background()

	t.Run("a rename while the roles are written", func(t *testing.T) {
		r := roleFixtureRepo()
		var once sync.Once
		r.onUpdate = func() {
			once.Do(func() {
				r.mu.Lock()
				r.spaces[rfSpace].Name = "Renamed"
				r.spaces[rfSpace].Description = "new description"
				r.mu.Unlock()
			})
		}
		s, _, events := roleServiceWithEvents(r)
		if _, err := s.ReorderSpaceRoles(ctx, rfOwnerID, rfSpace, []RolePosition{{rfAdmin, 1}}); err != nil {
			t.Fatal(err)
		}
		ups := events.of("SPACE_UPDATE")
		if len(ups) != 1 {
			t.Fatalf("SPACE_UPDATE x%d", len(ups))
		}
		if ups[0].Data["name"] != "Renamed" || ups[0].Data["description"] != "new description" {
			t.Errorf("sent name %v, description %v: the space as the request first read it", ups[0].Data["name"], ups[0].Data["description"])
		}
		// Still the whole space: clients default what is missing.
		for _, k := range []string{"id", "owner_id", "everyone_role_id", "roles", "verification_level", "features"} {
			if _, ok := ups[0].Data[k]; !ok {
				t.Errorf("payload lacks %q", k)
			}
		}
	})

	t.Run("a space gone by then is not announced", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _, events := roleServiceWithEvents(r)
		sp := *r.spaces[rfSpace]
		r.mu.Lock()
		delete(r.spaces, rfSpace)
		r.mu.Unlock()
		roles, _ := r.ListSpaceRoles(ctx, rfSpace)
		s.publishRoleOrder(ctx, &sp, rfEveryone, roles)
		if len(events.events) != 0 {
			t.Errorf("events %+v", events.events)
		}
	})
}

func TestInstallBotRoleCap(t *testing.T) {
	ctx := context.Background()
	r := roleFixtureRepo()
	for i := 0; i < MaxRolesPerSpace-4; i++ {
		r.addRole(rfSpace, SpaceRole{ID: int64(1000 + i), Name: "filler", Position: 10 + i})
	}
	s, _ := roleService(r)
	s.userRepo.(*profileUsers).users[700] = &auth.User{ID: 700, Username: "bot", Bot: true}
	if _, err := s.InstallBot(ctx, rfOwnerID, rfSpace, 700, permissions.PermManageRoles); !errors.Is(err, ErrTooManyRoles) {
		t.Errorf("got %v", err)
	}
}

// TestReinstallBot: re-authorising an installed bot rewrites its role's permissions in place;
// one installed with no permissions (so no role) gets its role at 1 on the way.
func TestReinstallBot(t *testing.T) {
	ctx := context.Background()
	r := roleFixtureRepo()
	s, _ := roleService(r)
	s.userRepo.(*profileUsers).users[700] = &auth.User{ID: 700, Username: "RoleBot", Bot: true}

	if granted, err := s.InstallBot(ctx, rfOwnerID, rfSpace, 700, 0); err != nil || granted != 0 {
		t.Fatalf("install without permissions: %d %v", granted, err)
	}
	if n := len(r.positions(rfSpace)); n != 4 {
		t.Fatalf("%d roles: a role-less install made one", n)
	}
	if _, err := s.InstallBot(ctx, rfOwnerID, rfSpace, 700, permissions.PermManageRoles); err != nil {
		t.Fatal(err)
	}
	var bot SpaceRole
	for _, role := range r.roles[rfSpace] {
		if role.BotID == 700 {
			bot = role
		}
	}
	if bot.ID == 0 || bot.Position != 1 || bot.Permissions != permissions.PermManageRoles {
		t.Fatalf("bot role %+v", bot)
	}
	if !hasRoleID(r.roleIDsOf(rfSpace, 700), bot.ID) {
		t.Errorf("the bot does not hold its role: %v", r.roleIDsOf(rfSpace, 700))
	}
	assertUniquePositions(t, r, rfSpace)

	// Again with other permissions: the same role, edited, nothing moved.
	before := r.positions(rfSpace)
	if _, err := s.InstallBot(ctx, rfOwnerID, rfSpace, 700, permissions.PermManageRoles|permissions.PermKickMembers); err != nil {
		t.Fatal(err)
	}
	if got := r.roles[rfSpace][bot.ID].Permissions; got != permissions.PermManageRoles|permissions.PermKickMembers {
		t.Errorf("permissions %d", got)
	}
	if after := r.positions(rfSpace); !reflect.DeepEqual(after, before) {
		t.Errorf("positions moved: %v -> %v", before, after)
	}
}
