package auth

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

type SessionRepository interface {
	Create(ctx context.Context, s *Session) error
	GetByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	ListByUser(ctx context.Context, userID int64) ([]Session, error)
	Revoke(ctx context.Context, userID, sessionID int64) error
	RevokeAllForUser(ctx context.Context, userID int64) error
}

var sessionsByUserTable = table.New(table.Metadata{
	Name:    "sessions_by_user",
	Columns: []string{"user_id", "session_id", "token_hash", "created_at", "expires_at", "ip_address", "user_agent", "device_name", "revoked_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"session_id"},
})

var sessionsByTokenTable = table.New(table.Metadata{
	Name:    "sessions_by_token",
	Columns: []string{"token_hash", "user_id", "session_id", "created_at", "expires_at", "ip_address", "user_agent", "device_name", "revoked_at"},
	PartKey: []string{"token_hash"},
})

type scyllaSessionRepo struct {
	session gocqlx.Session
}

func NewSessionRepository(session gocqlx.Session) SessionRepository {
	return &scyllaSessionRepo{session: session}
}

func (r *scyllaSessionRepo) Create(ctx context.Context, s *Session) error {
	b := r.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	stmt1, _ := sessionsByUserTable.Insert()
	b.Query(stmt1, s.UserID, s.SessionID, s.TokenHash, s.CreatedAt, s.ExpiresAt, s.IPAddress, s.UserAgent, s.DeviceName, s.RevokedAt)
	stmt2, _ := sessionsByTokenTable.Insert()
	b.Query(stmt2, s.TokenHash, s.UserID, s.SessionID, s.CreatedAt, s.ExpiresAt, s.IPAddress, s.UserAgent, s.DeviceName, s.RevokedAt)
	return r.session.ExecuteBatch(b)
}

func (r *scyllaSessionRepo) GetByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	var s Session
	stmt, names := sessionsByTokenTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(tokenHash).GetRelease(&s); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *scyllaSessionRepo) ListByUser(ctx context.Context, userID int64) ([]Session, error) {
	stmt, names := sessionsByUserTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID).Iter()
	defer iter.Close()

	var out []Session
	var row Session
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *scyllaSessionRepo) Revoke(ctx context.Context, userID, sessionID int64) error {
	now := time.Now().UTC()

	stmt, names := sessionsByUserTable.Update("revoked_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	if err := q.Bind(now, userID, sessionID).ExecRelease(); err != nil {
		return err
	}

	var s Session
	stmt, names = sessionsByUserTable.Get()
	q = r.session.Query(stmt, names).WithContext(ctx)
	if err := q.Bind(userID, sessionID).GetRelease(&s); err != nil {
		if err == gocql.ErrNotFound {
			return nil
		}
		return err
	}

	stmt, names = sessionsByTokenTable.Update("revoked_at")
	q = r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(now, s.TokenHash).ExecRelease()
}

func (r *scyllaSessionRepo) RevokeAllForUser(ctx context.Context, userID int64) error {
	sessions, err := r.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.RevokedAt.IsZero() {
			if err := r.Revoke(ctx, userID, s.SessionID); err != nil {
				return err
			}
		}
	}
	return nil
}
