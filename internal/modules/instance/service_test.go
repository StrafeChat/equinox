package instance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
)

// fakeRepo is an in-memory Repository whose TakeInviteUse has the same compare-and-set
// semantics as the Scylla one, so the tests exercise the real contention logic.
type fakeRepo struct {
	mu      sync.Mutex
	invites map[string]*Invite
	state   *int64
	admins  map[int64]bool
	// usersExist stands in for a keyspace that predates instance invites.
	usersExist bool
	claimed    map[string]bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{invites: map[string]*Invite{}, admins: map[int64]bool{}, claimed: map[string]bool{}}
}

func (f *fakeRepo) CreateInvite(_ context.Context, inv *Invite) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *inv
	f.invites[inv.Code] = &cp
	return nil
}

func (f *fakeRepo) GetInviteByCode(_ context.Context, code string) (*Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv, ok := f.invites[code]
	if !ok {
		return nil, nil
	}
	cp := *inv
	return &cp, nil
}

func (f *fakeRepo) ListInvites(_ context.Context) ([]Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Invite, 0, len(f.invites))
	for _, inv := range f.invites {
		out = append(out, *inv)
	}
	return out, nil
}

func (f *fakeRepo) DeleteInvite(_ context.Context, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.invites, code)
	return nil
}

func (f *fakeRepo) TakeInviteUse(_ context.Context, code string, from int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv, ok := f.invites[code]
	if !ok || inv.Uses != from {
		return false, nil
	}
	inv.Uses = from + 1
	return true, nil
}

func (f *fakeRepo) ClaimBootstrap(_ context.Context, userID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != nil {
		return false, nil
	}
	f.state = &userID
	return true, nil
}

func (f *fakeRepo) ReleaseBootstrap(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = nil
	return nil
}

func (f *fakeRepo) IsBootstrapped(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state != nil, nil
}

func (f *fakeRepo) SetInstanceAdmin(_ context.Context, userID int64, admin bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admins[userID] = admin
	return nil
}

func (f *fakeRepo) IsInstanceAdmin(_ context.Context, userID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admins[userID], nil
}

func (f *fakeRepo) AnyUserExists(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usersExist, nil
}

func (f *fakeRepo) ClaimDataMigration(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed[name] {
		return false, nil
	}
	f.claimed[name] = true
	return true, nil
}

func (f *fakeRepo) ReleaseDataMigration(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claimed, name)
	return nil
}

func newTestService(repo Repository, admins ...int64) *Service {
	cfg := &config.Config{}
	cfg.Flags.InstanceAdmins = admins
	return NewService(cfg, repo)
}

