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
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// Installer is what the `bot` and `spaces.join` scopes need from the spaces module. The
// spaces service implements it; it is injected so this package never constructs one.
type Installer interface {
	BotInstallTargets(ctx context.Context, userID int64) ([]spaces.InstallTarget, error)
	InstallBot(ctx context.Context, actorID, spaceID, botUserID, permissions int64) (granted int64, err error)
	AddMemberViaOAuth(ctx context.Context, actorID, spaceID, userID int64) (added bool, err error)
}

type Service struct {
	repo      Repository
	apps      applications.Repository
	users     auth.UserRepository
	installer Installer
}

func NewService(repo Repository, apps applications.Repository, users auth.UserRepository) *Service {
	return &Service{repo: repo, apps: apps, users: users}
}

// SetInstaller wires the spaces service in; without it the `bot` and `spaces.join` scopes
// are refused as invalid_scope.
func (s *Service) SetInstaller(i Installer) { s.installer = i }

// request is a validated authorize request: the client, the parsed scopes, and - when
// `bot` is among them - the bot account and the requested permission bits.
type request struct {
	App         *applications.Application
	Scopes      []string
	Bot         *auth.User
	Permissions int64
	RedirectURI string
}

func (r *request) wantsBot() bool { return HasScope(r.Scopes, ScopeBot) }

// botOnly is Discord's "just add my bot" shape: scope=bot and nothing else. It is the one
// request that needs no redirect_uri (there is no code to deliver) and issues no token.
func (r *request) botOnly() bool { return len(r.Scopes) == 1 && r.Scopes[0] == ScopeBot }

// validateRequest checks the client, redirect and scopes common to authorize and exchange.
// userID is who is consenting, for the private-bot rule.
func (s *Service) validateRequest(ctx context.Context, userID, clientID int64, redirectURI, rawScope string, perms int64) (*request, error) {
	app, err := s.apps.GetByID(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, ErrInvalidClient
	}
	scopes, err := parseScopes(rawScope)
	if err != nil {
		return nil, err
	}
	req := &request{App: app, Scopes: scopes, RedirectURI: strings.TrimSpace(redirectURI)}
	if req.wantsBot() || HasScope(scopes, ScopeSpacesJoin) {
		if s.installer == nil {
			return nil, ErrInvalidScope
		}
	}
	if req.wantsBot() {
		if !app.HasBot() {
			return nil, ErrNoBot
		}
		if !app.BotPublic && app.OwnerID != userID {
			return nil, ErrBotPrivate
		}
		bot, err := s.users.GetByID(ctx, app.BotUserID)
		if err != nil {
			return nil, err
		}
		if bot == nil {
			return nil, ErrNoBot
		}
		req.Bot = bot
		req.Permissions = perms & permissions.AllSpace
	}
	if req.RedirectURI == "" {
		if !req.botOnly() {
			return nil, ErrInvalidRedirect
		}
	} else if !redirectAllowed(app.RedirectURIs, req.RedirectURI) {
		return nil, ErrInvalidRedirect
	}
	return req, nil
}

// AuthorizeView backs the consent screen: the app, the scopes it asks for and, for a bot
// install, the bot plus the spaces this user could add it to.
type AuthorizeView struct {
	App         *applications.Application
	Scopes      []string
	Bot         *auth.User
	Permissions int64
	Targets     []spaces.InstallTarget
}

func (s *Service) AuthorizeInfo(ctx context.Context, userID, clientID int64, redirectURI, rawScope string, perms int64) (*AuthorizeView, error) {
	req, err := s.validateRequest(ctx, userID, clientID, redirectURI, rawScope, perms)
	if err != nil {
		return nil, err
	}
	view := &AuthorizeView{App: req.App, Scopes: req.Scopes, Bot: req.Bot, Permissions: req.Permissions}
	if req.wantsBot() {
		targets, err := s.installer.BotInstallTargets(ctx, userID)
		if err != nil {
			return nil, err
		}
		view.Targets = targets
	}
	return view, nil
}

// AuthorizeResult is what a consent produced: a single-use code (unless the request was a
// bare bot install) and, when a bot was added, where and with what.
type AuthorizeResult struct {
	Code        string
	SpaceID     int64
	Permissions int64
	Installed   bool
}

