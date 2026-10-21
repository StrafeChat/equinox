package permissions

import "testing"

// The numbers here are also written into the developer docs (07-permissions.md) and the web
// client's mirror (lib/spacePermissions.ts); a change to any mask must move all three.
func TestNicknameBits(t *testing.T) {
	if PermChangeNickname != 268435456 {
		t.Errorf("PermChangeNickname = %d, want 268435456 (1<<28)", PermChangeNickname)
	}
	if PermManageNicknames != 536870912 {
		t.Errorf("PermManageNicknames = %d, want 536870912 (1<<29)", PermManageNicknames)
	}
	// Space-scoped: the owner and Administrators hold them (SpacePermissionBase returns
	// AllSpace), and they never reach a room override (overrides are masked with AllRoom).
	for name, bit := range map[string]int64{"ChangeNickname": PermChangeNickname, "ManageNicknames": PermManageNicknames} {
		if !Has(AllSpace, bit) {
			t.Errorf("AllSpace lacks %s", name)
		}
		if AllRoom&bit != 0 {
			t.Errorf("AllRoom has %s", name)
		}
	}
	if !Has(DefaultEveryone, PermChangeNickname) {
		t.Error("DefaultEveryone lacks Change Nickname")
	}
	if DefaultEveryone&PermManageNicknames != 0 {
		t.Error("DefaultEveryone has Manage Nicknames")
	}
}

func TestMaskValues(t *testing.T) {
	cases := []struct {
		name string
		got  int64
		want int64
	}{
		{"DefaultEveryone", DefaultEveryone, 363044895},
		{"AllRoom", AllRoom, 268402815},
		{"AllSpace", AllSpace, 1073741823},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	// Every bit 0-29 is assigned and all of them are space bits; bit 30 is the last the web
	// client's 32-bit maths can hold, so nothing may sit at or above it yet.
	if AllSpace != 1<<30-1 {
		t.Errorf("AllSpace = %d, want bits 0-29 exactly", AllSpace)
	}
}
