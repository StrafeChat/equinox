package messages

import (
	"context"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var messagesTable = table.New(table.Metadata{
	Name:    "messages",
	Columns: []string{"room_id", "id", "sender_id", "sender_device_id", "ciphertext", "plaintext", "reply_to_id", "mentions", "mention_everyone", "mention_roles", "system_type", "system_payload", "attachments", "created_at", "updated_at", "deleted_at", "pinned_at", "pinned_by", "thread_id"},
	PartKey: []string{"room_id"},
	SortKey: []string{"id"},
})

var attachmentsTable = table.New(table.Metadata{
	Name:    "message_attachments",
	Columns: []string{"room_id", "id", "uploader_id", "message_id", "filename", "content_type", "size", "url", "width", "height", "encrypted", "created_at"},
	PartKey: []string{"room_id"},
	SortKey: []string{"id"},
})

type Repository interface {
	Create(ctx context.Context, m *Message) error
	// Insert stores a message as-is, keeping the timestamps the caller set (used for
	// messages relayed from another instance, which keep their origin created_at).
	Insert(ctx context.Context, m *Message) error
	GetByID(ctx context.Context, roomID, msgID int64) (*Message, error)
	List(ctx context.Context, roomID int64, beforeID *int64, limit int) ([]Message, error)
	// ListAfter returns up to limit non-deleted messages immediately after afterID, oldest-first.
	ListAfter(ctx context.Context, roomID, afterID int64, limit int) ([]Message, error)
	Update(ctx context.Context, roomID, msgID int64, ciphertext, plaintext string) (*Message, error)
	SoftDelete(ctx context.Context, roomID, msgID int64) error

	CreateAttachment(ctx context.Context, a *AttachmentRow) error
	GetAttachment(ctx context.Context, roomID, attachmentID int64) (*AttachmentRow, error)
	// SetAttachmentMessage links an upload to the message that uses it.
	SetAttachmentMessage(ctx context.Context, roomID, attachmentID, msgID int64) error
	DeleteAttachment(ctx context.Context, roomID, attachmentID int64) error

	AddReaction(ctx context.Context, roomID, messageID, userID int64, emoji string) error
	RemoveReaction(ctx context.Context, roomID, messageID, userID int64, emoji string) error
	ListReactions(ctx context.Context, roomID, messageID int64) ([]Reaction, error)
	// ListReactionsForMessages batches the per-message reads a page of history needs.
	ListReactionsForMessages(ctx context.Context, roomID int64, messageIDs []int64) (map[int64][]Reaction, error)

	// Pins (repo_pins.go): the flag on the message row and room_pins, the room's newest-first
	// list, are written together; GetByIDs reads the pinned rows in one query.
	GetByIDs(ctx context.Context, roomID int64, ids []int64) ([]Message, error)
	Pin(ctx context.Context, roomID, messageID, userID int64, at time.Time) error
	Unpin(ctx context.Context, roomID, messageID int64, at time.Time) error
	ListPins(ctx context.Context, roomID int64, limit int) ([]Pin, error)
	CountPins(ctx context.Context, roomID int64) (int, error)
	// SetThreadID points a message at the thread started from it (nil clears it).
	SetThreadID(ctx context.Context, roomID, messageID int64, threadID *int64) error
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
	return r.Insert(ctx, m)
}

func (r *repo) Insert(ctx context.Context, m *Message) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}
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
	// Clustering is (id DESC), so a bare partition select already yields newest-first.
	// Push the cursor into the clustering key (id < ?) instead of streaming+discarding
	// every newer row client-side - critical once a room has more than a page of history.
	cols := strings.Join(messagesTable.Metadata().Columns, ", ")
	var q *gocqlx.Queryx
	if beforeID != nil {
		stmt := "SELECT " + cols + " FROM messages WHERE room_id = ? AND id < ?"
		q = r.session.Query(stmt, nil).WithContext(ctx).Bind(roomID, *beforeID)
	} else {
		stmt := "SELECT " + cols + " FROM messages WHERE room_id = ?"
		q = r.session.Query(stmt, nil).WithContext(ctx).Bind(roomID)
	}
	// Small overfetch cushion: soft-deleted rows have no secondary index, so they're
	// still filtered client-side, but the range predicate keeps the scan window tight.
	q = q.PageSize(limit + 20)
	defer q.Release()
	iter := q.Iter()
	defer iter.Close()
	var out []Message
	var row Message
	for iter.StructScan(&row) {
		if row.DeletedAt != nil && !row.DeletedAt.IsZero() {
			continue
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out, iter.Close()
}

// ListAfter returns up to limit non-deleted messages immediately AFTER afterID, oldest-first
// (ascending id). Clustering is id DESC, so this reverses it with ORDER BY id ASC to get the
// rows *closest* to the cursor rather than the newest in the room - that's what "load newer"
// while reading from a jumped-to window needs. Same soft-delete overfetch as List.
func (r *repo) ListAfter(ctx context.Context, roomID, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	cols := strings.Join(messagesTable.Metadata().Columns, ", ")
	stmt := "SELECT " + cols + " FROM messages WHERE room_id = ? AND id > ? ORDER BY id ASC"
	q := r.session.Query(stmt, nil).WithContext(ctx).Bind(roomID, afterID)
	q = q.PageSize(limit + 20)
	defer q.Release()
	iter := q.Iter()
	defer iter.Close()
	var out []Message
	var row Message
	for iter.StructScan(&row) {
		if row.DeletedAt != nil && !row.DeletedAt.IsZero() {
			continue
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out, iter.Close()
}

// Update writes exactly one of ciphertext/plaintext (the other passed as ""), matching
// whichever the room's E2EE setting dictates. Previously this always wrote `ciphertext`
// regardless of the room's setting, so editing was effectively broken for plaintext rooms.
func (r *repo) Update(ctx context.Context, roomID, msgID int64, ciphertext, plaintext string) (*Message, error) {
	m, err := r.GetByID(ctx, roomID, msgID)
	if err != nil || m == nil {
		return m, err
	}
	if m.DeletedAt != nil && !m.DeletedAt.IsZero() {
		return nil, nil
	}
	now := time.Now().UTC()
	var stmt string
	var names []string
	if plaintext != "" {
		stmt, names = messagesTable.Update("plaintext", "updated_at")
	} else {
		stmt, names = messagesTable.Update("ciphertext", "updated_at")
	}
	q := r.session.Query(stmt, names).WithContext(ctx)
	value := ciphertext
	if plaintext != "" {
		value = plaintext
	}
	if err := q.Bind(value, now, roomID, msgID).ExecRelease(); err != nil {
		return nil, err
	}
	m.Ciphertext = ciphertext
	m.Plaintext = plaintext
	m.UpdatedAt = now
	return m, nil
}

// SoftDelete marks a message deleted AND scrubs its content columns - previously only
// `deleted_at` was set, so "deleted" messages (including full plaintext content in
// non-E2EE rooms) remained fully recoverable from Scylla indefinitely.
func (r *repo) SoftDelete(ctx context.Context, roomID, msgID int64) error {
	now := time.Now().UTC()
	stmt, names := messagesTable.Update("deleted_at", "ciphertext", "plaintext", "mentions", "mention_everyone", "mention_roles", "attachments")
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(now, "", "", nil, false, nil, "", roomID, msgID).ExecRelease()
}

func (r *repo) CreateAttachment(ctx context.Context, a *AttachmentRow) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	stmt, names := attachmentsTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(a).ExecRelease()
}

func (r *repo) GetAttachment(ctx context.Context, roomID, attachmentID int64) (*AttachmentRow, error) {
	var a AttachmentRow
	stmt, names := attachmentsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(roomID, attachmentID).GetRelease(&a); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (r *repo) SetAttachmentMessage(ctx context.Context, roomID, attachmentID, msgID int64) error {
	stmt, names := attachmentsTable.Update("message_id")
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(msgID, roomID, attachmentID).ExecRelease()
}

func (r *repo) DeleteAttachment(ctx context.Context, roomID, attachmentID int64) error {
	stmt, names := attachmentsTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(roomID, attachmentID).ExecRelease()
}