// Authorize records the user's consent: installs the bot when asked to, then returns the
// code for the redirect.
func (s *Service) Authorize(ctx context.Context, userID, clientID int64, redirectURI, rawScope string, spaceID, perms int64) (*AuthorizeResult, error) {
	req, err := s.validateRequest(ctx, userID, clientID, redirectURI, rawScope, perms)
	if err != nil {
		return nil, err
	}
	res := &AuthorizeResult{}
	if req.wantsBot() {
		if spaceID == 0 {
			return nil, ErrNoSpace
		}
		granted, err := s.installer.InstallBot(ctx, userID, spaceID, req.Bot.ID, req.Permissions)
		if err != nil {
			return nil, err
		}
		res.SpaceID = spaceID
		res.Permissions = granted
		res.Installed = true
	}
	if req.botOnly() && req.RedirectURI == "" {
		return res, nil
	}
	code, codeHash := newToken()
	if err := s.repo.SaveCode(ctx, codeHash, codeRow{
		UserID: userID, ApplicationID: clientID, Scopes: req.Scopes, RedirectURI: req.RedirectURI, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}
	res.Code = code
	return res, nil
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
	if code == nil || code.ApplicationID != app.ID || code.RedirectURI != strings.TrimSpace(redirectURI) {
		return nil, ErrInvalidGrant
	}
	if time.Since(code.CreatedAt) > CodeTTL {
		return nil, ErrInvalidGrant
	}
	return s.issue(ctx, code.UserID, app.ID, code.Scopes)
}

// Refresh mints a fresh token pair from a refresh token. The old pair is replaced.
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

// issue mints an access+refresh pair and records the grant. One live pair per (user,
// app): the pair a previous authorisation issued is invalidated, so "revoke" in the
// user's Authorized Apps always means the whole application, never just its newest token.
func (s *Service) issue(ctx context.Context, userID, appID int64, scopes []string) (*TokenResult, error) {
	prev, _ := s.repo.GrantTokenHash(ctx, userID, appID)
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
	if prev != "" && prev != accessHash {
		_ = s.repo.DeleteToken(ctx, prev)
	}
	return &TokenResult{AccessToken: access, RefreshToken: refresh, Scopes: scopes, ExpiresIn: int(AccessTTL.Seconds())}, nil
}

// Revoke is RFC 7009: the client hands back an access or refresh token it holds and the
// whole pair dies. Unknown tokens are not an error (there is nothing left to revoke).
func (s *Service) Revoke(ctx context.Context, clientID int64, clientSecret, rawToken string) error {
	app, err := s.authClient(ctx, clientID, clientSecret)
	if err != nil {
		return err
	}
	h := hashToken(rawToken)
	if h == "" {
		return nil
	}
	tokenHash := h
	row, err := s.repo.ResolveToken(ctx, h)
	if err != nil {
		return err
	}
	if row == nil {
		tokenHash, row, err = s.repo.FindByRefresh(ctx, h)
		if err != nil || row == nil {
			return err
		}
	}
	if row.ApplicationID != app.ID {
		return nil
	}
	if err := s.repo.DeleteToken(ctx, tokenHash); err != nil {
		return err
	}
	if cur, _ := s.repo.GrantTokenHash(ctx, row.UserID, app.ID); cur == tokenHash {
		_, _ = s.repo.RevokeGrant(ctx, row.UserID, app.ID)
	}
	return nil
}

// TokenInfo describes a live access token: for GET /oauth2/@me.
type TokenInfo struct {
	App       *applications.Application
	Scopes    []string
	ExpiresAt time.Time
	User      *auth.User
}

func (s *Service) Introspect(ctx context.Context, rawToken string) (*TokenInfo, error) {
	h := hashToken(rawToken)
	if h == "" {
		return nil, ErrInvalidGrant
	}
	row, err := s.repo.ResolveToken(ctx, h)
	if err != nil {
		return nil, err
	}
	if row == nil || time.Now().UTC().After(row.ExpiresAt) {
		return nil, ErrInvalidGrant
	}
	app, err := s.apps.GetByID(ctx, row.ApplicationID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, ErrInvalidClient
	}
	info := &TokenInfo{App: app, Scopes: row.Scopes, ExpiresAt: row.ExpiresAt}
	if HasScope(row.Scopes, ScopeIdentify) || HasScope(row.Scopes, ScopeEmail) {
		info.User, _ = s.users.GetByID(ctx, row.UserID)
	}
	return info, nil
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

// GrantView is one row of the user's Authorized Apps: the grant plus the app it is for
// (nil when the application has since been deleted).
type GrantView struct {
	Grant Grant
	App   *applications.Application
}

func (s *Service) ListGrants(ctx context.Context, userID int64) ([]GrantView, error) {
	grants, err := s.repo.ListGrants(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]GrantView, 0, len(grants))
	for _, g := range grants {
		app, _ := s.apps.GetByID(ctx, g.ApplicationID)
		out = append(out, GrantView{Grant: g, App: app})
	}
	return out, nil
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

// AddSpaceMember is the `spaces.join` scope in action: the caller (a bot with Create
// Invite in the space, typically) adds userID, who authorised it with an access token
// carrying spaces.join. Returns false when they were already a member.
func (s *Service) AddSpaceMember(ctx context.Context, actorID, spaceID, userID int64, rawAccessToken string) (bool, error) {
	if s.installer == nil {
		return false, ErrInvalidScope
	}
	u, scopes, err := s.ResolveToken(ctx, rawAccessToken)
	if err != nil {
		return false, err
	}
	if u == nil || u.ID != userID || !HasScope(scopes, ScopeSpacesJoin) {
		return false, ErrInvalidGrant
	}
	return s.installer.AddMemberViaOAuth(ctx, actorID, spaceID, userID)
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
