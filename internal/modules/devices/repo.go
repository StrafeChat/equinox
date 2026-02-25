package devices

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var deviceKeysTable = table.New(table.Metadata{
	Name:    "device_keys",
	Columns: []string{"user_id", "device_id", "identity_key_public", "signed_prekey_public", "signed_prekey_signature", "signed_prekey_id", "registration_id", "created_at", "updated_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"device_id"},
})

var oneTimePrekeysTable = table.New(table.Metadata{
	Name:    "one_time_prekeys",
	Columns: []string{"user_id", "device_id", "key_id", "public_key", "created_at"},
	PartKey: []string{"user_id", "device_id"},
	SortKey: []string{"key_id"},
})

var deviceKeyBackupsTable = table.New(table.Metadata{
	Name:    "device_key_backups",
	Columns: []string{"user_id", "encrypted_backup", "salt", "created_at", "updated_at"},
	PartKey: []string{"user_id"},
})

type DeviceKeyBackup struct {
	UserID          int64     `db:"user_id"`
	EncryptedBackup string    `db:"encrypted_backup"`
	Salt            string    `db:"salt"`
	CreatedAt       time.Time `db:"created_at"`
	UpdatedAt       time.Time `db:"updated_at"`
}

type Repository interface {
	UpsertDevice(ctx context.Context, userID int64, d *DeviceKeys) error
	GetDevice(ctx context.Context, userID, deviceID int64) (*DeviceKeys, error)
	ListDevices(ctx context.Context, userID int64) ([]DeviceKeys, error)
	AddOneTimePrekeys(ctx context.Context, userID, deviceID int64, prekeys []OneTimePrekeyUpload) error
	TakeOneTimePrekey(ctx context.Context, userID, deviceID int64) (*OneTimePrekey, error)
	UpsertKeyBackup(ctx context.Context, userID int64, encryptedBackup, salt string) error
	GetKeyBackup(ctx context.Context, userID int64) (*DeviceKeyBackup, error)
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) UpsertDevice(ctx context.Context, userID int64, d *DeviceKeys) error {
	now := time.Now().UTC()
	d.UserID = userID
	d.UpdatedAt = now
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	stmt, names := deviceKeysTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(d).ExecRelease()
}

func (r *repo) GetDevice(ctx context.Context, userID, deviceID int64) (*DeviceKeys, error) {
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

func (r *repo) ListDevices(ctx context.Context, userID int64) ([]DeviceKeys, error) {
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

func (r *repo) AddOneTimePrekeys(ctx context.Context, userID, deviceID int64, prekeys []OneTimePrekeyUpload) error {
	now := time.Now().UTC()
	stmt, _ := oneTimePrekeysTable.Insert()
	b := r.session.NewBatch(gocql.UnloggedBatch).WithContext(ctx)
	for _, p := range prekeys {
		b.Query(stmt, userID, deviceID, p.KeyID, p.PublicKey, now)
	}
	return r.session.ExecuteBatch(b)
}

/* TakeOneTimePrekey fetches one one-time prekey and deletes it (consume).
 Returns nil if none available. */
func (r *repo) TakeOneTimePrekey(ctx context.Context, userID, deviceID int64) (*OneTimePrekey, error) {
	stmt, names := oneTimePrekeysTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID, deviceID).Iter()
	defer iter.Close()
	var p OneTimePrekey
	if !iter.StructScan(&p) {
		return nil, iter.Close()
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	delStmt, delNames := oneTimePrekeysTable.Delete()
	q2 := r.session.Query(delStmt, delNames).WithContext(ctx)
	if err := q2.Bind(userID, deviceID, p.KeyID).ExecRelease(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *repo) UpsertKeyBackup(ctx context.Context, userID int64, encryptedBackup, salt string) error {
	now := time.Now().UTC()
	stmt, names := deviceKeyBackupsTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(userID, encryptedBackup, salt, now, now).ExecRelease()
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
