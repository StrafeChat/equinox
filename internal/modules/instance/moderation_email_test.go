package instance

import (
	"context"
	"testing"
)

// The admin tool normalises through auth.NormalizeEmail and moves the address through
// auth.ApplyEmailChange (one ChangeEmail call, verified), the only way an address moves.
func TestSetUserEmailNormalisesAndApplies(t *testing.T) {
	svc, mod, _ := newModService(t)
	users := svc.mod.Users.(*fakeUsers)
	ctx := context.Background()

	got, err := svc.SetUserEmail(ctx, adminID, aliceID, " Bob@X.com ")
	if err != nil {
		t.Fatal(err)
	}
	if len(users.changes) != 1 {
		t.Fatalf("ChangeEmail calls = %d, want 1", len(users.changes))
	}
	c := users.changes[0]
	if c.userID != aliceID || c.old != "alice@x" || c.new != "bob@x.com" || !c.verified {
		t.Errorf("ChangeEmail(%+v), want alice@x -> bob@x.com verified", c)
	}
	if got.Email != "bob@x.com" || !got.VerifiedEmail {
		t.Errorf("returned %q verified=%v", got.Email, got.VerifiedEmail)
	}
	audit, _ := mod.ListAudit(ctx, 10)
	if len(audit) != 1 || audit[0].Action != AuditUserEmail || audit[0].TargetID != aliceID || audit[0].Reason != "alice@x -> bob@x.com" {
		t.Errorf("audit = %+v, want one %s entry with both addresses", audit, AuditUserEmail)
	}
}

func TestSetUserEmailRefusals(t *testing.T) {
	svc, mod, _ := newModService(t)
	users := svc.mod.Users.(*fakeUsers)
	users.users[aliceID].Email = "alice@x.com"
	users.users[mallory].Email = "mallory@x.com"
	ctx := context.Background()

	if _, err := svc.SetUserEmail(ctx, adminID, aliceID, "bob@x.com."); err != ErrInvalidEmail {
		t.Errorf("trailing dot = %v, want ErrInvalidEmail", err)
	}
	if _, err := svc.SetUserEmail(ctx, adminID, aliceID, "ALICE@x.com"); err != nil {
		t.Errorf("same address = %v, want a no-op", err)
	}
	if len(users.changes) != 0 {
		t.Errorf("a refused or unchanged address must not reach ChangeEmail: %+v", users.changes)
	}
	if _, err := svc.SetUserEmail(ctx, adminID, aliceID, "Mallory@X.com"); err != ErrEmailTaken {
		t.Errorf("taken address = %v, want ErrEmailTaken", err)
	}
	if audit, _ := mod.ListAudit(ctx, 10); len(audit) != 0 {
		t.Errorf("nothing changed, so nothing is audited: %+v", audit)
	}
	if _, err := svc.SetUserEmail(ctx, aliceID, mallory, "new@x.com"); err != ErrNotAdmin {
		t.Errorf("non-admin = %v, want ErrNotAdmin", err)
	}
	if _, err := svc.SetUserEmail(ctx, adminID, remoteID, "far@x.com"); err != ErrCannotBanRemote {
		t.Errorf("remote = %v, want ErrCannotBanRemote", err)
	}
}
