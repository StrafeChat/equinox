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

// The overwrite stack is Discord's: @everyone overwrite, then the member's role overwrites
// aggregated (all denies, then all allows), then the member overwrite - followed by the two
// implicit rules (no View Room = nothing; no Send Messages = no Attach Files / @everyone).
func TestResolveEffectiveRoomPermissions_DiscordRules(t *testing.T) {
	const everyone, roleA, roleB, user = int64(1), int64(2), int64(3), int64(42)
	text := permissions.PermViewRoom | permissions.PermSendMessages | permissions.PermAttachFiles |
		permissions.PermMentionEveryone | permissions.PermAddReactions

	t.Run("role allow beats @everyone deny; member deny beats role allow", func(t *testing.T) {
		got := resolveEffectiveRoomPermissions(text, everyone, []int64{roleA}, user,
			[]SpaceRoomRoleOverride{
				{RoleID: everyone, Deny: permissions.PermSendMessages},
				{RoleID: roleA, Allow: permissions.PermSendMessages},
			},
			[]SpaceRoomUserOverride{{UserID: user, Deny: permissions.PermAddReactions}})
		if !permissions.Has(got, permissions.PermSendMessages) {
			t.Fatalf("role allow should override the @everyone deny: %b", got)
		}
		if permissions.Has(got, permissions.PermAddReactions) {
			t.Fatalf("member deny should win over the role grant: %b", got)
		}
	})

	t.Run("role denies aggregate before role allows", func(t *testing.T) {
		got := resolveEffectiveRoomPermissions(text, everyone, []int64{roleA, roleB}, user,
			[]SpaceRoomRoleOverride{
				{RoleID: roleA, Deny: permissions.PermAttachFiles},
				{RoleID: roleB, Allow: permissions.PermAttachFiles},
			}, nil)
		if !permissions.Has(got, permissions.PermAttachFiles) {
			t.Fatalf("an allow on any of the member's roles should win over a deny on another: %b", got)
		}
	})

	t.Run("no View Room means no permissions at all", func(t *testing.T) {
		got := resolveEffectiveRoomPermissions(text|permissions.AllVoice, everyone, nil, user,
			[]SpaceRoomRoleOverride{{RoleID: everyone, Deny: permissions.PermViewRoom}}, nil)
		if got != 0 {
			t.Fatalf("want 0 without View Room, got %b", got)
		}
	})

	t.Run("no Send Messages clears Attach Files and @everyone but nothing else", func(t *testing.T) {
		got := resolveEffectiveRoomPermissions(text, everyone, nil, user,
			nil, []SpaceRoomUserOverride{{UserID: user, Deny: permissions.PermSendMessages}})
		if permissions.Has(got, permissions.PermAttachFiles) || permissions.Has(got, permissions.PermMentionEveryone) {
			t.Fatalf("attach/mention should follow Send Messages: %b", got)
		}
		if !permissions.Has(got, permissions.PermViewRoom) || !permissions.Has(got, permissions.PermAddReactions) {
			t.Fatalf("unrelated bits must survive: %b", got)
		}
	})

	t.Run("AllRoom carries every room bit including Attach Files", func(t *testing.T) {
		for _, bit := range []int64{permissions.PermViewRoom, permissions.PermSendMessages, permissions.PermAttachFiles,
			permissions.PermReadMessageHistory, permissions.PermAddReactions, permissions.PermUseExternalEmojis,
			permissions.PermMentionEveryone, permissions.PermManageMessages, permissions.PermConnect, permissions.PermPrioritySpeaker} {
			if !permissions.Has(permissions.AllRoom, bit) {
				t.Fatalf("AllRoom is missing bit %b", bit)
			}
		}
	})
}
