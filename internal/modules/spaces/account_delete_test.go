package spaces

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// RemoveAccount: the leave a deleted account makes from every space it was in.

// leavingFed is the hook world's federator with an origin that answers a leave with err,
// and the optional mirror dropper.
type leavingFed struct {
	*visFed
	err     error
	asked   []int64
	dropped []int64
}

func (f *leavingFed) LeaveRemoteSpace(_ context.Context, _ string, _ int64, u *auth.User) error {
	f.asked = append(f.asked, u.ID)
	return f.err
}

func (f *leavingFed) AfterSpaceMemberRemoved(context.Context, int64, int64, string) {}

func (f *leavingFed) DropMirrorIfEmpty(_ context.Context, spaceID int64) {
	f.dropped = append(f.dropped, spaceID)
}

func TestRemoveAccountFromASpaceHostedHere(t *testing.T) {
	ctx := context.Background()
	w := newHookWorld()
	w.svc.SetFederator(&leavingFed{visFed: w.fed})
	w.repo.spaces[rfSpace].WidgetInviteCode = "widget"
	w.repo.invites = []SpaceInvite{
		{Code: "mine", SpaceID: rfSpace, InviterID: rfPlainID},
		{Code: "widget", SpaceID: rfSpace, InviterID: rfPlainID},
		{Code: "theirs", SpaceID: rfSpace, InviterID: rfMemberID},
	}
	w.repo.userOv[hookRoom] = map[int64]SpaceRoomUserOverride{
		rfPlainID:  {SpaceID: rfSpace, RoomID: hookRoom, UserID: rfPlainID},
		rfMemberID: {SpaceID: rfSpace, RoomID: hookRoom, UserID: rfMemberID},
	}

	warning, err := w.svc.RemoveAccount(ctx, rfPlainID, rfSpace)
	if err != nil || warning != "" {
		t.Fatalf("RemoveAccount = %q, %v", warning, err)
	}
	if m, _ := w.repo.GetMember(ctx, rfSpace, rfPlainID); m != nil {
		t.Fatal("still a member")
	}
	if !eq64(w.removed, []int64{rfPlainID}) {
		t.Errorf("member-removed hook saw %v", w.removed)
	}
	codes := []string{}
	for _, inv := range w.repo.invites {
		codes = append(codes, inv.Code)
	}
	if strings.Join(codes, ",") != "widget,theirs" {
		t.Errorf("invites left %v: the account's own go, the space widget's and others' stay", codes)
	}
	if _, ok := w.repo.userOv[hookRoom][rfPlainID]; ok {
		t.Error("the account's room override survived")
	}
	if _, ok := w.repo.userOv[hookRoom][rfMemberID]; !ok {
		t.Error("someone else's override went")
	}

	// Idempotent: nothing left to remove is not an error, and a space that is gone neither.
	if _, err := w.svc.RemoveAccount(ctx, rfPlainID, rfSpace); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if len(w.removed) != 1 {
		t.Errorf("a rerun removed again: %v", w.removed)
	}
	if _, err := w.svc.RemoveAccount(ctx, rfPlainID, 4242); err != nil {
		t.Fatalf("missing space: %v", err)
	}
	// The owner of a space hosted here cannot leave it: the pipeline takes it down first.
	if _, err := w.svc.RemoveAccount(ctx, rfOwnerID, rfSpace); !errors.Is(err, ErrOwnerCannotLeave) {
		t.Fatalf("owner: %v", err)
	}
}

func TestRemoveAccountFromAMirror(t *testing.T) {
	ctx := context.Background()

	t.Run("the origin takes the leave", func(t *testing.T) {
		w := newHookWorld()
		w.mirror()
		fed := &leavingFed{visFed: w.fed}
		w.svc.SetFederator(fed)
		warning, err := w.svc.RemoveAccount(ctx, rfPlainID, rfSpace)
		if err != nil || warning != "" || !eq64(fed.asked, []int64{rfPlainID}) {
			t.Fatalf("= %q, %v; asked %v", warning, err, fed.asked)
		}
		// The origin's relay removes the member; nothing is done locally meanwhile.
		if len(w.removed) != 0 {
			t.Errorf("removed locally as well: %v", w.removed)
		}
	})

	t.Run("the origin cannot be reached", func(t *testing.T) {
		w := newHookWorld()
		w.mirror()
		fed := &leavingFed{visFed: w.fed, err: errors.New("a.example unreachable")}
		w.svc.SetFederator(fed)
		warning, err := w.svc.RemoveAccount(ctx, rfPlainID, rfSpace)
		if err != nil || !strings.Contains(warning, "a.example") {
			t.Fatalf("= %q, %v; want a warning naming the origin", warning, err)
		}
		if m, _ := w.repo.GetMember(ctx, rfSpace, rfPlainID); m != nil {
			t.Fatal("not removed locally")
		}
		if !eq64(w.removed, []int64{rfPlainID}) || !eq64(fed.dropped, []int64{rfSpace}) {
			t.Errorf("hook %v, mirror drop %v", w.removed, fed.dropped)
		}
	})
}
