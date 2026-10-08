package instance

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

// listBucket is the single partition every live invite is listed under. An instance's open
// invites are a short list read occasionally by one operator, so one partition is right.
const listBucket = 0

// stateKey is the primary key of the single instance_state row.
const stateKey = "instance"

// systemAccountKey is the single row of instance_system_account (migration 047).
const systemAccountKey = "system"

var inviteColumns = []string{"code", "created_by", "note", "max_uses", "uses", "expires_at", "created_at"}

var instanceInvitesTable = table.New(table.Metadata{
	Name:    "instance_invites",
	Columns: inviteColumns,
	PartKey: []string{"code"},
})

var instanceInvitesByBucketTable = table.New(table.Metadata{
	Name:    "instance_invites_by_bucket",
	Columns: append([]string{"bucket"}, inviteColumns...),
	PartKey: []string{"bucket"},
	SortKey: []string{"code"},
})

type Repository interface {
	CreateInvite(ctx context.Context, inv *Invite) error
	GetInviteByCode(ctx context.Context, code string) (*Invite, error)
	ListInvites(ctx context.Context) ([]Invite, error)
	DeleteInvite(ctx context.Context, code string) error
	// TakeInviteUse increments uses from `from` to `from+1` only if it is still `from`,
	// so two people redeeming the last use of an invite cannot both win.
	TakeInviteUse(ctx context.Context, code string, from int) (bool, error)

	ClaimBootstrap(ctx context.Context, userID int64) (bool, error)
	ReleaseBootstrap(ctx context.Context) error
	IsBootstrapped(ctx context.Context) (bool, error)

	SetInstanceAdmin(ctx context.Context, userID int64, admin bool) error
	IsInstanceAdmin(ctx context.Context, userID int64) (bool, error)

	// AnyUserExists is only for deciding whether this keyspace predates instance invites;
	// it runs once per keyspace, never on a request path.
	AnyUserExists(ctx context.Context) (bool, error)
	ClaimDataMigration(ctx context.Context, name string) (bool, error)
	ReleaseDataMigration(ctx context.Context, name string) error

	// ClaimSystemAccount reserves userID as the instance's official account, once per
	// keyspace (compare-and-set); GetSystemAccountID reads it back, 0 when unset.
	ClaimSystemAccount(ctx context.Context, userID int64) (bool, error)
	GetSystemAccountID(ctx context.Context) (int64, error)
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) CreateInvite(ctx context.Context, inv *Invite) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := instanceInvitesTable.Insert()
	b.Query(stmt, inv.Code, inv.CreatedBy, inv.Note, inv.MaxUses, inv.Uses, inv.ExpiresAt, inv.CreatedAt)
	stmt, _ = instanceInvitesByBucketTable.Insert()
	b.Query(stmt, listBucket, inv.Code, inv.CreatedBy, inv.Note, inv.MaxUses, inv.Uses, inv.ExpiresAt, inv.CreatedAt)
	return r.session.ExecuteBatch(b)
}

func (r *repo) GetInviteByCode(ctx context.Context, code string) (*Invite, error) {
	stmt, names := instanceInvitesTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var inv Invite
	if err := q.Bind(code).GetRelease(&inv); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &inv, nil
}

func (r *repo) ListInvites(ctx context.Context) ([]Invite, error) {
	stmt, names := instanceInvitesByBucketTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var out []Invite
	if err := q.Bind(listBucket).SelectRelease(&out); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func (r *repo) DeleteInvite(ctx context.Context, code string) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	b.Query("DELETE FROM instance_invites WHERE code = ?", code)
	b.Query("DELETE FROM instance_invites_by_bucket WHERE bucket = ? AND code = ?", listBucket, code)
	return r.session.ExecuteBatch(b)
}

