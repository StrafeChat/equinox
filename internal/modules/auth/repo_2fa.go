package auth

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

// RecoveryCode is one single-use fallback code, stored hashed (see hashRecoveryCode) since
// the server only ever needs to compare, never show it again.
type RecoveryCode struct {
	UserID    int64     `db:"user_id"`
	CodeHash  string    `db:"code_hash"`
	UsedAt    time.Time `db:"used_at"`
	CreatedAt time.Time `db:"created_at"`
}

// WebAuthnCredentialRow is one registered passkey/security key. Credential is the
// go-webauthn library's own Credential struct, JSON-serialized whole - see the migration
// comment for why a single blob is the right shape here.
type WebAuthnCredentialRow struct {
	UserID       int64     `db:"user_id"`
	CredentialID string    `db:"credential_id"`
	Credential   []byte    `db:"credential"`
	Name         string    `db:"name"`
	CreatedAt    time.Time `db:"created_at"`
	LastUsedAt   time.Time `db:"last_used_at"`
}

type TwoFactorRepository interface {
	// GetTOTPState reads totp_enabled/totp_secret directly from Scylla, bypassing
	// CachedUserRepository entirely. This pair of columns is security state, not display
	// data: a cache-aside read racing SetTOTP's write can re-populate the cache with the
	// pre-write snapshot *after* invalidation runs, serving a stale enabled flag or secret
	// for up to the full cache TTL - the wrong direction either way (a false negative
	// locks a real code out of login; a false positive would do worse). Every login-time
	// or settings check of this pair goes through here instead of User.TOTPEnabled/
	// TOTPSecret from a cached GetByID.
	GetTOTPState(ctx context.Context, userID int64) (enabled bool, secretCiphertext string, err error)

	// Recovery codes. CreateRecoveryCodes replaces the whole set (it deletes the partition
	// first) so "regenerate" is one call. ConsumeRecoveryCode marks one used with a
	// lightweight transaction, atomically, so the same code cannot be spent twice by two
	// concurrent requests.
	CreateRecoveryCodes(ctx context.Context, userID int64, hashes []string) error
	CountUnusedRecoveryCodes(ctx context.Context, userID int64) (int, error)
	ConsumeRecoveryCode(ctx context.Context, userID int64, codeHash string) (bool, error)

	CreateWebAuthnCredential(ctx context.Context, row *WebAuthnCredentialRow) error
	ListWebAuthnCredentials(ctx context.Context, userID int64) ([]WebAuthnCredentialRow, error)
	GetWebAuthnCredential(ctx context.Context, userID int64, credentialID string) (*WebAuthnCredentialRow, error)
	UpdateWebAuthnCredential(ctx context.Context, userID int64, credentialID string, credential []byte, lastUsedAt time.Time) error
	DeleteWebAuthnCredential(ctx context.Context, userID int64, credentialID string) error
}

var recoveryCodesTable = table.New(table.Metadata{
	Name:    "recovery_codes_by_user",
	Columns: []string{"user_id", "code_hash", "used_at", "created_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"code_hash"},
})

var webauthnCredentialsTable = table.New(table.Metadata{
	Name:    "webauthn_credentials_by_user",
	Columns: []string{"user_id", "credential_id", "credential", "name", "created_at", "last_used_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"credential_id"},
})

type scyllaTwoFactorRepo struct {
	session gocqlx.Session
}

func NewTwoFactorRepository(session gocqlx.Session) TwoFactorRepository {
	return &scyllaTwoFactorRepo{session: session}
}

func (r *scyllaTwoFactorRepo) GetTOTPState(ctx context.Context, userID int64) (bool, string, error) {
	var enabled bool
	var secret string
	q := r.session.Session.Query("SELECT totp_enabled, totp_secret FROM users WHERE id = ?", userID).WithContext(ctx)
	defer q.Release()
	if err := q.Scan(&enabled, &secret); err != nil {
		if err == gocql.ErrNotFound {
			return false, "", nil
		}
		return false, "", err
	}
	return enabled, secret, nil
}

func (r *scyllaTwoFactorRepo) CreateRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	// A partition-level delete needs no clustering key, so the old set (whatever its size)
	// goes in one statement before the new ten are batched in.
	if err := r.session.Session.Query("DELETE FROM recovery_codes_by_user WHERE user_id = ?", userID).WithContext(ctx).Exec(); err != nil {
		return err
	}
	now := time.Now().UTC()
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := recoveryCodesTable.Insert()
	for _, h := range hashes {
		b.Query(stmt, userID, h, time.Time{}, now)
	}
	return r.session.ExecuteBatch(b)
}

func (r *scyllaTwoFactorRepo) CountUnusedRecoveryCodes(ctx context.Context, userID int64) (int, error) {
	stmt, names := recoveryCodesTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID).Iter()
	defer iter.Close()

	count := 0
	var row RecoveryCode
	for iter.StructScan(&row) {
		if row.UsedAt.IsZero() {
			count++
		}
	}
	return count, iter.Close()
}

// ConsumeRecoveryCode marks a code used only if it exists and is still unused (IF used_at =
// null), the same compare-and-set shape sessions and everything else single-use in this
// codebase already uses to close a check-then-act race.
func (r *scyllaTwoFactorRepo) ConsumeRecoveryCode(ctx context.Context, userID int64, codeHash string) (bool, error) {
	q := r.session.Session.Query(
		"UPDATE recovery_codes_by_user SET used_at = ? WHERE user_id = ? AND code_hash = ? IF used_at = null",
		time.Now().UTC(), userID, codeHash,
	).WithContext(ctx)
	defer q.Release()
	applied, err := q.MapScanCAS(map[string]interface{}{})
	if err != nil {
		return false, err
	}
	return applied, nil
}

func (r *scyllaTwoFactorRepo) CreateWebAuthnCredential(ctx context.Context, row *WebAuthnCredentialRow) error {
	row.CreatedAt = time.Now().UTC()
	stmt, names := webauthnCredentialsTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(row).ExecRelease()
}

func (r *scyllaTwoFactorRepo) ListWebAuthnCredentials(ctx context.Context, userID int64) ([]WebAuthnCredentialRow, error) {
	stmt, names := webauthnCredentialsTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID).Iter()
	defer iter.Close()

	var out []WebAuthnCredentialRow
	var row WebAuthnCredentialRow
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	return out, iter.Close()
}

func (r *scyllaTwoFactorRepo) GetWebAuthnCredential(ctx context.Context, userID int64, credentialID string) (*WebAuthnCredentialRow, error) {
	var row WebAuthnCredentialRow
	stmt, names := webauthnCredentialsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(userID, credentialID).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

func (r *scyllaTwoFactorRepo) UpdateWebAuthnCredential(ctx context.Context, userID int64, credentialID string, credential []byte, lastUsedAt time.Time) error {
	stmt, names := webauthnCredentialsTable.Update("credential", "last_used_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(credential, lastUsedAt, userID, credentialID).ExecRelease()
}

func (r *scyllaTwoFactorRepo) DeleteWebAuthnCredential(ctx context.Context, userID int64, credentialID string) error {
	stmt, names := webauthnCredentialsTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(userID, credentialID).ExecRelease()
}
