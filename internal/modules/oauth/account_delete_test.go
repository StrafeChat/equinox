package oauth

import (
	"context"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// An access token acts for its application: once the application is deleted (by its owner,
// or with its owner's account) the token stops working at once. A deleted account's grants
// are revoked with it.

type memTokens struct {
	Repository
	tokens  map[string]*tokenRow
	grants  map[int64]map[int64]string // user -> app -> token hash
	deleted []string
}

func (m *memTokens) ResolveToken(_ context.Context, h string) (*tokenRow, error) {
	return m.tokens[h], nil
}

func (m *memTokens) DeleteToken(_ context.Context, h string) error {
	delete(m.tokens, h)
	m.deleted = append(m.deleted, h)
	return nil
}

func (m *memTokens) ListGrants(_ context.Context, userID int64) ([]Grant, error) {
	var out []Grant
	for appID := range m.grants[userID] {
		out = append(out, Grant{ApplicationID: appID})
	}
	return out, nil
}

func (m *memTokens) RevokeGrant(_ context.Context, userID, appID int64) (string, error) {
	h := m.grants[userID][appID]
	delete(m.grants[userID], appID)
	return h, nil
}

type memApps struct {
	applications.Repository
	apps map[int64]*applications.Application
}

func (m memApps) GetByID(_ context.Context, appID int64) (*applications.Application, error) {
	return m.apps[appID], nil
}

type memAccountUsers struct{ auth.UserRepository }

func (memAccountUsers) GetByID(_ context.Context, uid int64) (*auth.User, error) {
	return &auth.User{ID: uid, Username: "u"}, nil
}

func TestResolveTokenRefusesDeletedApplication(t *testing.T) {
	ctx := context.Background()
	raw := "0a0b0c0d"
	h := hashToken(raw)
	tokens := &memTokens{tokens: map[string]*tokenRow{
		h: {UserID: 7, ApplicationID: 40, Scopes: []string{ScopeIdentify}, ExpiresAt: time.Now().Add(time.Hour)},
	}}
	apps := memApps{apps: map[int64]*applications.Application{40: {ID: 40, Name: "Helper"}}}
	s := NewService(tokens, apps, memAccountUsers{})

	u, scopes, err := s.ResolveToken(ctx, raw)
	if err != nil || u == nil || u.ID != 7 || len(scopes) != 1 {
		t.Fatalf("live application: %+v %v %v", u, scopes, err)
	}

	delete(apps.apps, 40)
	u, _, err = s.ResolveToken(ctx, raw)
	if err != nil || u != nil {
		t.Fatalf("deleted application still resolves: %+v %v", u, err)
	}
	if _, ok := tokens.tokens[h]; ok {
		t.Error("the dead token row was kept")
	}
}

func TestPurgeUserRevokesEveryGrant(t *testing.T) {
	tokens := &memTokens{
		tokens: map[string]*tokenRow{"h1": {UserID: 7}, "h2": {UserID: 7}, "h3": {UserID: 8}},
		grants: map[int64]map[int64]string{7: {40: "h1", 41: "h2"}, 8: {40: "h3"}},
	}
	s := NewService(tokens, memApps{}, memAccountUsers{})
	if n, err := s.CountGrants(context.Background(), 7); err != nil || n != 2 {
		t.Fatalf("count %d %v", n, err)
	}
	n, err := s.PurgeUser(context.Background(), 7)
	if err != nil || n != 2 {
		t.Fatalf("purge %d %v", n, err)
	}
	if len(tokens.grants[7]) != 0 {
		t.Errorf("grants left %v", tokens.grants[7])
	}
	if _, ok := tokens.tokens["h1"]; ok {
		t.Error("h1 survived")
	}
	if _, ok := tokens.tokens["h2"]; ok {
		t.Error("h2 survived")
	}
	if _, ok := tokens.tokens["h3"]; !ok || len(tokens.grants[8]) != 1 {
		t.Error("someone else's grant went")
	}
}
