package spaces

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
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

// ---- R3: granting checks the role, never the member -----------------------------------------

func TestSetMemberRolesChecksOnlyTheRoles(t *testing.T) {
	ctx := context.Background()
	// mod (2) hands out member (1) - to the owner, to an admin who outranks them, to someone
	// at their own rank and to themselves: Discord checks the role's position, not the member's.
	for _, target := range []int64{rfOwnerID, rfAdminID, rfMod2ID, rfModID, rfPlainID} {
		r := roleFixtureRepo()
		s, fed := roleService(r)
		current := r.roleIDsOf(rfSpace, target)
		want := withRoleID(current, indexRolesOf(r), rfEveryone, rfMember, true)
		if err := s.SetMemberRoles(ctx, rfModID, rfSpace, target, want); err != nil {
			t.Errorf("mod gives member to %d: %v", target, err)
			continue
		}
		if !hasRoleID(r.roleIDsOf(rfSpace, target), rfMember) {
			t.Errorf("target %d lacks the role", target)
		}
		if len(fed.memberRelays) != 1 || len(r.audits) != 1 || r.audits[0].ActionType != AuditMemberRolesUpdate {
			t.Errorf("target %d: relays %v audits %+v", target, fed.memberRelays, r.audits)
		}
	}

	t.Run("and takes it away from the owner", func(t *testing.T) {
		r := roleFixtureRepo()
		r.members[rfSpace][rfOwnerID].RoleIDs = []int64{rfEveryone, rfMember}
		s, _ := roleService(r)
		if err := s.SetMemberRoles(ctx, rfModID, rfSpace, rfOwnerID, nil); err != nil {
			t.Fatal(err)
		}
		if got := r.roleIDsOf(rfSpace, rfOwnerID); !reflect.DeepEqual(got, []int64{rfEveryone}) {
			t.Errorf("owner holds %v", got)
		}
	})

	t.Run("the roles above the actor are still out of reach", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		for _, role := range []int64{rfMod, rfAdmin} {
			if err := s.SetMemberRoles(ctx, rfModID, rfSpace, rfPlainID, []int64{role}); !errors.Is(err, ErrRoleHierarchy) {
				t.Errorf("mod grants %d: %v", role, err)
			}
		}
		// The admin's own role is carried over, not stripped, when mod edits their roles.
		if err := s.SetMemberRoles(ctx, rfModID, rfSpace, rfAdminID, []int64{rfMember}); err != nil {
			t.Fatal(err)
		}
		if got := r.roleIDsOf(rfSpace, rfAdminID); !hasRoleID(got, rfAdmin) || !hasRoleID(got, rfMember) {
			t.Errorf("admin holds %v", got)
		}
	})

	t.Run("managed roles, permissions and membership", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 70, Name: "RoleBot", Position: 1, BotID: 700})
		s, _ := roleService(r)
		if err := s.SetMemberRoles(ctx, rfOwnerID, rfSpace, rfPlainID, []int64{70}); !errors.Is(err, ErrManagedRole) {
			t.Errorf("managed role: %v", err)
		}
		if err := s.SetMemberRoles(ctx, rfMemberID, rfSpace, rfPlainID, []int64{rfMember}); !errors.Is(err, ErrMissingPerm) {
			t.Errorf("without Manage Roles: %v", err)
		}
		if err := s.SetMemberRoles(ctx, rfModID, rfSpace, 99, []int64{rfMember}); !errors.Is(err, ErrNotMember) {
			t.Errorf("non-member target: %v", err)
		}
	})
}

func indexRolesOf(r *orderRepo) map[int64]*SpaceRole {
	roles, _ := r.ListSpaceRoles(context.Background(), rfSpace)
	return indexRoles(roles)
}