// A single-use invite must admit exactly one account even when many people redeem it at
// the same instant - the whole point of the compare-and-set.
func TestConsumeSingleUseAdmitsExactlyOne(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	if err := repo.CreateInvite(context.Background(), &Invite{Code: "once", MaxUses: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	const racers = 25
	var wg sync.WaitGroup
	results := make([]bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := svc.Consume(context.Background(), "once")
			if err != nil {
				t.Errorf("Consume: %v", err)
			}
			results[i] = ok
		}(i)
	}
	wg.Wait()

	admitted := 0
	for _, ok := range results {
		if ok {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d accounts on a single-use invite, want exactly 1", admitted)
	}
}

func TestConsumeRespectsMaxUses(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	_ = repo.CreateInvite(context.Background(), &Invite{Code: "three", MaxUses: 3})
	admitted := 0
	for i := 0; i < 10; i++ {
		if ok, _ := svc.Consume(context.Background(), "three"); ok {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("admitted %d, want 3", admitted)
	}
}

func TestConsumeUnlimitedInviteKeepsWorking(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	_ = repo.CreateInvite(context.Background(), &Invite{Code: "open", MaxUses: 0})
	for i := 0; i < 50; i++ {
		ok, err := svc.Consume(context.Background(), "open")
		if err != nil || !ok {
			t.Fatalf("use %d: ok=%v err=%v, want an unlimited invite to keep admitting", i, ok, err)
		}
	}
}

func TestConsumeRejectsExpiredMissingAndBlank(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	past := time.Now().UTC().Add(-time.Minute)
	_ = repo.CreateInvite(context.Background(), &Invite{Code: "stale", ExpiresAt: &past})

	for _, code := range []string{"stale", "nosuchcode", "", "   "} {
		ok, err := svc.Consume(context.Background(), code)
		if err != nil {
			t.Fatalf("Consume(%q): unexpected error %v", code, err)
		}
		if ok {
			t.Errorf("Consume(%q) admitted someone, want refused", code)
		}
	}
}

// INSTANCE_ADMINS is the documented way back in for an operator who lost the first
// account, so it must work without any database row saying so.
func TestIsAdminFromEnvironmentWithoutDatabaseRow(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo, 4242)
	if ok, _ := svc.IsAdmin(context.Background(), 4242); !ok {
		t.Error("INSTANCE_ADMINS entry is not an admin")
	}
	if ok, _ := svc.IsAdmin(context.Background(), 99); ok {
		t.Error("a stranger is an admin")
	}
}

func TestOnlyAdminsMintAndRevokeInvites(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo, 1)
	if _, err := svc.CreateInvite(context.Background(), 2, nil); err != ErrNotAdmin {
		t.Fatalf("non-admin CreateInvite = %v, want ErrNotAdmin", err)
	}
	inv, err := svc.CreateInvite(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("admin CreateInvite: %v", err)
	}
	if err := svc.RevokeInvite(context.Background(), 2, inv.Code); err != ErrNotAdmin {
		t.Fatalf("non-admin RevokeInvite = %v, want ErrNotAdmin", err)
	}
	if err := svc.RevokeInvite(context.Background(), 1, inv.Code); err != nil {
		t.Fatalf("admin RevokeInvite: %v", err)
	}
	if ok, _ := svc.CheckInvite(context.Background(), inv.Code); ok {
		t.Error("revoked invite still checks out as valid")
	}
}

func TestCreateInviteRejectsOutOfRangeLimits(t *testing.T) {
	svc := newTestService(newFakeRepo(), 1)
	for _, in := range []*CreateInviteInput{
		{MaxAgeSeconds: MaxInviteAgeSeconds + 1},
		{MaxUses: MaxInviteUses + 1},
		{MaxUses: -1},
	} {
		if _, err := svc.CreateInvite(context.Background(), 1, in); err != ErrInvalidInvite {
			t.Errorf("CreateInvite(%+v) = %v, want ErrInvalidInvite", in, err)
		}
	}
}

// Only one account can be the instance's first, however many register at once.
func TestClaimBootstrapAdmitsOneWinner(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	var wg sync.WaitGroup
	won := make([]bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _ := svc.ClaimBootstrap(context.Background(), int64(i+1))
			won[i] = ok
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, ok := range won {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d accounts claimed the instance, want exactly 1", winners)
	}
}

// The upgrade case: an instance that already had accounts must not hand the next person to
// register its administrator bit.
func TestSealBootstrapClosesWindowOnAnInstanceWithAccounts(t *testing.T) {
	repo := newFakeRepo()
	repo.usersExist = true
	svc := newTestService(repo)

	if err := svc.SealBootstrapOnExistingInstance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.IsBootstrapped(context.Background()); !ok {
		t.Fatal("window left open on an instance that already had accounts")
	}
	if claimed, _ := svc.ClaimBootstrap(context.Background(), 777); claimed {
		t.Error("a later registration still claimed the instance")
	}
}

// A genuinely empty instance must keep the window open, or the operator can never make
// their own first account on an invite-only deployment.
func TestSealBootstrapLeavesEmptyInstanceOpen(t *testing.T) {
	repo := newFakeRepo()
	repo.usersExist = false
	svc := newTestService(repo)

	if err := svc.SealBootstrapOnExistingInstance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.IsBootstrapped(context.Background()); ok {
		t.Fatal("window closed on an empty instance")
	}
	if claimed, _ := svc.ClaimBootstrap(context.Background(), 777); !claimed {
		t.Error("the operator could not claim their own new instance")
	}
}
