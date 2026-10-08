package messages

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3/qb"
)

// GetByIDs returns the live (not deleted) messages among ids, in no particular order. One
// query: a room's messages share a partition.
func (r *repo) GetByIDs(ctx context.Context, roomID int64, ids []int64) ([]Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	stmt, names := qb.Select(messagesTable.Name()).Columns(messagesTable.Metadata().Columns...).
		Where(qb.Eq("room_id"), qb.In("id")).ToCql()
	var rows []Message
	q := r.session.Query(stmt, names).WithContext(ctx).BindMap(qb.M{"room_id": roomID, "id": ids})
	if err := q.SelectRelease(&rows); err != nil {
		return nil, err
	}
	live := rows[:0]
	for _, m := range rows {
		if m.DeletedAt == nil || m.DeletedAt.IsZero() {
			live = append(live, m)
		}
	}
	return live, nil
}

// Pin flags the message and adds it to room_pins in one logged batch, so the two tables
// cannot disagree.
func (r *repo) Pin(ctx context.Context, roomID, messageID, userID int64, at time.Time) error {
	b := r.session.Session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	b.Query("UPDATE messages SET pinned_at = ?, pinned_by = ? WHERE room_id = ? AND id = ?", at, userID, roomID, messageID)
	b.Query("INSERT INTO room_pins (room_id, pinned_at, message_id, pinned_by) VALUES (?, ?, ?, ?)", roomID, at, messageID, userID)
	return r.session.Session.ExecuteBatch(b)
}

// Unpin clears the flag and removes the room_pins row; at is the pinned_at the row carries.
func (r *repo) Unpin(ctx context.Context, roomID, messageID int64, at time.Time) error {
	b := r.session.Session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	b.Query("UPDATE messages SET pinned_at = null, pinned_by = null WHERE room_id = ? AND id = ?", roomID, messageID)
	b.Query("DELETE FROM room_pins WHERE room_id = ? AND pinned_at = ? AND message_id = ?", roomID, at, messageID)
	return r.session.Session.ExecuteBatch(b)
}

// ListPins returns a room's pins, newest first.
func (r *repo) ListPins(ctx context.Context, roomID int64, limit int) ([]Pin, error) {
	iter := r.session.Session.Query("SELECT room_id, pinned_at, message_id, pinned_by FROM room_pins WHERE room_id = ? LIMIT ?", roomID, limit).
		WithContext(ctx).Iter()
	var out []Pin
	var p Pin
	for iter.Scan(&p.RoomID, &p.PinnedAt, &p.MessageID, &p.PinnedBy) {
		out = append(out, p)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) CountPins(ctx context.Context, roomID int64) (int, error) {
	var n int
	if err := r.session.Session.Query("SELECT COUNT(*) FROM room_pins WHERE room_id = ?", roomID).WithContext(ctx).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// SetThreadID points a message at the thread started from it (nil clears it).
func (r *repo) SetThreadID(ctx context.Context, roomID, messageID int64, threadID *int64) error {
	return r.session.Session.Query("UPDATE messages SET thread_id = ? WHERE room_id = ? AND id = ?", threadID, roomID, messageID).WithContext(ctx).Exec()
}
