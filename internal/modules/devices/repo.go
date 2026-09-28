package devices

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/qb"
	"github.com/scylladb/gocqlx/v3/table"
)

var deviceKeysTable = table.New(table.Metadata{
	Name:    "device_keys",
	Columns: []string{"user_id", "device_id", "keys_json", "created_at", "updated_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"device_id"},
})

var oneTimeKeysTable = table.New(table.Metadata{
	Name:    "device_one_time_keys",
	Columns: []string{"user_id", "device_id", "key_id", "key_json", "is_fallback", "created_at"},
	PartKey: []string{"user_id", "device_id"},
	SortKey: []string{"key_id"},
})

var toDeviceMessagesTable = table.New(table.Metadata{
	Name:    "to_device_messages",
	Columns: []string{"recipient_user_id", "recipient_device_id", "message_id", "sender_user_id", "sender_device_id", "sender_fid", "event_type", "content_json", "created_at"},
	PartKey: []string{"recipient_user_id", "recipient_device_id"},
	SortKey: []string{"message_id"},
})

var deviceKeyBackupsTable = table.New(table.Metadata{
	Name:    "device_key_backups",
	Columns: []string{"user_id", "encrypted_backup", "salt", "room_keys", "created_at", "updated_at"},
	PartKey: []string{"user_id"},
})

// Columns spelled out rather than taken from deviceKeyBackupsTable.Insert(): that builds
// an INSERT over every column in the metadata, so adding one (room_keys did exactly that)
// changes the arity this positional Bind has to match, and nothing catches the drift until
// the query runs.
var upsertKeyBackupStmt, upsertKeyBackupNames = qb.Insert("device_key_backups").
	Columns("user_id", "encrypted_backup", "salt", "room_keys", "created_at", "updated_at").
	ToCql()

var keyBackupVersionsTable = table.New(table.Metadata{
	Name:    "key_backup_versions",
	Columns: []string{"user_id", "version", "algorithm", "auth_data", "wrapped_private_key", "salt", "etag", "created_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"version"},
})

var keyBackupSessionsTable = table.New(table.Metadata{
	Name:    "key_backup_sessions",
	Columns: []string{"user_id", "version", "room_id", "session_id", "session_data", "first_message_index", "forwarded_count", "is_verified", "created_at"},
	PartKey: []string{"user_id", "version"},
	SortKey: []string{"room_id", "session_id"},
})

// Spelled out for the same reason as upsertKeyBackupStmt below: a batch binds these
// positionally, so the statement's arity must not drift with the table metadata.
var insertBackupSessionStmt, _ = qb.Insert("key_backup_sessions").
	Columns("user_id", "version", "room_id", "session_id", "session_data", "first_message_index", "forwarded_count", "is_verified", "created_at").
	ToCql()

type DeviceKeyBackup struct {
	UserID          int64     `db:"user_id"`
	EncryptedBackup string    `db:"encrypted_backup"`
	Salt            string    `db:"salt"`
	// RoomKeys is the device's Megolm session export, encrypted under the store passphrase.
	// Empty for backups made before recovery carried key material.
	RoomKeys  string    `db:"room_keys"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

type Repository interface {
	UpsertDeviceKeys(ctx context.Context, userID, deviceID int64, keysJSON string) error
	GetDeviceKeys(ctx context.Context, userID, deviceID int64) (*DeviceKeys, error)
	ListDeviceKeys(ctx context.Context, userID int64) ([]DeviceKeys, error)
	DeleteDeviceKeys(ctx context.Context, userID, deviceID int64) error

	AddOneTimeKeys(ctx context.Context, userID, deviceID int64, keys []OneTimeKey) error
	// TakeOneTimeKey atomically consumes one non-fallback key if available, falling back
	// to (a copy of) the fallback key without deleting it. Returns nil if neither exists.
	TakeOneTimeKey(ctx context.Context, userID, deviceID int64) (*OneTimeKey, error)
	CountOneTimeKeys(ctx context.Context, userID, deviceID int64) (regular int, hasFallback bool, err error)

	AddToDeviceMessages(ctx context.Context, msgs []ToDeviceMessage) error
	ListToDeviceMessages(ctx context.Context, userID, deviceID int64, limit int) ([]ToDeviceMessage, error)
	AckToDeviceMessages(ctx context.Context, userID, deviceID int64, messageIDs []int64) error

	UpsertKeyBackup(ctx context.Context, userID int64, encryptedBackup, salt, roomKeys string) error
	GetKeyBackup(ctx context.Context, userID int64) (*DeviceKeyBackup, error)
	DeleteKeyBackup(ctx context.Context, userID int64) error

	CreateBackupVersion(ctx context.Context, v *KeyBackupVersion) error
	// LatestBackupVersion returns the newest version, or nil when the account has none.
	LatestBackupVersion(ctx context.Context, userID int64) (*KeyBackupVersion, error)
	GetBackupVersion(ctx context.Context, userID, version int64) (*KeyBackupVersion, error)
	DeleteBackupVersion(ctx context.Context, userID, version int64) error
	// GetBackupSessions looks up the stored copies of specific sessions so a write can
	// decide whether the incoming copy is an improvement (see KeyBackupSession.BetterThan).
	GetBackupSessions(ctx context.Context, userID, version int64, roomID string, sessionIDs []string) (map[string]KeyBackupSession, error)
	PutBackupSessions(ctx context.Context, sessions []KeyBackupSession) error
	// ListBackupSessions returns one page of a version's sessions, resuming after the
	// (roomID, sessionID) the previous page ended on. Empty strings start from the beginning.
	ListBackupSessions(ctx context.Context, userID, version int64, afterRoomID, afterSessionID string) ([]KeyBackupSession, error)
	CountBackupSessions(ctx context.Context, userID, version int64) (int, error)
	DeleteBackupSessions(ctx context.Context, userID, version int64) error
}

// BackupKeysPageSize bounds one page of a restore. A whole backup can hold tens of thousands
// of sessions, and serving them in one response would build a body of tens of megabytes in
// memory - so a restore walks the partition instead.
const BackupKeysPageSize = 500

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) UpsertDeviceKeys(ctx context.Context, userID, deviceID int64, keysJSON string) error {
	now := time.Now().UTC()
	// created_at is preserved across re-registration (key replenishment) when a row
	// already exists, rather than being reset to "now" every call.
	existing, err := r.GetDeviceKeys(ctx, userID, deviceID)
	if err != nil {
		return err
	}
	createdAt := now
	if existing != nil {
		createdAt = existing.CreatedAt
	}
	stmt, names := deviceKeysTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.BindStruct(&DeviceKeys{
		UserID: userID, DeviceID: deviceID, KeysJSON: keysJSON,
		CreatedAt: createdAt, UpdatedAt: now,
	}).ExecRelease()
}

func (r *repo) GetDeviceKeys(ctx context.Context, userID, deviceID int64) (*DeviceKeys, error) {
	var d DeviceKeys
	stmt, names := deviceKeysTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(userID, deviceID).GetRelease(&d); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

func (r *repo) ListDeviceKeys(ctx context.Context, userID int64) ([]DeviceKeys, error) {
	stmt, names := deviceKeysTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID).Iter()
	defer iter.Close()
	var out []DeviceKeys
	var row DeviceKeys
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	return out, iter.Close()
}

func (r *repo) DeleteDeviceKeys(ctx context.Context, userID, deviceID int64) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := deviceKeysTable.Delete()
	b.Query(stmt, userID, deviceID)
	// device_one_time_keys shares the same (user_id, device_id) partition key prefix,
	// so a partition-scoped delete (no key_id) removes the whole pool in one statement.
	b.Query("DELETE FROM device_one_time_keys WHERE user_id = ? AND device_id = ?", userID, deviceID)
	b.Query("DELETE FROM to_device_messages WHERE recipient_user_id = ? AND recipient_device_id = ?", userID, deviceID)
	return r.session.ExecuteBatch(b)
}

func (r *repo) AddOneTimeKeys(ctx context.Context, userID, deviceID int64, keys []OneTimeKey) error {
	if len(keys) == 0 {
		return nil
	}
	now := time.Now().UTC()
	stmt, _ := oneTimeKeysTable.Insert()
	const chunkSize = 50
	for i := 0; i < len(keys); i += chunkSize {
		end := i + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		b := r.session.Batch(gocql.UnloggedBatch).WithContext(ctx)
		for _, k := range keys[i:end] {
			createdAt := k.CreatedAt
			if createdAt.IsZero() {
				createdAt = now
			}
			b.Query(stmt, userID, deviceID, k.KeyID, k.KeyJSON, k.IsFallback, createdAt)
		}
		if err := r.session.ExecuteBatch(b); err != nil {
			return err
		}
	}
	return nil
}

// TakeOneTimeKey prefers a real one-time key (deleted atomically on claim via a
// lightweight transaction, so concurrent claims can't both take the same row - the
// SELECT-then-DELETE race the previous implementation had) and falls back to the
// fallback key (never deleted - it's meant to be reused as a last resort) once the
// one-time pool is empty.
func (r *repo) TakeOneTimeKey(ctx context.Context, userID, deviceID int64) (*OneTimeKey, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		stmt, names := oneTimeKeysTable.Select()
		q := r.session.Query(stmt, names).WithContext(ctx)
		iter := q.Bind(userID, deviceID).Iter()
		var rows []OneTimeKey
		var row OneTimeKey
		for iter.StructScan(&row) {
			rows = append(rows, row)
		}
		if err := iter.Close(); err != nil {
			q.Release()
			return nil, err
		}
		q.Release()

		var chosen *OneTimeKey
		var fallback *OneTimeKey
		for i := range rows {
			if rows[i].IsFallback {
				if fallback == nil {
					fallback = &rows[i]
				}
				continue
			}
			chosen = &rows[i]
			break
		}
		if chosen == nil {
			return fallback, nil // may be nil too - caller handles "no keys at all"
		}

		delQuery := r.session.Session.Query(
			"DELETE FROM device_one_time_keys WHERE user_id = ? AND device_id = ? AND key_id = ? IF EXISTS",
			userID, deviceID, chosen.KeyID,
		).WithContext(ctx)
		// ScanCAS always needs a destination for every column of the table, not just
		// [applied]: on a failed IF EXISTS (lost the race below, or the row's simply
		// gone) Scylla echoes back the row's other column values alongside
		// applied=false, and gocql's ScanCAS scans positionally into whatever's passed -
		// passing none errors with a column-count mismatch. The order below is the
		// actual wire order (verified against iter.Columns()), *not* CREATE TABLE's
		// declaration order: primary key columns first (user_id, device_id, key_id),
		// then the rest alphabetically (created_at, is_fallback, key_json). The echoed
		// values aren't needed either way - we just retry with whatever's left in `rows`.
		var casUserID, casDeviceID int64
		var casKeyID, casKeyJSON string
		var casCreatedAt time.Time
		var casIsFallback bool
		applied, err := delQuery.ScanCAS(&casUserID, &casDeviceID, &casKeyID, &casCreatedAt, &casIsFallback, &casKeyJSON)
		delQuery.Release()
		if err != nil {
			return nil, err
		}
		if applied {
			return chosen, nil
		}
		// Lost the race to another concurrent claim - retry with whatever's left.
	}
	return nil, nil
}

func (r *repo) CountOneTimeKeys(ctx context.Context, userID, deviceID int64) (int, bool, error) {
	stmt, names := oneTimeKeysTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID, deviceID).Iter()
	defer iter.Close()
	var row OneTimeKey
	regular := 0
	hasFallback := false
	for iter.StructScan(&row) {
		if row.IsFallback {
			hasFallback = true
		} else {
			regular++
		}
	}
	return regular, hasFallback, iter.Close()
}

func (r *repo) AddToDeviceMessages(ctx context.Context, msgs []ToDeviceMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	now := time.Now().UTC()
	stmt, _ := toDeviceMessagesTable.Insert()
	const chunkSize = 50
	for i := 0; i < len(msgs); i += chunkSize {
		end := i + chunkSize
		if end > len(msgs) {
			end = len(msgs)
		}
		b := r.session.Batch(gocql.UnloggedBatch).WithContext(ctx)
		for _, m := range msgs[i:end] {
			createdAt := m.CreatedAt
			if createdAt.IsZero() {
				createdAt = now
			}
			b.Query(stmt, m.RecipientUserID, m.RecipientDeviceID, m.MessageID, m.SenderUserID, m.SenderDeviceID, m.SenderFID, m.EventType, m.ContentJSON, createdAt)
		}
		if err := r.session.ExecuteBatch(b); err != nil {
			return err
		}
	}
	return nil
}

func (r *repo) ListToDeviceMessages(ctx context.Context, userID, deviceID int64, limit int) ([]ToDeviceMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	stmt, names := toDeviceMessagesTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	q = q.PageSize(limit)
	defer q.Release()
	iter := q.Bind(userID, deviceID).Iter()
	defer iter.Close()
	var out []ToDeviceMessage
	var row ToDeviceMessage
	for len(out) < limit && iter.StructScan(&row) {
		out = append(out, row)
	}
	return out, iter.Close()
}

func (r *repo) AckToDeviceMessages(ctx context.Context, userID, deviceID int64, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	stmt, _ := toDeviceMessagesTable.Delete()
	const chunkSize = 50
	for i := 0; i < len(messageIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(messageIDs) {
			end = len(messageIDs)
		}
		b := r.session.Batch(gocql.UnloggedBatch).WithContext(ctx)
		for _, id := range messageIDs[i:end] {
			b.Query(stmt, userID, deviceID, id)
		}
		if err := r.session.ExecuteBatch(b); err != nil {
			return err
		}
	}
	return nil
}

func (r *repo) UpsertKeyBackup(ctx context.Context, userID int64, encryptedBackup, salt, roomKeys string) error {
	now := time.Now().UTC()
	q := r.session.Query(upsertKeyBackupStmt, upsertKeyBackupNames).WithContext(ctx)
	defer q.Release()
	return q.Bind(userID, encryptedBackup, salt, roomKeys, now, now).ExecRelease()
}

func (r *repo) GetKeyBackup(ctx context.Context, userID int64) (*DeviceKeyBackup, error) {
	var b DeviceKeyBackup
	stmt, names := deviceKeyBackupsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(userID).GetRelease(&b); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// DeleteKeyBackup drops the legacy PIN-wrapped backup. Called once the asymmetric backup
// holds the same room keys: leaving the old row behind would keep a copy of the account's
// history recoverable with a 6-digit PIN, which is exactly what the new scheme exists to fix.
func (r *repo) DeleteKeyBackup(ctx context.Context, userID int64) error {
	stmt, names := deviceKeyBackupsTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(userID).ExecRelease()
}

func (r *repo) CreateBackupVersion(ctx context.Context, v *KeyBackupVersion) error {
	stmt, names := keyBackupVersionsTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(v).ExecRelease()
}

func (r *repo) LatestBackupVersion(ctx context.Context, userID int64) (*KeyBackupVersion, error) {
	// The table clusters by version DESC, so the first row is the newest.
	stmt, names := keyBackupVersionsTable.SelectBuilder().Limit(1).ToCql()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var v KeyBackupVersion
	if err := q.Bind(userID).GetRelease(&v); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}

func (r *repo) GetBackupVersion(ctx context.Context, userID, version int64) (*KeyBackupVersion, error) {
	stmt, names := keyBackupVersionsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var v KeyBackupVersion
	if err := q.Bind(userID, version).GetRelease(&v); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}

func (r *repo) DeleteBackupVersion(ctx context.Context, userID, version int64) error {
	stmt, names := keyBackupVersionsTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(userID, version).ExecRelease()
}

func (r *repo) GetBackupSessions(ctx context.Context, userID, version int64, roomID string, sessionIDs []string) (map[string]KeyBackupSession, error) {
	out := make(map[string]KeyBackupSession, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	// SelectBuilder already constrains the partition key (user_id, version); room_id and
	// session_id are clustering columns, so they have to be added here.
	stmt, names := keyBackupSessionsTable.SelectBuilder().
		Where(qb.Eq("room_id"), qb.In("session_id")).
		ToCql()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID, version, roomID, sessionIDs).Iter()
	var row KeyBackupSession
	for iter.StructScan(&row) {
		out[row.SessionID] = row
	}
	return out, iter.Close()
}

func (r *repo) PutBackupSessions(ctx context.Context, sessions []KeyBackupSession) error {
	if len(sessions) == 0 {
		return nil
	}
	now := time.Now().UTC()
	const chunkSize = 50
	for i := 0; i < len(sessions); i += chunkSize {
		end := i + chunkSize
		if end > len(sessions) {
			end = len(sessions)
		}
		b := r.session.Batch(gocql.UnloggedBatch).WithContext(ctx)
		for _, s := range sessions[i:end] {
			createdAt := s.CreatedAt
			if createdAt.IsZero() {
				createdAt = now
			}
			b.Query(insertBackupSessionStmt, s.UserID, s.Version, s.RoomID, s.SessionID, s.SessionData, s.FirstMessageIndex, s.ForwardedCount, s.IsVerified, createdAt)
		}
		if err := r.session.ExecuteBatch(b); err != nil {
			return err
		}
	}
	return nil
}

// A multi-column slice restriction on the clustering columns, which is what makes the cursor
// stable: it names an exact position in the partition rather than a driver-side page state
// that can expire between requests. ("", "") sorts before every real row, since empty room
// and session ids are rejected on write.
var listBackupSessionsPageStmt, listBackupSessionsPageNames = qb.Select("key_backup_sessions").
	Columns("room_id", "session_id", "session_data", "first_message_index", "forwarded_count", "is_verified").
	Where(qb.Eq("user_id"), qb.Eq("version"), qb.GtTuple("(room_id,session_id)", 2)).
	Limit(BackupKeysPageSize).
	ToCql()

func (r *repo) ListBackupSessions(ctx context.Context, userID, version int64, afterRoomID, afterSessionID string) ([]KeyBackupSession, error) {
	q := r.session.Query(listBackupSessionsPageStmt, listBackupSessionsPageNames).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID, version, afterRoomID, afterSessionID).Iter()
	out := make([]KeyBackupSession, 0, BackupKeysPageSize)
	var row KeyBackupSession
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	return out, iter.Close()
}

// CountBackupSessions is a partition-wide COUNT(*). That is a real scan, but it runs once
// per upload batch (to report the backup's size, and to enforce the per-account ceiling)
// over a partition that holds thousands of small rows, not millions.
func (r *repo) CountBackupSessions(ctx context.Context, userID, version int64) (int, error) {
	stmt, names := keyBackupSessionsTable.SelectBuilder().Count("session_id").ToCql()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var count int
	if err := q.Bind(userID, version).Scan(&count); err != nil {
		if err == gocql.ErrNotFound {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

// DeleteBackupSessions drops a whole version's sessions. Spelled out rather than using
// table.DeleteBuilder(), which constrains the *full* primary key - that would need a room
// and session id, and this has to delete the entire partition in one tombstone.
var deleteBackupSessionsStmt, deleteBackupSessionsNames = qb.Delete("key_backup_sessions").
	Where(qb.Eq("user_id"), qb.Eq("version")).
	ToCql()

func (r *repo) DeleteBackupSessions(ctx context.Context, userID, version int64) error {
	q := r.session.Query(deleteBackupSessionsStmt, deleteBackupSessionsNames).WithContext(ctx)
	return q.Bind(userID, version).ExecRelease()
}
