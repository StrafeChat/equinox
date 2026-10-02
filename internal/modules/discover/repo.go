package discover

import (
	"context"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var listingsTable = table.New(table.Metadata{
	Name:    "discover_listings",
	Columns: []string{"kind", "id", "status", "tagline", "tags", "requested_by", "requested_at", "reviewed_by", "reviewed_at", "note"},
	PartKey: []string{"kind"},
	SortKey: []string{"id"},
})

type Repository interface {
	Get(ctx context.Context, kind string, id int64) (*Listing, error)
	Put(ctx context.Context, l *Listing) error
	Delete(ctx context.Context, kind string, id int64) error
	// List is every listing of a kind, whatever its status.
	List(ctx context.Context, kind string) ([]Listing, error)
}

type repo struct{ session gocqlx.Session }

func NewRepository(session gocqlx.Session) Repository { return &repo{session: session} }

func (r *repo) Get(ctx context.Context, kind string, id int64) (*Listing, error) {
	var l Listing
	stmt, names := listingsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(kind, id).GetRelease(&l); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &l, nil
}

func (r *repo) Put(ctx context.Context, l *Listing) error {
	stmt, names := listingsTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(l).ExecRelease()
}

func (r *repo) Delete(ctx context.Context, kind string, id int64) error {
	stmt, names := listingsTable.Delete()
	return r.session.Query(stmt, names).WithContext(ctx).Bind(kind, id).ExecRelease()
}

func (r *repo) List(ctx context.Context, kind string) ([]Listing, error) {
	stmt, names := listingsTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx).Bind(kind)
	var out []Listing
	if err := q.SelectRelease(&out); err != nil {
		return nil, err
	}
	return out, nil
}
