package discover

import (
	"context"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var listingsTable = table.New(table.Metadata{
	Name:    "discover_listings",
	Columns: []string{"kind", "id", "status", "tagline", "tags", "requested_by", "requested_at", "reviewed_by", "reviewed_at", "note", "federate_opt_out"},
	PartKey: []string{"kind"},
	SortKey: []string{"id"},
})

// One partition: a directory is hundreds of rows. Clustering on domain first lets a peer's
// whole set be replaced or dropped in one range delete.
var remoteListingsTable = table.New(table.Metadata{
	Name: "discover_remote_listings",
	Columns: []string{
		"bucket", "domain", "space_id", "name", "name_acronym", "icon", "banner",
		"description", "tagline", "tags", "member_count", "online_count", "fetched_at",
	},
	PartKey: []string{"bucket"},
	SortKey: []string{"domain", "space_id"},
})

const remoteBucket = 0

type Repository interface {
	Get(ctx context.Context, kind string, id int64) (*Listing, error)
	Put(ctx context.Context, l *Listing) error
	Delete(ctx context.Context, kind string, id int64) error
	// List is every listing of a kind, whatever its status.
	List(ctx context.Context, kind string) ([]Listing, error)

	// ReplaceRemote swaps everything known about one peer's directory for what it just
	// said, in that order: a space it stopped listing disappears here too.
	ReplaceRemote(ctx context.Context, domain string, listings []RemoteListing) error
	// DropRemote forgets a peer's directory (it went away, or was blocked).
	DropRemote(ctx context.Context, domain string) error
	// ListRemote is every peer listing this instance currently knows.
	ListRemote(ctx context.Context) ([]RemoteListing, error)
	// GetRemote is one peer's listing of one space, for the join path.
	GetRemote(ctx context.Context, domain string, spaceID int64) (*RemoteListing, error)
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

func (r *repo) ReplaceRemote(ctx context.Context, domain string, listings []RemoteListing) error {
	if err := r.DropRemote(ctx, domain); err != nil {
		return err
	}
	stmt, names := remoteListingsTable.Insert()
	for i := range listings {
		l := listings[i]
		l.Domain = domain
		if err := r.session.Query(stmt, names).WithContext(ctx).BindStructMap(&l, map[string]interface{}{"bucket": remoteBucket}).ExecRelease(); err != nil {
			return err
		}
	}
	return nil
}

func (r *repo) DropRemote(ctx context.Context, domain string) error {
	q := r.session.Session.Query(
		"DELETE FROM discover_remote_listings WHERE bucket = ? AND domain = ?", remoteBucket, domain,
	).WithContext(ctx)
	defer q.Release()
	return q.Exec()
}

func (r *repo) ListRemote(ctx context.Context) ([]RemoteListing, error) {
	stmt, names := remoteListingsTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var out []RemoteListing
	if err := q.Bind(remoteBucket).SelectRelease(&out); err != nil && err != gocql.ErrNotFound {
		return nil, err
	}
	return out, nil
}

func (r *repo) GetRemote(ctx context.Context, domain string, spaceID int64) (*RemoteListing, error) {
	stmt, names := remoteListingsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var l RemoteListing
	if err := q.Bind(remoteBucket, domain, spaceID).GetRelease(&l); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &l, nil
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
