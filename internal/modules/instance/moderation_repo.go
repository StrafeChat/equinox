package instance

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var banColumns = []string{"user_id", "banned_by", "reason", "created_at", "expires_at"}

var instanceBansTable = table.New(table.Metadata{
	Name:    "instance_bans",
	Columns: banColumns,
	PartKey: []string{"user_id"},
})

var instanceBansByBucketTable = table.New(table.Metadata{
	Name:    "instance_bans_by_bucket",
	Columns: append([]string{"bucket"}, banColumns...),
	PartKey: []string{"bucket"},
	SortKey: []string{"user_id"},
})

// One partition: every API process reads the whole list into memory (see ipbans.go).
var instanceIPBansTable = table.New(table.Metadata{
	Name:    "instance_ip_bans",
	Columns: []string{"bucket", "cidr", "banned_by", "reason", "created_at", "expires_at"},
	PartKey: []string{"bucket"},
	SortKey: []string{"cidr"},
})

var reportColumns = []string{
	"id", "reporter_id", "target_type", "target_id", "space_id", "room_id", "message_id",
	"reason", "details", "status", "created_at", "resolved_by", "resolved_at", "resolution", "resolution_note",
}

var reportsTable = table.New(table.Metadata{
	Name:    "reports",
	Columns: reportColumns,
	PartKey: []string{"id"},
})

// reports_by_status carries every column except status is the partition key.
var reportsByStatusTable = table.New(table.Metadata{
	Name: "reports_by_status",
	Columns: []string{
		"status", "id", "reporter_id", "target_type", "target_id", "space_id", "room_id", "message_id",
		"reason", "details", "created_at", "resolved_by", "resolved_at", "resolution", "resolution_note",
	},
	PartKey: []string{"status"},
	SortKey: []string{"id"},
})

var reportsByTargetTable = table.New(table.Metadata{
	Name:    "reports_by_target",
	Columns: []string{"target_type", "target_id", "id", "reporter_id", "status", "reason", "created_at"},
	PartKey: []string{"target_type", "target_id"},
	SortKey: []string{"id"},
})

var instanceAuditTable = table.New(table.Metadata{
	Name:    "instance_audit_log",
	Columns: []string{"bucket", "id", "actor_id", "action", "target_type", "target_id", "reason", "created_at"},
	PartKey: []string{"bucket"},
	SortKey: []string{"id"},
})

// ModerationRepository is the moderation half of the instance store. Separate from
// Repository (invites and the bootstrap claim) so the invite tests' fake keeps working.
type ModerationRepository interface {
	CreateBan(ctx context.Context, b *Ban) error
	DeleteBan(ctx context.Context, userID int64) error
	GetBan(ctx context.Context, userID int64) (*Ban, error)
	ListBans(ctx context.Context) ([]Ban, error)

	// IP bans: CIDR keyed, one partition.
	CreateIPBan(ctx context.Context, b *IPBan) error
	DeleteIPBan(ctx context.Context, cidr string) error
	ListIPBans(ctx context.Context) ([]IPBan, error)

	CreateReport(ctx context.Context, r *Report) error
	GetReport(ctx context.Context, id int64) (*Report, error)
	ListReportsByStatus(ctx context.Context, status string, limit int) ([]Report, error)
	ListReportsByTarget(ctx context.Context, targetType string, targetID int64) ([]ReportSummary, error)
	// MoveReportStatus rewrites the report with a new status, moving it between queue
	// partitions and keeping the target index in step.
	MoveReportStatus(ctx context.Context, r *Report, from string) error

	AppendAudit(ctx context.Context, e *AuditEntry) error
	ListAudit(ctx context.Context, limit int) ([]AuditEntry, error)

	// Federation peer policy: the admin-managed allow/block list (one small partition).
	ListPeerPolicy(ctx context.Context) ([]PeerPolicyEntry, error)
	SetPeerPolicy(ctx context.Context, e *PeerPolicyEntry) error
	RemovePeerPolicy(ctx context.Context, domain string) error

	// CountUsers and CountSpaces are full-table COUNT(*) scans; GetStats caches the
	// results in Redis so the scan runs at most once a minute.
	CountUsers(ctx context.Context) (int64, error)
	CountSpaces(ctx context.Context) (int64, error)
}

func NewModerationRepository(session gocqlx.Session) ModerationRepository {
	return &repo{session: session}
}

