package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Service struct {
	repo  Repository
	apps  applications.Repository
	users auth.UserRepository
}

func NewService(repo Repository, apps applications.Repository, users auth.UserRepository) *Service {
	return &Service{repo: repo, apps: apps, users: users}
}

// validateRequest checks the client, redirect and scopes common to authorize and exchange.
func (s *Service) validateRequest(ctx context.Context, clientID int64, redirectURI, rawScope string) (*applications.Application, []string, error) {
	app, err := s.apps.GetByID(ctx, clientID)
	if err != nil {
		return nil, nil, err
	}
	if app == nil {
		return nil, nil, ErrInvalidClient
	}
	if !redirectAllowed(app.RedirectURIs, redirectURI) {
		return nil, nil, ErrInvalidRedirect
	}
	scopes, err := parseScopes(rawScope)
	if err != nil {
		return nil, nil, err
	}
	return app, scopes, nil
}

// AuthorizeInfo backs the consent screen: the app and the scopes it is asking for.
func (s *Service) AuthorizeInfo(ctx context.Context, clientID int64, redirectURI, rawScope string) (*applications.Application, []string, error) {
	return s.validateRequest(ctx, clientID, redirectURI, rawScope)
}

// Authorize records the user's consent and returns a single-use code for the redirect.
func (s *Service) Authorize(ctx context.Context, userID, clientID int64, redirectURI, rawScope string) (string, error) {
	_, scopes, err := s.validateRequest(ctx, clientID, redirectURI, rawScope)
	if err != nil {
		return "", err
	}
	code, codeHash := newToken()
	if err := s.repo.SaveCode(ctx, codeHash, codeRow{
		UserID: userID, ApplicationID: clientID, Scopes: scopes, RedirectURI: redirectURI, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return "", err
	}
	return code, nil
}

// TokenResult is the token endpoint's response.
type TokenResult struct {
	AccessToken  string
	RefreshToken string
	Scopes       []string
	ExpiresIn    int
}

// Exchange trades an authorization code for tokens. The client authenticates with its id and
// secret; the code must match the client and redirect it was issued for.
func (s *Service) Exchange(ctx context.Context, clientID int64, clientSecret, rawCode, redirectURI string) (*TokenResult, error) {
	app, err := s.authClient(ctx, clientID, clientSecret)
	if err != nil {
		return nil, err
	}
	code, err := s.repo.TakeCode(ctx, hashToken(rawCode))
	if err != nil {
		return nil, err
	}
	if code == nil || code.ApplicationID != app.ID || code.RedirectURI != redirectURI {
		return nil, ErrInvalidGrant
	}
	if time.Since(code.CreatedAt) > CodeTTL {
		return nil, ErrInvalidGrant
	}
	return s.issue(ctx, code.UserID, app.ID, code.Scopes)
}

// Refresh mints a fresh access token from a refresh token. The old access token is replaced.
func (s *Service) Refresh(ctx context.Context, clientID int64, clientSecret, rawRefresh string) (*TokenResult, error) {
	app, err := s.authClient(ctx, clientID, clientSecret)
	if err != nil {
		return nil, err
	}
	oldHash, row, err := s.repo.FindByRefresh(ctx, hashToken(rawRefresh))
	if err != nil {
		return nil, err
	}
	if row == nil || row.ApplicationID != app.ID {
		return nil, ErrInvalidGrant
	}
	_ = s.repo.DeleteToken(ctx, oldHash)
	return s.issue(ctx, row.UserID, app.ID, row.Scopes)
}

func (s *Service) issue(ctx context.Context, userID, appID int64, scopes []string) (*TokenResult, error) {
	access, accessHash := newToken()
	refresh, refreshHash := newToken()
	exp := time.Now().UTC().Add(AccessTTL)
	if err := s.repo.SaveToken(ctx, accessHash, tokenRow{
		UserID: userID, ApplicationID: appID, Scopes: scopes, RefreshHash: refreshHash, ExpiresAt: exp,
	}); err != nil {
		return nil, err
	}
	if err := s.repo.SaveGrant(ctx, userID, appID, scopes, accessHash, time.Now().UTC()); err != nil {
		return nil, err
	}
	return &TokenResult{AccessToken: access, RefreshToken: refresh, Scopes: scopes, ExpiresIn: int(AccessTTL.Seconds())}, nil
}

// ResolveToken is the auth middleware's OAuthResolver: it returns the account and granted
// scopes for a valid, unexpired access token.
func (s *Service) ResolveToken(ctx context.Context, rawToken string) (*auth.User, []string, error) {
	h := hashToken(rawToken)
	if h == "" {
		return nil, nil, nil
	}
	row, err := s.repo.ResolveToken(ctx, h)
	if err != nil || row == nil {
		return nil, nil, err
	}
	if time.Now().UTC().After(row.ExpiresAt) {
		return nil, nil, nil
	}
	u, err := s.users.GetByID(ctx, row.UserID)
	if err != nil || u == nil {
		return nil, nil, err
	}
	return u, row.Scopes, nil
}

func (s *Service) ListGrants(ctx context.Context, userID int64) ([]Grant, error) {
	return s.repo.ListGrants(ctx, userID)
}

func (s *Service) RevokeGrant(ctx context.Context, userID, appID int64) error {
	tokenHash, err := s.repo.RevokeGrant(ctx, userID, appID)
	if err != nil {
		return err
	}
	if tokenHash != "" {
		_ = s.repo.DeleteToken(ctx, tokenHash)
	}
	return nil
}

// authClient verifies a client id + secret pair.
func (s *Service) authClient(ctx context.Context, clientID int64, clientSecret string) (*applications.Application, error) {
	app, err := s.apps.GetByID(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if app == nil || app.SecretHash == "" {
		return nil, ErrInvalidClient
	}
	if subtle.ConstantTimeCompare([]byte(hashToken(clientSecret)), []byte(app.SecretHash)) != 1 {
		return nil, ErrInvalidClient
	}
	return app, nil
}

// ---- helpers ----

func parseScopes(raw string) ([]string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 || len(fields) > MaxScopes {
		return nil, ErrInvalidScope
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if !knownScope(f) {
			return nil, ErrInvalidScope
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out, nil
}

func redirectAllowed(allowed []string, uri string) bool {
	for _, a := range allowed {
		if a == uri {
			return true
		}
	}
	return false
}

func newToken() (raw, hash string) {
	b := make([]byte, TokenBytes)
	_, _ = rand.Read(b)
	raw = hex.EncodeToString(b)
	return raw, hashToken(raw)
}

func hashToken(raw string) string {
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HasScope reports whether scopes contains want.
func HasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