func TestAddRemoveMemberRole(t *testing.T) {
	ctx := context.Background()

	t.Run("add, add again, remove, remove again", func(t *testing.T) {
		r := roleFixtureRepo()
		s, fed := roleService(r)
		if err := s.AddMemberRole(ctx, rfModID, rfSpace, rfPlainID, rfMember); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMemberRole(ctx, rfModID, rfSpace, rfPlainID, rfMember); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveMemberRole(ctx, rfModID, rfSpace, rfPlainID, rfMember); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveMemberRole(ctx, rfModID, rfSpace, rfPlainID, rfMember); err != nil {
			t.Fatal(err)
		}
		// One write each way: holding it already (or not) writes and announces nothing.
		if want := []string{"add:5:51", "remove:5:51"}; !reflect.DeepEqual(r.memberWrites, want) {
			t.Errorf("writes %v, want %v", r.memberWrites, want)
		}
		if len(fed.memberRelays) != 2 || len(r.audits) != 2 {
			t.Errorf("relays %v audits %d", fed.memberRelays, len(r.audits))
		}
		if r.audits[0].ActionType != AuditMemberRolesUpdate || r.audits[0].TargetID != "5" || r.audits[0].Changes != `{"roles":{"new":["50","51"]}}` {
			t.Errorf("audit %+v", r.audits[0])
		}
		if got := r.roleIDsOf(rfSpace, rfPlainID); !reflect.DeepEqual(got, []int64{rfEveryone}) {
			t.Errorf("roles %v", got)
		}
	})

	t.Run("whoever the member is", func(t *testing.T) {
		r := roleFixtureRepo()
		s, _ := roleService(r)
		for _, target := range []int64{rfOwnerID, rfAdminID, rfMod2ID, rfModID} {
			if err := s.AddMemberRole(ctx, rfModID, rfSpace, target, rfMember); err != nil {
				t.Errorf("add to %d: %v", target, err)
			}
			if err := s.RemoveMemberRole(ctx, rfModID, rfSpace, target, rfMember); err != nil {
				t.Errorf("remove from %d: %v", target, err)
			}
		}
		// The owner may hand out any role but a bot's.
		if err := s.AddMemberRole(ctx, rfOwnerID, rfSpace, rfPlainID, rfAdmin); err != nil {
			t.Errorf("owner grants admin: %v", err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		r := roleFixtureRepo()
		r.addRole(rfSpace, SpaceRole{ID: 70, Name: "RoleBot", Position: 1, BotID: 700})
		r.members[rfSpace][700] = &SpaceMember{SpaceID: rfSpace, UserID: 700, RoleIDs: []int64{rfEveryone, 70}, JoinedAt: time.Now()}
		s, _ := roleService(r)
		cases := []struct {
			name                  string
			actor, target, roleID int64
			add                   bool
			want                  error
		}{
			{"unknown role", rfModID, rfPlainID, 77, true, ErrRoleNotFound},
			{"@everyone", rfOwnerID, rfPlainID, rfEveryone, true, ErrEveryoneRole},
			{"@everyone removed", rfOwnerID, rfPlainID, rfEveryone, false, ErrEveryoneRole},
			{"a bot's role given", rfOwnerID, rfPlainID, 70, true, ErrManagedRole},
			{"a bot's role taken from it", rfOwnerID, 700, 70, false, ErrManagedRole},
			{"the actor's own rank", rfModID, rfPlainID, rfMod, true, ErrRoleHierarchy},
			{"above the actor", rfModID, rfPlainID, rfAdmin, true, ErrRoleHierarchy},
			{"above the actor, removed", rfModID, rfAdminID, rfAdmin, false, ErrRoleHierarchy},
			{"Administrator does not lift it", rfAdminID, rfPlainID, rfAdmin, true, ErrRoleHierarchy},
			{"held already, still above the actor", rfModID, rfAdminID, rfAdmin, true, ErrRoleHierarchy},
			{"not a member", rfModID, 99, rfMember, true, ErrNotMember},
			{"without Manage Roles", rfMemberID, rfPlainID, rfMember, true, ErrMissingPerm},
			{"actor not in the space", 99, rfPlainID, rfMember, true, ErrNotMember},
		}
		for _, tc := range cases {
			var err error
			if tc.add {
				err = s.AddMemberRole(ctx, tc.actor, rfSpace, tc.target, tc.roleID)
			} else {
				err = s.RemoveMemberRole(ctx, tc.actor, rfSpace, tc.target, tc.roleID)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
			}
		}
		if len(r.memberWrites) != 0 || len(r.audits) != 0 {
			t.Errorf("refusals wrote %v / audited %v", r.memberWrites, r.audits)
		}
	})

	t.Run("two at once both land", func(t *testing.T) {
		r := roleFixtureRepo()
		r.yield = true
		r.addRole(rfSpace, SpaceRole{ID: 60, Name: "a", Position: 1})
		r.addRole(rfSpace, SpaceRole{ID: 61, Name: "b", Position: 1})
		s, _ := roleService(r)
		var wg sync.WaitGroup
		for _, role := range []int64{60, 61, rfMember} {
			wg.Add(1)
			go func(role int64) {
				defer wg.Done()
				if err := s.AddMemberRole(ctx, rfOwnerID, rfSpace, rfPlainID, role); err != nil {
					t.Error(err)
				}
			}(role)
		}
		wg.Wait()
		if got := r.roleIDsOf(rfSpace, rfPlainID); !reflect.DeepEqual(got, []int64{rfEveryone, rfMember, 60, 61}) {
			t.Errorf("roles %v", got)
		}
	})
}

func TestMemberRoleOnAMirror(t *testing.T) {
	ctx := context.Background()
	setup := func() (*orderRepo, *Service, *recordingFed) {
		r := roleFixtureRepo()
		s, fed := roleService(r)
		fed.mirrors[rfSpace] = "origin.example"
		return r, s, fed
	}

	t.Run("the one change goes to the origin, not the list", func(t *testing.T) {
		r, s, fed := setup()
		if err := s.AddMemberRole(ctx, rfModID, rfSpace, rfPlainID, rfMember); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveMemberRole(ctx, rfModID, rfSpace, rfMemberID, rfMember); err != nil {
			t.Fatal(err)
		}
		want := []remoteChangeRole{
			{origin: "origin.example", actor: rfModID, target: rfPlainID, role: rfMember, add: true, spaceID: rfSpace},
			{origin: "origin.example", actor: rfModID, target: rfMemberID, role: rfMember, add: false, spaceID: rfSpace},
		}
		if !reflect.DeepEqual(fed.changes, want) {
			t.Errorf("forwarded %+v, want %+v", fed.changes, want)
		}
		if len(fed.setRoles) != 0 {
			t.Errorf("a list was sent too: %+v", fed.setRoles)
		}
		if len(r.memberWrites) != 0 || len(r.audits) != 0 {
			t.Errorf("a mirror wrote %v / audited %v", r.memberWrites, r.audits)
		}
	})

	t.Run("an origin without the op gets the whole list", func(t *testing.T) {
		r, s, fed := setup()
		fed.changeErr = fmt.Errorf("%w: member.role", ErrOriginLacksOperation)
		// 88: a role deleted since; the member row still names it, the origin would refuse it.
		r.members[rfSpace][rfMemberID].RoleIDs = []int64{rfEveryone, rfMember, 88}
		if err := s.AddMemberRole(ctx, rfModID, rfSpace, rfPlainID, rfMember); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveMemberRole(ctx, rfModID, rfSpace, rfMemberID, rfMember); err != nil {
			t.Fatal(err)
		}
		want := []remoteSetRoles{
			{origin: "origin.example", actor: rfModID, target: rfPlainID, roleIDs: []int64{rfMember}, spaceID: rfSpace},
			{origin: "origin.example", actor: rfModID, target: rfMemberID, roleIDs: []int64{}, spaceID: rfSpace},
		}
		if len(fed.changes) != 2 || !reflect.DeepEqual(fed.setRoles, want) {
			t.Errorf("asked %+v, then forwarded %+v, want %+v", fed.changes, fed.setRoles, want)
		}
		if len(r.memberWrites) != 0 || len(r.audits) != 0 {
			t.Errorf("a mirror wrote %v / audited %v", r.memberWrites, r.audits)
		}
	})

	t.Run("any other refusal from the origin is the answer", func(t *testing.T) {
		_, s, fed := setup()
		refusal := &OriginError{Status: 403, Message: ErrRoleHierarchy.Error()}
		fed.changeErr = refusal
		if err := s.AddMemberRole(ctx, rfModID, rfSpace, rfPlainID, rfMember); !errors.Is(err, refusal) {
			t.Errorf("got %v, want the origin's refusal", err)
		}
		if len(fed.setRoles) != 0 {
			t.Errorf("retried with the list: %+v", fed.setRoles)
		}
	})

	t.Run("nothing to change, nothing forwarded; refusals stay local", func(t *testing.T) {
		_, s, fed := setup()
		if err := s.AddMemberRole(ctx, rfModID, rfSpace, rfMemberID, rfMember); err != nil {
			t.Fatal(err)
		}
		// Without the local check this would forward a list the origin quietly carries the
		// role over in, and answer success.
		if err := s.RemoveMemberRole(ctx, rfModID, rfSpace, rfAdminID, rfAdmin); !errors.Is(err, ErrRoleHierarchy) {
			t.Errorf("got %v", err)
		}
		if len(fed.setRoles) != 0 || len(fed.changes) != 0 {
			t.Errorf("forwarded %+v / %+v", fed.setRoles, fed.changes)
		}
	})
}

// TestTargetRankStillGuardsModeration: the same moderator who may now give a higher member a
// role still may not kick, ban or rename them - Discord keeps the member's rank for those.
func TestTargetRankStillGuardsModeration(t *testing.T) {
	ctx := context.Background()
	r := roleFixtureRepo()
	s, _ := roleService(r)
	if err := s.AddMemberRole(ctx, rfModID, rfSpace, rfAdminID, rfMember); err != nil {
		t.Fatalf("grant to a higher member: %v", err)
	}
	for _, target := range []int64{rfAdminID, rfMod2ID} {
		if err := s.KickMember(ctx, rfModID, rfSpace, target); !errors.Is(err, ErrRoleHierarchy) {
			t.Errorf("kick %d: %v", target, err)
		}
		if err := s.BanMember(ctx, rfModID, rfSpace, target, ""); !errors.Is(err, ErrRoleHierarchy) {
			t.Errorf("ban %d: %v", target, err)
		}
		if err := s.canModerateMember(ctx, rfSpace, rfModID, target, permissions.PermKickMembers); !errors.Is(err, ErrRoleHierarchy) {
			t.Errorf("moderate %d: %v", target, err)
		}
		m := *r.members[rfSpace][target]
		if err := s.memberProfilePermission(ctx, r.spaces[rfSpace], rfModID, &m, &MemberProfilePatch{Nick: profileStr("x")}); !errors.Is(err, ErrRoleHierarchy) {
			t.Errorf("rename %d: %v", target, err)
		}
	}
	if err := s.canModerateMember(ctx, rfSpace, rfModID, rfMemberID, permissions.PermKickMembers); err != nil {
		t.Errorf("a lower member: %v", err)
	}
	if err := s.KickMember(ctx, rfModID, rfSpace, rfOwnerID); !errors.Is(err, ErrCannotModerateOwner) {
		t.Errorf("kick the owner: %v", err)
	}
}
