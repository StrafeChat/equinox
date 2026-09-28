package applications

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var applicationsTable = table.New(table.Metadata{
	Name:    "applications",
	Columns: []string{"id", "owner_id", "name", "description", "icon", "secret_hash", "bot_user_id", "redirect_uris", "created_at", "updated_at"},
	PartKey: []string{"id"},
})

var applicationsByOwnerTable = table.New(table.Metadata{
	Name:    "applications_by_owner",
	Columns: []string{"owner_id", "id", "created_at"},
	PartKey: []string{"owner_id"},
	SortKey: []string{"id"},
})

// Repository stores applications and the two token lookup tables. OAuth grant tables are in
// the oauth package's repository, kept separate so each module owns its own storage.
type Repository interface {
	Create(ctx context.Context, a *Application) error
	GetByID(ctx context.Context, id int64) (*Application, error)
	ListByOwner(ctx context.Context, ownerID int64) ([]Application, error)
	Update(ctx context.Context, a *Application) error
	Delete(ctx context.Context, id, ownerID int64) error

	// Bot tokens: one live token per bot, hashed. SetBotToken replaces any previous one.
	SetBotToken(ctx context.Context, appID, botUserID int64, tokenHash string) error
	ResolveBotToken(ctx context.Context, tokenHash string) (botUserID, appID int64, err error)
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) Create(ctx context.Context, a *Application) error {
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := applicationsTable.Insert()
	batch.Query(stmt, a.ID, a.OwnerID, a.Name, a.Description, a.Icon, a.SecretHash, a.BotUserID, a.RedirectURIs, a.CreatedAt, a.UpdatedAt)
	stmt, _ = applicationsByOwnerTable.Insert()
	batch.Query(stmt, a.OwnerID, a.ID, a.CreatedAt)
	return r.session.ExecuteBatch(batch)
}

func (r *repo) GetByID(ctx context.Context, id int64) (*Application, error) {
	stmt, names := applicationsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var a Application
	if err := q.Bind(id).GetRelease(&a); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (r *repo) ListByOwner(ctx context.Context, ownerID int64) ([]Application, error) {
	var ids []struct {
		ID int64 `db:"id"`
	}
	q := r.session.Query("SELECT id FROM applications_by_owner WHERE owner_id = ?", []string{"owner_id"}).WithContext(ctx)
	if err := q.Bind(ownerID).SelectRelease(&ids); err != nil && err != gocql.ErrNotFound {
		return nil, err
	}
	out := make([]Application, 0, len(ids))
	for _, row := range ids {
		a, err := r.GetByID(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		if a != nil {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (r *repo) Update(ctx context.Context, a *Application) error {
	stmt, names := applicationsTable.Update("name", "description", "icon", "secret_hash", "bot_user_id", "redirect_uris", "updated_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(a.Name, a.Description, a.Icon, a.SecretHash, a.BotUserID, a.RedirectURIs, a.UpdatedAt, a.ID).Exec()
}

func (r *repo) Delete(ctx context.Context, id, ownerID int64) error {
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query("DELETE FROM applications WHERE id = ?", id)
	batch.Query("DELETE FROM applications_by_owner WHERE owner_id = ? AND id = ?", ownerID, id)
	batch.Query("DELETE FROM bot_token_by_app WHERE application_id = ?", id)
	return r.session.ExecuteBatch(batch)
}

func (r *repo) SetBotToken(ctx context.Context, appID, botUserID int64, tokenHash string) error {
	// Drop the previous token for this app so a reset invalidates the old one.
	var prev string
	err := r.session.Query("SELECT token_hash FROM bot_token_by_app WHERE application_id = ?", []string{"application_id"}).
		WithContext(ctx).Bind(appID).GetRelease(&prev)
	if err != nil && err != gocql.ErrNotFound {
		return err
	}
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	if prev != "" {
		batch.Query("DELETE FROM bot_tokens WHERE token_hash = ?", prev)
	}
	batch.Query("INSERT INTO bot_tokens (token_hash, bot_user_id, application_id, created_at) VALUES (?, ?, ?, ?)",
		tokenHash, botUserID, appID, time.Now().UTC())
	batch.Query("INSERT INTO bot_token_by_app (application_id, token_hash) VALUES (?, ?)", appID, tokenHash)
	return r.session.ExecuteBatch(batch)
}

func (r *repo) ResolveBotToken(ctx context.Context, tokenHash string) (int64, int64, error) {
	var row struct {
		BotUserID     int64 `db:"bot_user_id"`
		ApplicationID int64 `db:"application_id"`
	}
	err := r.session.Query("SELECT bot_user_id, application_id FROM bot_tokens WHERE token_hash = ?", []string{"token_hash"}).
		WithContext(ctx).Bind(tokenHash).GetRelease(&row)
	if err == gocql.ErrNotFound {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return row.BotUserID, row.ApplicationID, nil
}