func (r *repo) TakeInviteUse(ctx context.Context, code string, from int) (bool, error) {
	applied, err := r.session.
		Query("UPDATE instance_invites SET uses = ? WHERE code = ? IF uses = ?", nil).
		WithContext(ctx).
		Bind(from+1, code, from).
		ExecCASRelease()
	if err != nil {
		return false, err
	}
	if applied {
		// Best effort: the listing copy is cosmetic, and a failure here only makes the
		// operator's "uses" column stale, never lets an invite be over-redeemed.
		_ = r.session.Query("UPDATE instance_invites_by_bucket SET uses = ? WHERE bucket = ? AND code = ?", nil).
			WithContext(ctx).Bind(from+1, listBucket, code).ExecRelease()
	}
	return applied, nil
}

func (r *repo) ClaimBootstrap(ctx context.Context, userID int64) (bool, error) {
	return r.session.
		Query("INSERT INTO instance_state (key, bootstrapped_by, bootstrapped_at) VALUES (?, ?, ?) IF NOT EXISTS", nil).
		WithContext(ctx).
		Bind(stateKey, userID, time.Now().UTC()).
		ExecCASRelease()
}

func (r *repo) ReleaseBootstrap(ctx context.Context) error {
	return r.session.Query("DELETE FROM instance_state WHERE key = ?", nil).
		WithContext(ctx).Bind(stateKey).ExecRelease()
}

func (r *repo) IsBootstrapped(ctx context.Context) (bool, error) {
	var by int64
	err := r.session.Query("SELECT bootstrapped_by FROM instance_state WHERE key = ?", nil).
		WithContext(ctx).Bind(stateKey).GetRelease(&by)
	if err == gocql.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *repo) SetInstanceAdmin(ctx context.Context, userID int64, admin bool) error {
	return r.session.Query("UPDATE users SET instance_admin = ? WHERE id = ?", nil).
		WithContext(ctx).Bind(admin, userID).ExecRelease()
}

func (r *repo) IsInstanceAdmin(ctx context.Context, userID int64) (bool, error) {
	var admin bool
	err := r.session.Query("SELECT instance_admin FROM users WHERE id = ?", nil).
		WithContext(ctx).Bind(userID).GetRelease(&admin)
	if err == gocql.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return admin, nil
}

// AnyUserExists reports whether this keyspace holds a single account. It is a range scan,
// which is why it is confined to the one-shot check in backfill.go: LIMIT 1 keeps it cheap
// on an empty keyspace and it stops at the first row on a populated one.
func (r *repo) AnyUserExists(ctx context.Context) (bool, error) {
	var id int64
	err := r.session.Query("SELECT id FROM users LIMIT 1", nil).WithContext(ctx).GetRelease(&id)
	if err == gocql.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *repo) ClaimDataMigration(ctx context.Context, name string) (bool, error) {
	// MapScanCAS rather than ScanCAS: with no destinations, ScanCAS fails *after* a
	// winning insert, reporting the claim as an error while leaving it in place.
	return r.session.Session.
		Query("INSERT INTO data_migrations (name, applied_at) VALUES (?, ?) IF NOT EXISTS", name, time.Now().UTC()).
		WithContext(ctx).
		MapScanCAS(map[string]interface{}{})
}

func (r *repo) ReleaseDataMigration(ctx context.Context, name string) error {
	return r.session.Session.Query("DELETE FROM data_migrations WHERE name = ?", name).WithContext(ctx).Exec()
}

func (r *repo) ClaimSystemAccount(ctx context.Context, userID int64) (bool, error) {
	return r.session.
		Query("INSERT INTO instance_system_account (key, user_id, created_at) VALUES (?, ?, ?) IF NOT EXISTS", nil).
		WithContext(ctx).
		Bind(systemAccountKey, userID, time.Now().UTC()).
		ExecCASRelease()
}

func (r *repo) GetSystemAccountID(ctx context.Context) (int64, error) {
	var uid int64
	err := r.session.Query("SELECT user_id FROM instance_system_account WHERE key = ?", nil).
		WithContext(ctx).Bind(systemAccountKey).GetRelease(&uid)
	if err == gocql.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return uid, nil
}
