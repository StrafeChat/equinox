package oauth

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
)

type codeRow struct {
	UserID        int64
	ApplicationID int64
	Scopes        []string
	RedirectURI   string
	CreatedAt     time.Time
	// PKCE (RFC 7636): set when the authorization request carried a code_challenge; the
	// token exchange must then present the matching code_verifier.
	CodeChallenge       string
	CodeChallengeMethod string
}

type tokenRow struct {
	UserID        int64
	ApplicationID int64
	Scopes        []string
	RefreshHash   string
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

type Repository interface {
	SaveCode(ctx context.Context, codeHash string, r codeRow) error
	// TakeCode reads and deletes a code (single use); returns nil if absent.
	TakeCode(ctx context.Context, codeHash string) (*codeRow, error)

	SaveToken(ctx context.Context, tokenHash string, r tokenRow) error
	ResolveToken(ctx context.Context, tokenHash string) (*tokenRow, error)
	FindByRefresh(ctx context.Context, refreshHash string) (tokenHash string, r *tokenRow, err error)
	// DeleteToken removes an access token and its refresh index entry.
	DeleteToken(ctx context.Context, tokenHash string) error

	SaveGrant(ctx context.Context, userID, appID int64, scopes []string, tokenHash string, created time.Time) error
	ListGrants(ctx context.Context, userID int64) ([]Grant, error)
	// GrantTokenHash returns the access token currently recorded for a (user, app) grant,
	// or "" when there is none.
	GrantTokenHash(ctx context.Context, userID, appID int64) (string, error)
	RevokeGrant(ctx context.Context, userID, appID int64) (tokenHash string, err error)
}

type repo struct{ session gocqlx.Session }

func NewRepository(session gocqlx.Session) Repository { return &repo{session: session} }

func (r *repo) SaveCode(ctx context.Context, codeHash string, c codeRow) error {
	return r.session.Query(
		"INSERT INTO oauth_codes (code_hash, user_id, application_id, scopes, redirect_uri, created_at, code_challenge, code_challenge_method) VALUES (?, ?, ?, ?, ?, ?, ?, ?) USING TTL ?",
		nil).WithContext(ctx).
		Bind(codeHash, c.UserID, c.ApplicationID, c.Scopes, c.RedirectURI, c.CreatedAt, c.CodeChallenge, c.CodeChallengeMethod, int(CodeTTL.Seconds())).ExecRelease()
}

func (r *repo) TakeCode(ctx context.Context, codeHash string) (*codeRow, error) {
	var c codeRow
	err := r.session.Query("SELECT user_id, application_id, scopes, redirect_uri, created_at, code_challenge, code_challenge_method FROM oauth_codes WHERE code_hash = ?", nil).
		WithContext(ctx).Bind(codeHash).Scan(&c.UserID, &c.ApplicationID, &c.Scopes, &c.RedirectURI, &c.CreatedAt, &c.CodeChallenge, &c.CodeChallengeMethod)
	if err == gocql.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = r.session.Query("DELETE FROM oauth_codes WHERE code_hash = ?", nil).WithContext(ctx).Bind(codeHash).ExecRelease()
	return &c, nil
}

func (r *repo) SaveToken(ctx context.Context, tokenHash string, t tokenRow) error {
	now := time.Now().UTC()
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query(
		"INSERT INTO oauth_tokens (token_hash, user_id, application_id, scopes, refresh_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		tokenHash, t.UserID, t.ApplicationID, t.Scopes, t.RefreshHash, t.ExpiresAt, now)
	batch.Query("INSERT INTO oauth_tokens_by_refresh (refresh_hash, token_hash) VALUES (?, ?)", t.RefreshHash, tokenHash)
	return r.session.ExecuteBatch(batch)
}

func (r *repo) ResolveToken(ctx context.Context, tokenHash string) (*tokenRow, error) {
	var t tokenRow
	err := r.session.Query("SELECT user_id, application_id, scopes, refresh_hash, expires_at, created_at FROM oauth_tokens WHERE token_hash = ?", nil).
		WithContext(ctx).Bind(tokenHash).Scan(&t.UserID, &t.ApplicationID, &t.Scopes, &t.RefreshHash, &t.ExpiresAt, &t.CreatedAt)
	if err == gocql.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *repo) FindByRefresh(ctx context.Context, refreshHash string) (string, *tokenRow, error) {
	var th string
	err := r.session.Query("SELECT token_hash FROM oauth_tokens_by_refresh WHERE refresh_hash = ?", nil).
		WithContext(ctx).Bind(refreshHash).Scan(&th)
	if err == gocql.ErrNotFound {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	t, err := r.ResolveToken(ctx, th)
	if err != nil || t == nil {
		return "", nil, err
	}
	return th, t, nil
}

func (r *repo) DeleteToken(ctx context.Context, tokenHash string) error {
	t, err := r.ResolveToken(ctx, tokenHash)
	if err != nil {
		return err
	}
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query("DELETE FROM oauth_tokens WHERE token_hash = ?", tokenHash)
	if t != nil && t.RefreshHash != "" {
		batch.Query("DELETE FROM oauth_tokens_by_refresh WHERE refresh_hash = ?", t.RefreshHash)
	}
	return r.session.ExecuteBatch(batch)
}

func (r *repo) SaveGrant(ctx context.Context, userID, appID int64, scopes []string, tokenHash string, created time.Time) error {
	return r.session.Query(
		"INSERT INTO oauth_grants_by_user (user_id, application_id, scopes, token_hash, created_at) VALUES (?, ?, ?, ?, ?)",
		nil).WithContext(ctx).Bind(userID, appID, scopes, tokenHash, created).ExecRelease()
}

func (r *repo) ListGrants(ctx context.Context, userID int64) ([]Grant, error) {
	var out []Grant
	iter := r.session.Query("SELECT application_id, scopes, created_at FROM oauth_grants_by_user WHERE user_id = ?", nil).WithContext(ctx).Bind(userID).Iter()
	var g Grant
	for iter.Scan(&g.ApplicationID, &g.Scopes, &g.CreatedAt) {
		out = append(out, g)
		g = Grant{}
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) GrantTokenHash(ctx context.Context, userID, appID int64) (string, error) {
	var th string
	err := r.session.Query("SELECT token_hash FROM oauth_grants_by_user WHERE user_id = ? AND application_id = ?", nil).
		WithContext(ctx).Bind(userID, appID).Scan(&th)
	if err == gocql.ErrNotFound {
		return "", nil
	}
	return th, err
}

func (r *repo) RevokeGrant(ctx context.Context, userID, appID int64) (string, error) {
	th, err := r.GrantTokenHash(ctx, userID, appID)
	if err != nil {
		return "", err
	}
	_ = r.session.Query("DELETE FROM oauth_grants_by_user WHERE user_id = ? AND application_id = ?", nil).WithContext(ctx).Bind(userID, appID).ExecRelease()
	return th, nil
}
