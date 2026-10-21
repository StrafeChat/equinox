package spaces

import (
	"context"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

func TestMirrorNicknameBits(t *testing.T) {
	change, manage := permissions.PermChangeNickname, permissions.PermManageNicknames
	cases := []struct {
		name  string
		role  string
		perms int64
		want  int64
	}{
		{"@everyone gains Change Nickname", EveryoneRoleName, permissions.PermViewRoom, permissions.PermViewRoom | change},
		{"kick gains Manage Nicknames", "mod", permissions.PermKickMembers, permissions.PermKickMembers | manage},
		{"ban gains Manage Nicknames", "mod", permissions.PermBanMembers, permissions.PermBanMembers | manage},
		{"plain custom role unchanged", "member", permissions.PermSendMessages, permissions.PermSendMessages},
		{"Manage Roles alone is not moderation", "roles", permissions.PermManageRoles, permissions.PermManageRoles},
		{"@everyone that can kick gets both", EveryoneRoleName, permissions.PermKickMembers, permissions.PermKickMembers | change | manage},
	}
	for _, tc := range cases {
		got := mirrorNicknameBits(tc.role, tc.perms)
		if got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
		if again := mirrorNicknameBits(tc.role, got); again != got {
			t.Errorf("%s: not idempotent (%d then %d)", tc.name, got, again)
		}
	}
}

// nickBackfillRepo adds the data-migration and role-scan methods to profileRepo.
type nickBackfillRepo struct {
	*profileRepo
	all map[int64][]SpaceRole // every space's roles, by space
}

func (r *nickBackfillRepo) ClaimDataMigration(_ context.Context, name string) (bool, error) {
	if r.claimed[name] {
		return false, nil
	}
	r.claimed[name] = true
	return true, nil
}

func (r *nickBackfillRepo) ForEachSpaceRole(_ context.Context, fn func(spaceID, roleID int64, name string, permissions, botID int64) error) error {
	for sid, roles := range r.all {
		for _, role := range roles {
			if err := fn(sid, role.ID, role.Name, role.Permissions, role.BotID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *nickBackfillRepo) UpdateSpaceRolePermissions(_ context.Context, spaceID, roleID int64, perms int64, _ time.Time) error {
	r.permWrites[roleID] = perms
	return nil
}

func (r *nickBackfillRepo) GetSpaceRole(_ context.Context, spaceID, roleID int64) (*SpaceRole, error) {
	for _, role := range r.all[spaceID] {
		if role.ID == roleID {
			cp := role
			cp.Permissions = r.permWrites[roleID]
			return &cp, nil
		}
	}
	return nil, nil
}

// nickMirrorInfo says space 200 is hosted on another instance.
type nickMirrorInfo struct{}

func (nickMirrorInfo) SpaceFederation(_ context.Context, spaceID int64) *SpaceFederation {
	if spaceID == 200 {
		return &SpaceFederation{OriginDomain: "b.example", OriginID: "9"}
	}
	return nil
}

func (nickMirrorInfo) RoomFederations(context.Context, []int64) map[int64]*rooms.Federation {
	return nil
}

func TestBackfillNicknamePermissions(t *testing.T) {
	r := &nickBackfillRepo{
		profileRepo: &profileRepo{claimed: map[string]bool{}, permWrites: map[int64]int64{}},
		all: map[int64][]SpaceRole{
			100: {
				{ID: 1, Name: EveryoneRoleName, Permissions: permissions.PermViewRoom},
				{ID: 2, Name: "mod", Permissions: permissions.PermKickMembers},
				{ID: 3, Name: "member", Permissions: permissions.PermSendMessages},
				// A bot's role holds what its installer approved, nothing more.
				{ID: 4, Name: "bot", Permissions: permissions.PermBanMembers, BotID: 77},
				{ID: 5, Name: "already", Permissions: permissions.PermBanMembers | permissions.PermManageNicknames},
			},
			// A mirror: its origin backfills and relays; the copy here is left alone.
			200: {
				{ID: 6, Name: EveryoneRoleName, Permissions: permissions.PermViewRoom},
				{ID: 7, Name: "mod", Permissions: permissions.PermKickMembers},
			},
		},
	}
	s := NewService(r, nil, nil, nil, nil)
	s.SetFederationInfo(nickMirrorInfo{})
	if err := s.BackfillNicknamePermissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[int64]int64{
		1: permissions.PermViewRoom | permissions.PermChangeNickname,
		2: permissions.PermKickMembers | permissions.PermManageNicknames,
	}
	if len(r.permWrites) != len(want) {
		t.Errorf("wrote roles %v, want only %v", r.permWrites, want)
	}
	for id, perms := range want {
		if r.permWrites[id] != perms {
			t.Errorf("role %d = %d, want %d", id, r.permWrites[id], perms)
		}
	}
	// Exactly once per keyspace.
	r.permWrites = map[int64]int64{}
	if err := s.BackfillNicknamePermissions(context.Background()); err != nil || len(r.permWrites) != 0 {
		t.Errorf("second run: err %v, writes %v", err, r.permWrites)
	}
}
