package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/mail"
)

// memUsers is an in-memory UserRepository with the LWT semantics the Scylla one has: one
// users_by_email index, a claim that fails when taken, a row update conditional on the old
// address and a release conditional on the claim still being the account's. GetByEmail
// reads the index (uncached, like the real one); GetByID can be told to serve a stale row,
// the way a cache-aside entry from before a change would.
type memUsers struct {
	mu      sync.Mutex
	rows    map[int64]*User
	index   map[string]int64
	stale   map[int64]*User
	changes int
}

func newMemUsers() *memUsers {
	return &memUsers{rows: map[int64]*User{}, index: map[string]int64{}, stale: map[int64]*User{}}
}

func (m *memUsers) add(u *User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *u
	m.rows[u.ID] = &cp
	if u.Email != "" {
		m.index[u.Email] = u.ID
	}
}

func (m *memUsers) row(id int64) *User {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u := m.rows[id]; u != nil {
		cp := *u
		return &cp
	}
	return nil
}

func (m *memUsers) changeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changes
}

func (m *memUsers) Create(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.index[u.Email]; ok {
		return ErrEmailInUse
	}
	cp := *u
	m.rows[u.ID] = &cp
	m.index[u.Email] = u.ID
	return nil
}

func (m *memUsers) GetByID(_ context.Context, id int64) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u := m.stale[id]; u != nil {
		cp := *u
		return &cp, nil
	}
	if u := m.rows[id]; u != nil {
		cp := *u
		return &cp, nil
	}
	return nil, nil
}

func (m *memUsers) GetByIDs(ctx context.Context, ids []int64) ([]*User, error) {
	out := make([]*User, len(ids))
	for i, id := range ids {
		out[i], _ = m.GetByID(ctx, id)
	}
	return out, nil
}

func (m *memUsers) GetByEmail(_ context.Context, email string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.index[email]
	if !ok {
		return nil, nil
	}
	if u := m.rows[id]; u != nil {
		cp := *u
		return &cp, nil
	}
	return nil, nil
}

func (m *memUsers) GetByUsername(_ context.Context, name string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.rows {
		if strings.EqualFold(u.Username, name) {
			cp := *u
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *memUsers) EmailExists(_ context.Context, email string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.index[email]
	return ok, nil
}

func (m *memUsers) UsernameTaken(ctx context.Context, name string) (bool, error) {
	u, _ := m.GetByUsername(ctx, name)
	return u != nil, nil
}

func (m *memUsers) UpdateRelationships(context.Context, int64, []int64, []int64) error { return nil }
func (m *memUsers) UpdateBlocks(context.Context, int64, []int64, []int64) error        { return nil }
func (m *memUsers) UpdateProfile(ctx context.Context, id int64, _ *ProfileUpdate) (*User, error) {
	return m.GetByID(ctx, id)
}
func (m *memUsers) SetBirthdayIndex(context.Context, int64, int, int, bool) error { return nil }
func (m *memUsers) ListBirthdaysOn(context.Context, int, int) ([]int64, error)    { return nil, nil }
func (m *memUsers) SetTOTP(context.Context, int64, string, bool) error            { return nil }

func (m *memUsers) SetEmailVerified(_ context.Context, id int64, verified bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u := m.rows[id]; u != nil {
		u.VerifiedEmail = verified
	}
	return nil
}

func (m *memUsers) SetPassword(_ context.Context, id int64, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u := m.rows[id]; u != nil {
		u.PasswordHash = hash
	}
	return nil
}

func (m *memUsers) ChangeEmail(_ context.Context, id int64, oldEmail, newEmail string, verified bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.EqualFold(oldEmail, newEmail) {
		return ErrEmailUnchanged
	}
	if _, taken := m.index[newEmail]; taken {
		return ErrEmailInUse
	}
	m.index[newEmail] = id
	u := m.rows[id]
	if u == nil || u.Email != oldEmail {
		if m.index[newEmail] == id {
			delete(m.index, newEmail)
		}
		return ErrEmailChangeConflict
	}
	u.Email, u.VerifiedEmail = newEmail, verified
	if oldEmail != "" && m.index[oldEmail] == id {
		delete(m.index, oldEmail)
	}
	m.changes++
	return nil
}

func (m *memUsers) UpsertShadow(context.Context, *User) error { return nil }
func (m *memUsers) GetByRemote(context.Context, string, int64) (*User, error) {
	return nil, nil
}

// memTwoFactor holds TOTP state, recovery codes (by hash) and a passkey count per user.
type memTwoFactor struct {
	mu       sync.Mutex
	totp     map[int64]string // user -> secret ciphertext, present when enabled
	recovery map[int64]map[string]bool
	passkeys map[int64]int
}

func newMemTwoFactor() *memTwoFactor {
	return &memTwoFactor{totp: map[int64]string{}, recovery: map[int64]map[string]bool{}, passkeys: map[int64]int{}}
}

func (f *memTwoFactor) GetTOTPState(_ context.Context, id int64) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.totp[id]
	return ok, c, nil
}

func (f *memTwoFactor) CreateRecoveryCodes(_ context.Context, id int64, hashes []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := map[string]bool{}
	for _, h := range hashes {
		set[h] = false
	}
	f.recovery[id] = set
	return nil
}

func (f *memTwoFactor) CountUnusedRecoveryCodes(_ context.Context, id int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, used := range f.recovery[id] {
		if !used {
			n++
		}
	}
	return n, nil
}

func (f *memTwoFactor) ConsumeRecoveryCode(_ context.Context, id int64, hash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	used, ok := f.recovery[id][hash]
	if !ok || used {
		return false, nil
	}
	f.recovery[id][hash] = true
	return true, nil
}

func (f *memTwoFactor) CreateWebAuthnCredential(context.Context, *WebAuthnCredentialRow) error {
	return nil
}

func (f *memTwoFactor) ListWebAuthnCredentials(_ context.Context, id int64) ([]WebAuthnCredentialRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return make([]WebAuthnCredentialRow, f.passkeys[id]), nil
}

func (f *memTwoFactor) GetWebAuthnCredential(context.Context, int64, string) (*WebAuthnCredentialRow, error) {
	return nil, nil
}
func (f *memTwoFactor) UpdateWebAuthnCredential(context.Context, int64, string, []byte, time.Time) error {
	return nil
}
func (f *memTwoFactor) DeleteWebAuthnCredential(context.Context, int64, string) error { return nil }

type memSessions struct {
	mu      sync.Mutex
	revoked []int64
}

func (s *memSessions) Create(context.Context, *Session) error { return nil }
func (s *memSessions) GetByTokenHash(context.Context, string) (*Session, error) {
	return nil, nil
}
func (s *memSessions) GetByUserSession(context.Context, int64, int64) (*Session, error) {
	return nil, nil
}
func (s *memSessions) ListByUser(context.Context, int64) ([]Session, error) { return nil, nil }
func (s *memSessions) Revoke(context.Context, int64, int64) error           { return nil }
func (s *memSessions) RevokeAllForUser(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = append(s.revoked, id)
	return nil
}

// memMailer records what was sent. Mail goes out on its own goroutine (safego.Go), so tests
// wait for it with waitFor.
type memMailer struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (m *memMailer) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *memMailer) to(addr string) []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mail.Message
	for _, msg := range m.sent {
		if msg.To == addr {
			out = append(out, msg)
		}
	}
	return out
}

// waitFor returns the messages to addr once there are at least n, failing after 2 s.
func (m *memMailer) waitFor(t *testing.T, addr string, n int) []mail.Message {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := m.to(addr); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d mail(s) to %s, have %d", n, addr, len(m.to(addr)))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
