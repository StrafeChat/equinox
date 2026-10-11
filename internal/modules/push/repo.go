package push

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var subscriptionsTable = table.New(table.Metadata{
	Name: "push_subscriptions",
	Columns: []string{"user_id", "session_id", "endpoint", "p256dh", "auth", "kind",
		"pref_preview", "pref_pm", "pref_space", "pref_calls",
		"created_at", "updated_at", "last_success_at", "last_error", "failures"},
	PartKey: []string{"user_id"},
	SortKey: []string{"session_id"},
})

// vapidKeyRow is the single push_vapid row (migration 049).
const vapidKeyRow = "vapid"

// Repository stores push subscriptions and the generated VAPID key.
type Repository interface {
	Put(ctx context.Context, s *Subscription) error
	Get(ctx context.Context, userID, sessionID int64) (*Subscription, error)
	ListByUser(ctx context.Context, userID int64) ([]Subscription, error)
	// Delete removes one subscription and reports whether the user has any left.
	Delete(ctx context.Context, userID, sessionID int64) (remaining int, err error)
	UpdatePrefs(ctx context.Context, userID, sessionID int64, p Prefs, at time.Time) error
	RecordSuccess(ctx context.Context, userID, sessionID int64, at time.Time) error
	RecordFailure(ctx context.Context, userID, sessionID int64, reason string, failures int) error
	// ForEachUser calls fn with every user id that has at least one subscription. A full
	// scan, for rebuilding the Redis index - never on a request path.
	ForEachUser(ctx context.Context, fn func(userID int64) error) error
	keyStore
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) Put(ctx context.Context, s *Subscription) error {
	stmt, names := subscriptionsTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(s).ExecRelease()
}

func (r *repo) Get(ctx context.Context, userID, sessionID int64) (*Subscription, error) {
	var s Subscription
	stmt, names := subscriptionsTable.Get()
	err := r.session.Query(stmt, names).WithContext(ctx).Bind(userID, sessionID).GetRelease(&s)
	if err == gocql.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *repo) ListByUser(ctx context.Context, userID int64) ([]Subscription, error) {
	var out []Subscription
	stmt, names := subscriptionsTable.Select()
	err := r.session.Query(stmt, names).WithContext(ctx).Bind(userID).SelectRelease(&out)
	return out, err
}

func (r *repo) Delete(ctx context.Context, userID, sessionID int64) (int, error) {
	stmt, names := subscriptionsTable.Delete()
	if err := r.session.Query(stmt, names).WithContext(ctx).Bind(userID, sessionID).ExecRelease(); err != nil {
		return 0, err
	}
	var n int
	err := r.session.Query("SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?", nil).
		WithContext(ctx).Bind(userID).GetRelease(&n)
	return n, err
}

func (r *repo) UpdatePrefs(ctx context.Context, userID, sessionID int64, p Prefs, at time.Time) error {
	stmt, names := subscriptionsTable.Update("pref_preview", "pref_pm", "pref_space", "pref_calls", "updated_at")
	return r.session.Query(stmt, names).WithContext(ctx).
		Bind(p.Preview, p.PM, p.Space, p.Calls, at, userID, sessionID).ExecRelease()
}

// RecordSuccess and RecordFailure are plain upserts: a delivery outcome that lands just
// after the subscription was deleted can leave a row with no endpoint behind, which the
// worker treats as gone and deletes (see usable). A lightweight transaction on every
// delivery would cost far more than that rare cleanup.
func (r *repo) RecordSuccess(ctx context.Context, userID, sessionID int64, at time.Time) error {
	stmt, names := subscriptionsTable.Update("last_success_at", "last_error", "failures")
	return r.session.Query(stmt, names).WithContext(ctx).Bind(at, "", 0, userID, sessionID).ExecRelease()
}

func (r *repo) RecordFailure(ctx context.Context, userID, sessionID int64, reason string, failures int) error {
	stmt, names := subscriptionsTable.Update("last_error", "failures")
	return r.session.Query(stmt, names).WithContext(ctx).Bind(reason, failures, userID, sessionID).ExecRelease()
}

func (r *repo) ForEachUser(ctx context.Context, fn func(userID int64) error) error {
	iter := r.session.Query("SELECT DISTINCT user_id FROM push_subscriptions", nil).
		WithContext(ctx).PageSize(1000).Iter()
	var uid int64
	for iter.Scan(&uid) {
		if err := fn(uid); err != nil {
			_ = iter.Close()
			return err
		}
	}
	return iter.Close()
}

type vapidRow struct {
	Key        string    `db:"key"`
	PrivateKey string    `db:"private_key"`
	CreatedAt  time.Time `db:"created_at"`
}

// ClaimVAPIDKey is a compare-and-set: the first replica to start writes its key, every
// later one (and every later start) reads that key back.
func (r *repo) ClaimVAPIDKey(ctx context.Context, candidate string) (string, error) {
	var existing vapidRow
	applied, err := r.session.
		Query("INSERT INTO push_vapid (key, private_key, created_at) VALUES (?, ?, ?) IF NOT EXISTS", nil).
		WithContext(ctx).Bind(vapidKeyRow, candidate, time.Now().UTC()).GetCASRelease(&existing)
	if err != nil {
		return "", err
	}
	if applied {
		return candidate, nil
	}
	return existing.PrivateKey, nil
}
