package spaces

import (
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

func TestResolveEffectiveRoomPermissions_OrderAndPrecedence(t *testing.T) {
	const (
		everyoneID = int64(1)
		roleA      = int64(2)
		roleB      = int64(3)
		userID     = int64(99)
	)

	base := permissions.PermViewRoom | permissions.PermSendMessages
	roleOverrides := []SpaceRoomRoleOverride{
		{RoleID: everyoneID, Deny: permissions.PermSendMessages, Allow: 0},
		{RoleID: roleA, Deny: 0, Allow: permissions.PermSendMessages},
		{RoleID: roleB, Deny: permissions.PermSendMessages, Allow: 0},
	}

	got := resolveEffectiveRoomPermissions(base, everyoneID, []int64{everyoneID, roleA, roleB}, userID, roleOverrides, nil)
	if !permissions.Has(got, permissions.PermSendMessages) {
		t.Fatalf("expected role-stage allow to win after deny in same stage, got: %d", got)
	}

	userOverrides := []SpaceRoomUserOverride{
		{UserID: userID, Allow: 0, Deny: permissions.PermSendMessages},
	}
	got = resolveEffectiveRoomPermissions(base, everyoneID, []int64{everyoneID, roleA, roleB}, userID, roleOverrides, userOverrides)
	if permissions.Has(got, permissions.PermSendMessages) {
		t.Fatalf("expected user override deny to win in final stage, got: %d", got)
	}
}

func TestResolveEffectiveRoomPermissions_UserDenyBeatsRoleAllow(t *testing.T) {
	const (
		everyoneID = int64(1)
		roleA      = int64(2)
		userID     = int64(7)
	)

	base := permissions.PermViewRoom | permissions.PermSendMessages
	roleOverrides := []SpaceRoomRoleOverride{
		{RoleID: roleA, Allow: permissions.PermSendMessages, Deny: 0},
	}
	userOverrides := []SpaceRoomUserOverride{
		{UserID: userID, Allow: 0, Deny: permissions.PermSendMessages},
	}

	got := resolveEffectiveRoomPermissions(base, everyoneID, []int64{everyoneID, roleA}, userID, roleOverrides, userOverrides)
	if permissions.Has(got, permissions.PermSendMessages) {
		t.Fatalf("expected final user deny to remove send_messages, got: %d", got)
	}
}
