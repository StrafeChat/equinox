package spaces

import (
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

// Embed Links follows Send Messages wherever it was granted or denied before the bit existed:
// a role, an override's allow and an override's deny alike.
func TestMirrorEmbedLinks(t *testing.T) {
	send, embed := permissions.PermSendMessages, permissions.PermEmbedLinks
	cases := []struct {
		name string
		mask int64
		want int64
	}{
		{"send gains embed", permissions.PermViewRoom | send, permissions.PermViewRoom | send | embed},
		{"no send, nothing", permissions.PermViewRoom | permissions.PermAttachFiles, permissions.PermViewRoom | permissions.PermAttachFiles},
		{"empty override half", 0, 0},
		{"already there", send | embed, send | embed},
	}
	for _, tc := range cases {
		got := mirrorEmbedLinks(tc.mask)
		if got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
		if again := mirrorEmbedLinks(got); again != got {
			t.Errorf("%s: not idempotent (%d then %d)", tc.name, got, again)
		}
	}
	// A deny of Send Messages becomes a deny of both, so the override still reads the same.
	allow, deny := int64(0), send
	if mirrorEmbedLinks(allow) != 0 || mirrorEmbedLinks(deny) != send|embed {
		t.Error("a send deny must deny embedding too")
	}
}
