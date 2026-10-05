package instance

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// fakeModRepo is the moderation store in memory, with the same "move between status
// partitions" shape as the Scylla one so the queue logic is what gets tested.
type fakeModRepo struct {
	mu         sync.Mutex
	bans       map[int64]*Ban
	ipBans     map[string]*IPBan
	reports    map[int64]*Report
	audit      []AuditEntry
	peerPolicy []PeerPolicyEntry
}

func newFakeModRepo() *fakeModRepo {
	return &fakeModRepo{bans: map[int64]*Ban{}, ipBans: map[string]*IPBan{}, reports: map[int64]*Report{}}
}

func (f *fakeModRepo) CreateIPBan(_ context.Context, b *IPBan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *b
	f.ipBans[b.CIDR] = &cp
	return nil
}
func (f *fakeModRepo) DeleteIPBan(_ context.Context, cidr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ipBans, cidr)
	return nil
}
func (f *fakeModRepo) ListIPBans(_ context.Context) ([]IPBan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []IPBan{}
	for _, b := range f.ipBans {
		out = append(out, *b)
	}
	return out, nil
}

func (f *fakeModRepo) CreateBan(_ context.Context, b *Ban) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *b
	f.bans[b.UserID] = &cp
	return nil
}
func (f *fakeModRepo) DeleteBan(_ context.Context, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.bans, userID)
	return nil
}
func (f *fakeModRepo) GetBan(_ context.Context, userID int64) (*Ban, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.bans[userID]; ok {
		cp := *b
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeModRepo) ListBans(_ context.Context) ([]Ban, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Ban{}
	for _, b := range f.bans {
		out = append(out, *b)
	}
	return out, nil
}
func (f *fakeModRepo) CreateReport(_ context.Context, r *Report) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.reports[r.ID] = &cp
	return nil
}
func (f *fakeModRepo) GetReport(_ context.Context, id int64) (*Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.reports[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeModRepo) ListReportsByStatus(_ context.Context, status string, _ int) ([]Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Report{}
	for _, r := range f.reports {
		if r.Status == status {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (f *fakeModRepo) ListReportsByTarget(_ context.Context, tt string, tid int64) ([]ReportSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []ReportSummary{}
	for _, r := range f.reports {
		if r.TargetType == tt && r.TargetID == tid {
			out = append(out, ReportSummary{TargetType: tt, TargetID: tid, ID: r.ID, ReporterID: r.ReporterID, Status: r.Status, Reason: r.Reason, CreatedAt: r.CreatedAt})
		}
	}
	return out, nil
}
func (f *fakeModRepo) MoveReportStatus(_ context.Context, r *Report, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.reports[r.ID] = &cp
	return nil
}
func (f *fakeModRepo) AppendAudit(_ context.Context, e *AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audit = append(f.audit, *e)
	return nil
}
func (f *fakeModRepo) ListAudit(_ context.Context, _ int) ([]AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AuditEntry{}, f.audit...), nil
}

func (f *fakeModRepo) CountUsers(context.Context) (int64, error)  { return 0, nil }
func (f *fakeModRepo) CountSpaces(context.Context) (int64, error) { return 0, nil }

func (f *fakeModRepo) ListPeerPolicy(context.Context) ([]PeerPolicyEntry, error) {
	return append([]PeerPolicyEntry(nil), f.peerPolicy...), nil
}
func (f *fakeModRepo) SetPeerPolicy(_ context.Context, e *PeerPolicyEntry) error {
	for i := range f.peerPolicy {
		if f.peerPolicy[i].Domain == e.Domain {
			f.peerPolicy[i] = *e
			return nil
		}
	}
	f.peerPolicy = append(f.peerPolicy, *e)
	return nil
}
func (f *fakeModRepo) RemovePeerPolicy(_ context.Context, domain string) error {
	out := f.peerPolicy[:0]
	for _, e := range f.peerPolicy {
		if e.Domain != domain {
			out = append(out, e)
		}
	}
	f.peerPolicy = out
	return nil
}

// fakeUsers implements the slice of auth.UserRepository moderation touches.
type fakeUsers struct{ users map[int64]*auth.User }

func (f *fakeUsers) Create(context.Context, *auth.User) error { return nil }
func (f *fakeUsers) GetByID(_ context.Context, id int64) (*auth.User, error) {
	return f.users[id], nil
}
func (f *fakeUsers) GetByIDs(_ context.Context, ids []int64) ([]*auth.User, error) {
	var out []*auth.User
	for _, id := range ids {
		if u := f.users[id]; u != nil {
			out = append(out, u)
		}
	}
	return out, nil
}
func (f *fakeUsers) GetByEmail(_ context.Context, email string) (*auth.User, error) {
	for _, u := range f.users {
		if u.Email == email {
			return u, nil
		}
	}
	return nil, nil
}
func (f *fakeUsers) GetByUsername(_ context.Context, name string) (*auth.User, error) {
	for _, u := range f.users {
		if strings.EqualFold(u.Username, name) && u.HomeDomain == "" {
			return u, nil
		}
	}
	return nil, nil
}
func (f *fakeUsers) EmailExists(context.Context, string) (bool, error) { return false, nil }
func (f *fakeUsers) UsernameTaken(_ context.Context, name string) (bool, error) {
	u, _ := f.GetByUsername(context.Background(), name)
	return u != nil, nil
}
func (f *fakeUsers) UpdateRelationships(context.Context, int64, []int64, []int64) error { return nil }
func (f *fakeUsers) UpdateBlocks(context.Context, int64, []int64, []int64) error        { return nil }
func (f *fakeUsers) UpdateProfile(context.Context, int64, *auth.ProfileUpdate) (*auth.User, error) {
	return nil, nil
}
func (f *fakeUsers) UpsertShadow(context.Context, *auth.User) error { return nil }
func (f *fakeUsers) GetByRemote(context.Context, string, int64) (*auth.User, error) {
	return nil, nil
}
func (f *fakeUsers) SetTOTP(context.Context, int64, string, bool) error  { return nil }
func (f *fakeUsers) SetEmailVerified(context.Context, int64, bool) error { return nil }
func (f *fakeUsers) SetPassword(context.Context, int64, string) error    { return nil }

type fakeSessions struct{ revoked []int64 }

func (f *fakeSessions) Create(context.Context, *auth.Session) error { return nil }
func (f *fakeSessions) GetByTokenHash(context.Context, string) (*auth.Session, error) {
	return nil, nil
}
func (f *fakeSessions) GetByUserSession(context.Context, int64, int64) (*auth.Session, error) {
	return nil, nil
}
func (f *fakeSessions) ListByUser(context.Context, int64) ([]auth.Session, error) { return nil, nil }
func (f *fakeSessions) Revoke(context.Context, int64, int64) error                { return nil }
func (f *fakeSessions) RevokeAllForUser(_ context.Context, userID int64) error {
	f.revoked = append(f.revoked, userID)
	return nil
}

// A shadow row is remote only when both HomeDomain and RemoteID are set (auth.IsRemote).
var remoteOrigin = int64(77)

const (
	adminID  = int64(1)
	aliceID  = int64(2)
	mallory  = int64(3)
	remoteID = int64(4)
)

func newModService(t *testing.T) (*Service, *fakeModRepo, *fakeSessions) {
	t.Helper()
	mod := newFakeModRepo()
	sessions := &fakeSessions{}
	svc := newTestService(newFakeRepo(), adminID)
	svc.SetModeration(ModerationDeps{
		Repo: mod,
		Users: &fakeUsers{users: map[int64]*auth.User{
			adminID:  {ID: adminID, Username: "admin", Email: "admin@x"},
			aliceID:  {ID: aliceID, Username: "alice", Email: "alice@x"},
			mallory:  {ID: mallory, Username: "alice2", Email: "mallory@x"},
			remoteID: {ID: remoteID, Username: "far", HomeDomain: "other.example", RemoteID: &remoteOrigin},
		}},
		Sessions: sessions,
	})
	return svc, mod, sessions
}

func TestBanRevokesSessionsAndBlocksLogin(t *testing.T) {
	svc, _, sessions := newModService(t)
	ctx := context.Background()
	if _, err := svc.BanUser(ctx, adminID, mallory, BanInput{Reason: "spam"}); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != mallory {
		t.Fatalf("sessions revoked for %v, want [%d]", sessions.revoked, mallory)
	}
	banned, reason, err := svc.BanReason(ctx, mallory)
	if err != nil || !banned || reason != "spam" {
		t.Fatalf("BanReason = %v %q %v, want banned with reason", banned, reason, err)
	}
	if _, err := svc.BanUser(ctx, adminID, mallory, BanInput{}); err != ErrAlreadyBanned {
		t.Fatalf("second ban = %v, want ErrAlreadyBanned", err)
	}
	if err := svc.UnbanUser(ctx, adminID, mallory); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if banned, _, _ := svc.BanReason(ctx, mallory); banned {
		t.Fatal("still banned after unban")
	}
}

func TestBanRefusesSelfRemoteAndNonAdmins(t *testing.T) {
	svc, _, _ := newModService(t)
	ctx := context.Background()
	if _, err := svc.BanUser(ctx, adminID, adminID, BanInput{}); err != ErrCannotBanSelf {
		t.Errorf("self ban = %v, want ErrCannotBanSelf", err)
	}
	if _, err := svc.BanUser(ctx, adminID, remoteID, BanInput{}); err != ErrCannotBanRemote {
		t.Errorf("remote ban = %v, want ErrCannotBanRemote", err)
	}
	if _, err := svc.BanUser(ctx, aliceID, mallory, BanInput{}); err != ErrNotAdmin {
		t.Errorf("non-admin ban = %v, want ErrNotAdmin", err)
	}
	if _, err := svc.BanUser(ctx, adminID, 999, BanInput{}); err != ErrUserNotFound {
		t.Errorf("unknown user ban = %v, want ErrUserNotFound", err)
	}
}

func TestExpiredBanClearsItself(t *testing.T) {
	svc, mod, _ := newModService(t)
	past := time.Now().UTC().Add(-time.Minute)
	_ = mod.CreateBan(context.Background(), &Ban{UserID: mallory, ExpiresAt: &past})
	if banned, _, _ := svc.BanReason(context.Background(), mallory); banned {
		t.Fatal("an expired ban still blocks login")
	}
	if b, _ := mod.GetBan(context.Background(), mallory); b != nil {
		t.Fatal("expired ban was not removed on the way past")
	}
}

func TestReportValidationAndDuplicates(t *testing.T) {
	svc, _, _ := newModService(t)
	ctx := context.Background()
	if _, err := svc.CreateReport(ctx, aliceID, CreateReportInput{TargetType: TargetUser, TargetID: "2", Reason: "spam"}); err != ErrReportSelf {
		t.Errorf("self report = %v, want ErrReportSelf", err)
	}
	if _, err := svc.CreateReport(ctx, aliceID, CreateReportInput{TargetType: TargetUser, TargetID: "3", Reason: "made-up"}); err != ErrInvalidReport {
		t.Errorf("bad reason = %v, want ErrInvalidReport", err)
	}
	if _, err := svc.CreateReport(ctx, aliceID, CreateReportInput{TargetType: TargetUser, TargetID: "999", Reason: "spam"}); err != ErrUserNotFound {
		t.Errorf("unknown target = %v, want ErrUserNotFound", err)
	}
	rep, err := svc.CreateReport(ctx, aliceID, CreateReportInput{TargetType: TargetUser, TargetID: "3", Reason: "spam", Details: "sent me 40 links"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != ReportOpen {
		t.Fatalf("new report status %q, want open", rep.Status)
	}
	if _, err := svc.CreateReport(ctx, aliceID, CreateReportInput{TargetType: TargetUser, TargetID: "3", Reason: "harassment"}); err != ErrDuplicateReport {
		t.Errorf("second open report by the same person = %v, want ErrDuplicateReport", err)
	}
	// A different person may report the same target.
	if _, err := svc.CreateReport(ctx, adminID, CreateReportInput{TargetType: TargetUser, TargetID: "3", Reason: "spam"}); err != nil {
		t.Errorf("another reporter: %v", err)
	}
}

func TestResolveReportActions(t *testing.T) {
	svc, mod, sessions := newModService(t)
	ctx := context.Background()
	rep, _ := svc.CreateReport(ctx, aliceID, CreateReportInput{TargetType: TargetUser, TargetID: "3", Reason: "spam"})

	if _, err := svc.ResolveReport(ctx, aliceID, rep.ID, ResolveInput{Action: "dismiss"}); err != ErrNotAdmin {
		t.Fatalf("non-admin resolve = %v, want ErrNotAdmin", err)
	}
	if _, err := svc.ResolveReport(ctx, adminID, rep.ID, ResolveInput{Action: "remove_space"}); err != ErrInvalidAction {
		t.Fatalf("remove_space on a user report = %v, want ErrInvalidAction", err)
	}

	// ban_user does the ban and then closes the report with the resolution recorded.
	out, err := svc.ResolveReport(ctx, adminID, rep.ID, ResolveInput{Action: "ban_user", Note: "confirmed spam"})
	if err != nil {
		t.Fatalf("ban_user: %v", err)
	}
	if out.Status != ReportResolved || out.Resolution != ResolutionBanned || out.ResolutionNote != "confirmed spam" {
		t.Fatalf("resolved report = %+v", out)
	}
	if banned, reason, _ := svc.BanReason(ctx, mallory); !banned || reason != "confirmed spam" {
		t.Fatalf("target not banned with the note as reason (banned=%v reason=%q)", banned, reason)
	}
	if len(sessions.revoked) != 1 {
		t.Fatalf("sessions revoked %v, want exactly the target's", sessions.revoked)
	}
	if _, err := svc.ResolveReport(ctx, adminID, rep.ID, ResolveInput{Action: "dismiss"}); err != ErrReportClosed {
		t.Fatalf("resolving twice = %v, want ErrReportClosed", err)
	}
	open, _ := mod.ListReportsByStatus(ctx, ReportOpen, 10)
	if len(open) != 0 {
		t.Fatalf("%d reports still open after resolution", len(open))
	}
	audit, _ := mod.ListAudit(ctx, 10)
	var actions []string
	for _, e := range audit {
		actions = append(actions, e.Action)
	}
	if len(actions) != 2 || actions[0] != AuditUserBan || actions[1] != AuditReportResolve {
		t.Fatalf("audit actions %v, want [user_ban report_resolve]", actions)
	}
}

func TestSearchUsersByEveryHandle(t *testing.T) {
	svc, _, _ := newModService(t)
	ctx := context.Background()
	cases := map[string]int{
		"3":       1, // id
		"alice@x": 1, // email
		"ALICE":   1, // username, case-insensitive
		"alice":   1, // username
		"nobody":  0,
	}
	for q, want := range cases {
		got, err := svc.SearchUsers(ctx, adminID, q)
		if err != nil {
			t.Errorf("search %q: %v", q, err)
			continue
		}
		if len(got) != want {
			t.Errorf("search %q returned %d, want %d", q, len(got), want)
		}
	}
	if _, err := svc.SearchUsers(ctx, adminID, "  "); err != ErrInvalidQuery {
		t.Errorf("blank search = %v, want ErrInvalidQuery", err)
	}
	if _, err := svc.SearchUsers(ctx, aliceID, "alice"); err != ErrNotAdmin {
		t.Errorf("non-admin search = %v, want ErrNotAdmin", err)
	}
}

func (f *fakeUsers) SetBirthdayIndex(context.Context, int64, int, int, bool) error { return nil }
func (f *fakeUsers) ListBirthdaysOn(context.Context, int, int) ([]int64, error)    { return nil, nil }