// ------------------------------------------------------------------------------ bans ----

func (r *repo) CreateBan(ctx context.Context, b *Ban) error {
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := instanceBansTable.Insert()
	batch.Query(stmt, b.UserID, b.BannedBy, b.Reason, b.CreatedAt, b.ExpiresAt)
	stmt, _ = instanceBansByBucketTable.Insert()
	batch.Query(stmt, listBucket, b.UserID, b.BannedBy, b.Reason, b.CreatedAt, b.ExpiresAt)
	return r.session.ExecuteBatch(batch)
}

func (r *repo) DeleteBan(ctx context.Context, userID int64) error {
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query("DELETE FROM instance_bans WHERE user_id = ?", userID)
	batch.Query("DELETE FROM instance_bans_by_bucket WHERE bucket = ? AND user_id = ?", listBucket, userID)
	return r.session.ExecuteBatch(batch)
}

func (r *repo) GetBan(ctx context.Context, userID int64) (*Ban, error) {
	stmt, names := instanceBansTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var b Ban
	if err := q.Bind(userID).GetRelease(&b); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

func (r *repo) ListBans(ctx context.Context) ([]Ban, error) {
	stmt, names := instanceBansByBucketTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var out []Ban
	if err := q.Bind(listBucket).SelectRelease(&out); err != nil && err != gocql.ErrNotFound {
		return nil, err
	}
	return out, nil
}

// --------------------------------------------------------------------------- ip bans ----

func (r *repo) CreateIPBan(ctx context.Context, b *IPBan) error {
	stmt, names := instanceIPBansTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.BindMap(map[string]interface{}{
		"bucket": listBucket, "cidr": b.CIDR, "banned_by": b.BannedBy, "reason": b.Reason,
		"created_at": b.CreatedAt, "expires_at": b.ExpiresAt,
	}).Exec()
}

func (r *repo) DeleteIPBan(ctx context.Context, cidr string) error {
	q := r.session.Session.Query("DELETE FROM instance_ip_bans WHERE bucket = ? AND cidr = ?", listBucket, cidr).WithContext(ctx)
	defer q.Release()
	return q.Exec()
}

func (r *repo) ListIPBans(ctx context.Context) ([]IPBan, error) {
	stmt, names := instanceIPBansTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var out []IPBan
	if err := q.Bind(listBucket).SelectRelease(&out); err != nil && err != gocql.ErrNotFound {
		return nil, err
	}
	return out, nil
}

// --------------------------------------------------------------------------- reports ----

func reportArgs(rep *Report) []interface{} {
	return []interface{}{
		rep.ID, rep.ReporterID, rep.TargetType, rep.TargetID, rep.SpaceID, rep.RoomID, rep.MessageID,
		rep.Reason, rep.Details, rep.Status, rep.CreatedAt, rep.ResolvedBy, rep.ResolvedAt, rep.Resolution, rep.ResolutionNote,
	}
}

func reportByStatusArgs(rep *Report) []interface{} {
	return []interface{}{
		rep.Status, rep.ID, rep.ReporterID, rep.TargetType, rep.TargetID, rep.SpaceID, rep.RoomID, rep.MessageID,
		rep.Reason, rep.Details, rep.CreatedAt, rep.ResolvedBy, rep.ResolvedAt, rep.Resolution, rep.ResolutionNote,
	}
}

func (r *repo) CreateReport(ctx context.Context, rep *Report) error {
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := reportsTable.Insert()
	batch.Query(stmt, reportArgs(rep)...)
	stmt, _ = reportsByStatusTable.Insert()
	batch.Query(stmt, reportByStatusArgs(rep)...)
	stmt, _ = reportsByTargetTable.Insert()
	batch.Query(stmt, rep.TargetType, rep.TargetID, rep.ID, rep.ReporterID, rep.Status, rep.Reason, rep.CreatedAt)
	return r.session.ExecuteBatch(batch)
}

func (r *repo) GetReport(ctx context.Context, id int64) (*Report, error) {
	stmt, names := reportsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var rep Report
	if err := q.Bind(id).GetRelease(&rep); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &rep, nil
}

func (r *repo) ListReportsByStatus(ctx context.Context, status string, limit int) ([]Report, error) {
	q := r.session.Query(
		"SELECT status, id, reporter_id, target_type, target_id, space_id, room_id, message_id, reason, details, "+
			"created_at, resolved_by, resolved_at, resolution, resolution_note FROM reports_by_status WHERE status = ? LIMIT ?",
		[]string{"status", "limit"},
	).WithContext(ctx)
	defer q.Release()
	var out []Report
	if err := q.Bind(status, limit).SelectRelease(&out); err != nil && err != gocql.ErrNotFound {
		return nil, err
	}
	return out, nil
}

func (r *repo) ListReportsByTarget(ctx context.Context, targetType string, targetID int64) ([]ReportSummary, error) {
	stmt, names := reportsByTargetTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var out []ReportSummary
	if err := q.Bind(targetType, targetID).SelectRelease(&out); err != nil && err != gocql.ErrNotFound {
		return nil, err
	}
	return out, nil
}

func (r *repo) MoveReportStatus(ctx context.Context, rep *Report, from string) error {
	batch := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := reportsTable.Insert()
	batch.Query(stmt, reportArgs(rep)...)
	batch.Query("DELETE FROM reports_by_status WHERE status = ? AND id = ?", from, rep.ID)
	stmt, _ = reportsByStatusTable.Insert()
	batch.Query(stmt, reportByStatusArgs(rep)...)
	batch.Query("UPDATE reports_by_target SET status = ? WHERE target_type = ? AND target_id = ? AND id = ?",
		rep.Status, rep.TargetType, rep.TargetID, rep.ID)
	return r.session.ExecuteBatch(batch)
}

// ----------------------------------------------------------------------------- audit ----

func (r *repo) AppendAudit(ctx context.Context, e *AuditEntry) error {
	stmt, names := instanceAuditTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(listBucket, e.ID, e.ActorID, e.Action, e.TargetType, e.TargetID, e.Reason, e.CreatedAt).Exec()
}

func (r *repo) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	q := r.session.Query(
		"SELECT id, actor_id, action, target_type, target_id, reason, created_at FROM instance_audit_log WHERE bucket = ? LIMIT ?",
		[]string{"bucket", "limit"},
	).WithContext(ctx)
	defer q.Release()
	var out []AuditEntry
	if err := q.Bind(listBucket, limit).SelectRelease(&out); err != nil && err != gocql.ErrNotFound {
		return nil, err
	}
	return out, nil
}

