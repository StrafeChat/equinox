package relationships

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var recipientTable = table.New(table.Metadata{
	Name:    "relationship_requests_by_recipient",
	Columns: []string{"to_user_id", "from_user_id", "created_at"},
	PartKey: []string{"to_user_id"},
	SortKey: []string{"from_user_id"},
})

var senderTable = table.New(table.Metadata{
	Name:    "relationship_requests_by_sender",
	Columns: []string{"from_user_id", "to_user_id", "created_at"},
	PartKey: []string{"from_user_id"},
	SortKey: []string{"to_user_id"},
})

var nicknamesTable = table.New(table.Metadata{
	Name:    "relationship_nicknames",
	Columns: []string{"user_id", "target_id", "nickname"},
	PartKey: []string{"user_id"},
	SortKey: []string{"target_id"},
})

type Repository interface {
	CreateRequest(ctx context.Context, from, to int64) error
	GetIncoming(ctx context.Context, toUserID int64) ([]Request, error)
	GetOutgoing(ctx context.Context, fromUserID int64) ([]Request, error)
	HasRequest(ctx context.Context, from, to int64) (bool, error)
	DeleteRequest(ctx context.Context, from, to int64) error
	SetNickname(ctx context.Context, userID, targetID int64, nickname string) error
	DeleteNickname(ctx context.Context, userID, targetID int64) error
	GetNicknames(ctx context.Context, userID int64, targetIDs []int64) (map[int64]string, error)
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) CreateRequest(ctx context.Context, from, to int64) error {
	now := time.Now().UTC()

	stmt, names := recipientTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	if err := q.Bind(to, from, now).ExecRelease(); err != nil {
		return err
	}

	stmt, names = senderTable.Insert()
	q = r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(from, to, now).ExecRelease()
}

func (r *repo) GetIncoming(ctx context.Context, toUserID int64) ([]Request, error) {
	stmt, names := recipientTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(toUserID).Iter()
	defer iter.Close()

	var out []Request
	var row IncomingRow
	for iter.StructScan(&row) {
		out = append(out, Request{
			FromUserID: row.FromUserID,
			ToUserID:   row.ToUserID,
			CreatedAt:  row.CreatedAt,
		})
	}
	return out, iter.Close()
}

func (r *repo) GetOutgoing(ctx context.Context, fromUserID int64) ([]Request, error) {
	stmt, names := senderTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(fromUserID).Iter()
	defer iter.Close()

	var out []Request
	var row OutgoingRow
	for iter.StructScan(&row) {
		out = append(out, Request{
			FromUserID: row.FromUserID,
			ToUserID:   row.ToUserID,
			CreatedAt:  row.CreatedAt,
		})
	}
	return out, iter.Close()
}

func (r *repo) HasRequest(ctx context.Context, from, to int64) (bool, error) {
	var row IncomingRow
	stmt, names := recipientTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(to, from).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *repo) DeleteRequest(ctx context.Context, from, to int64) error {
	stmt, names := recipientTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	if err := q.Bind(to, from).ExecRelease(); err != nil {
		return err
	}
	stmt, names = senderTable.Delete()
	q = r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(from, to).ExecRelease()
}

func (r *repo) SetNickname(ctx context.Context, userID, targetID int64, nickname string) error {
	stmt, names := nicknamesTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(userID, targetID, nickname).ExecRelease()
}

func (r *repo) DeleteNickname(ctx context.Context, userID, targetID int64) error {
	stmt, names := nicknamesTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(userID, targetID).ExecRelease()
}

type NicknameRow struct {
	UserID   int64  `db:"user_id"`
	TargetID int64  `db:"target_id"`
	Nickname string `db:"nickname"`
}

func (r *repo) GetNicknames(ctx context.Context, userID int64, targetIDs []int64) (map[int64]string, error) {
	if len(targetIDs) == 0 {
		return nil, nil
	}
	want := make(map[int64]struct{})
	for _, id := range targetIDs {
		want[id] = struct{}{}
	}
	out := make(map[int64]string)
	stmt, names := nicknamesTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID).Iter()
	defer iter.Close()
	var row NicknameRow
	for iter.StructScan(&row) {
		if _, ok := want[row.TargetID]; ok && row.Nickname != "" {
			out[row.TargetID] = row.Nickname
		}
		// Reset for next scan
		row = NicknameRow{}
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}
