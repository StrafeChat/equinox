package messages

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var messagesTable = table.New(table.Metadata{
	Name:    "messages",
	Columns: []string{"room_id", "id", "sender_id", "sender_device_id", "ciphertext", "reply_to_id", "created_at", "updated_at", "deleted_at"},
	PartKey: []string{"room_id"},
	SortKey: []string{"id"},
})

type Repository interface {
	Create(ctx context.Context, m *Message) error
	GetByID(ctx context.Context, roomID, msgID int64) (*Message, error)
	List(ctx context.Context, roomID int64, beforeID *int64, limit int) ([]Message, error)
	Update(ctx context.Context, roomID, msgID int64, ciphertext string) (*Message, error)
	SoftDelete(ctx context.Context, roomID, msgID int64) error
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) Create(ctx context.Context, m *Message) error {
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	stmt, names := messagesTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(m).ExecRelease()
}

func (r *repo) GetByID(ctx context.Context, roomID, msgID int64) (*Message, error) {
	var m Message
	stmt, names := messagesTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(roomID, msgID).GetRelease(&m); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *repo) List(ctx context.Context, roomID int64, beforeID *int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	stmt, names := messagesTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx).PageSize(limit + 100)
	defer q.Release()
	iter := q.Bind(roomID).Iter()
	defer iter.Close()
	var out []Message
	var row Message
	for iter.StructScan(&row) {
		if beforeID != nil && row.ID >= *beforeID {
			continue
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out, iter.Close()
}

func (r *repo) Update(ctx context.Context, roomID, msgID int64, ciphertext string) (*Message, error) {
	m, err := r.GetByID(ctx, roomID, msgID)
	if err != nil || m == nil {
		return m, err
	}
	if m.DeletedAt != nil && !m.DeletedAt.IsZero() {
		return nil, nil
	}
	now := time.Now().UTC()
	stmt, names := messagesTable.Update("ciphertext", "updated_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	if err := q.Bind(ciphertext, now, roomID, msgID).ExecRelease(); err != nil {
		return nil, err
	}
	m.Ciphertext = ciphertext
	m.UpdatedAt = now
	return m, nil
}

func (r *repo) SoftDelete(ctx context.Context, roomID, msgID int64) error {
	now := time.Now().UTC()
	stmt, names := messagesTable.Update("deleted_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(now, roomID, msgID).ExecRelease()
}