func (r *repo) CountUsers(ctx context.Context) (int64, error)  { return r.count(ctx, "users") }
func (r *repo) CountSpaces(ctx context.Context) (int64, error) { return r.count(ctx, "spaces") }

// -------------------------------------------------------------- federation policy ----

func (r *repo) ListPeerPolicy(ctx context.Context) ([]PeerPolicyEntry, error) {
	q := r.session.Query(
		"SELECT domain, kind, added_by, created_at FROM federation_peer_policy WHERE bucket = ?",
		[]string{"bucket"},
	).WithContext(ctx)
	defer q.Release()
	var out []PeerPolicyEntry
	if err := q.Bind(listBucket).SelectRelease(&out); err != nil && err != gocql.ErrNotFound {
		return nil, err
	}
	return out, nil
}

func (r *repo) SetPeerPolicy(ctx context.Context, e *PeerPolicyEntry) error {
	return r.session.Query(
		"INSERT INTO federation_peer_policy (bucket, domain, kind, added_by, created_at) VALUES (?, ?, ?, ?, ?)",
		[]string{"bucket", "domain", "kind", "added_by", "created_at"},
	).WithContext(ctx).Bind(listBucket, e.Domain, e.Kind, e.AddedBy, e.CreatedAt).ExecRelease()
}

func (r *repo) RemovePeerPolicy(ctx context.Context, domain string) error {
	return r.session.Query(
		"DELETE FROM federation_peer_policy WHERE bucket = ? AND domain = ?",
		[]string{"bucket", "domain"},
	).WithContext(ctx).Bind(listBucket, domain).ExecRelease()
}

// count is a COUNT(*) over a whole table - a full scan, aggregated server-side into one
// row. Only ever called behind the Redis cache in GetStats.
func (r *repo) count(ctx context.Context, table string) (int64, error) {
	var n int64
	if err := r.session.Query("SELECT COUNT(*) FROM "+table, nil).WithContext(ctx).GetRelease(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// unused guard so a future refactor that drops time from this file fails loudly here.
var _ = time.Now
