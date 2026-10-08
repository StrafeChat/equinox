package rooms

import (
	"context"
	"strings"
	"time"

	"github.com/gocql/gocql"
)

// Threads: the rows behind a thread room - its own columns on rooms, its member list (both
// directions), its message counter and the auto-archive queue. See the threads module for
// the rules; this file only reads and writes.

// ThreadArchiveBucket names the queue partition a due time falls in (one per hour, UTC).
func ThreadArchiveBucket(t time.Time) string {
	return t.UTC().Format("2006-01-02T15")
}

// CreateThread inserts a thread room into rooms and rooms_by_space.
func (r *repo) CreateThread(ctx context.Context, room *Room) error {
	// A thread is a space room with the thread columns filled in; the shared insert carries
	// them for every room (nil for channels).
	return r.CreateSpaceRoom(ctx, room)
}

// UpdateThread applies a partial update to a thread's settings; nil fields are left alone.
// Flipping the archive state also stamps thread_archived_at, Discord's archive_timestamp.
func (r *repo) UpdateThread(ctx context.Context, roomID int64, p ThreadPatch) error {
	now := time.Now().UTC()
	sets := []string{"updated_at = ?"}
	args := []interface{}{now}
	add := func(col string, v interface{}) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Archived != nil {
		add("thread_archived", *p.Archived)
		add("thread_archived_at", now)
	}
	if p.Locked != nil {
		add("thread_locked", *p.Locked)
	}
	if p.Invitable != nil {
		add("thread_invitable", *p.Invitable)
	}
	if p.AutoArchiveMinutes != nil {
		add("thread_auto_archive_minutes", *p.AutoArchiveMinutes)
	}
	if p.SlowmodeSeconds != nil {
		add("slowmode_seconds", *p.SlowmodeSeconds)
	}
	if p.LastActiveAt != nil {
		add("thread_last_active_at", p.LastActiveAt.UTC())
	}
	args = append(args, roomID)
	return r.session.Session.Query("UPDATE rooms SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...).WithContext(ctx).Exec()
}

// DeleteThread removes a thread and everything that hangs off it. The counter row cannot
// share a batch with the rest and is cleared separately, best-effort.
func (r *repo) DeleteThread(ctx context.Context, spaceID, roomID int64) error {
	members, _ := r.ListThreadMemberIDs(ctx, roomID)
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	b.Query("DELETE FROM rooms WHERE id = ?", roomID)
	b.Query("DELETE FROM rooms_by_space WHERE space_id = ? AND room_id = ?", spaceID, roomID)
	b.Query("DELETE FROM thread_members WHERE room_id = ?", roomID)
	for _, uid := range members {
		b.Query("DELETE FROM thread_members_by_user WHERE user_id = ? AND room_id = ?", uid, roomID)
	}
	b.Query("DELETE FROM space_room_role_overrides WHERE space_id = ? AND room_id = ?", spaceID, roomID)
	b.Query("DELETE FROM space_room_user_overrides WHERE space_id = ? AND room_id = ?", spaceID, roomID)
	if err := r.session.ExecuteBatch(b); err != nil {
		return err
	}
	_ = r.session.Session.Query("DELETE FROM thread_counters WHERE room_id = ?", roomID).WithContext(ctx).Exec()
	return nil
}

// ---- members ----------------------------------------------------------------------------

func (r *repo) AddThreadMember(ctx context.Context, roomID, userID int64, at time.Time) error {
	at = at.UTC()
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	b.Query("INSERT INTO thread_members (room_id, user_id, joined_at) VALUES (?, ?, ?)", roomID, userID, at)
	b.Query("INSERT INTO thread_members_by_user (user_id, room_id, joined_at) VALUES (?, ?, ?)", userID, roomID, at)
	return r.session.ExecuteBatch(b)
}

func (r *repo) RemoveThreadMember(ctx context.Context, roomID, userID int64) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	b.Query("DELETE FROM thread_members WHERE room_id = ? AND user_id = ?", roomID, userID)
	b.Query("DELETE FROM thread_members_by_user WHERE user_id = ? AND room_id = ?", userID, roomID)
	return r.session.ExecuteBatch(b)
}

func (r *repo) IsThreadMember(ctx context.Context, roomID, userID int64) (bool, error) {
	var found int64
	err := r.session.Session.Query("SELECT user_id FROM thread_members WHERE room_id = ? AND user_id = ?", roomID, userID).WithContext(ctx).Scan(&found)
	if err == gocql.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *repo) ListThreadMembers(ctx context.Context, roomID int64) ([]ThreadMember, error) {
	iter := r.session.Session.Query("SELECT room_id, user_id, joined_at FROM thread_members WHERE room_id = ?", roomID).WithContext(ctx).Iter()
	var out []ThreadMember
	var m ThreadMember
	for iter.Scan(&m.RoomID, &m.UserID, &m.JoinedAt) {
		out = append(out, m)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) ListThreadMemberIDs(ctx context.Context, roomID int64) ([]int64, error) {
	members, err := r.ListThreadMembers(ctx, roomID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(members))
	for _, m := range members {
		out = append(out, m.UserID)
	}
	return out, nil
}

func (r *repo) ListUserThreadIDs(ctx context.Context, userID int64) ([]int64, error) {
	iter := r.session.Session.Query("SELECT room_id FROM thread_members_by_user WHERE user_id = ?", userID).WithContext(ctx).Iter()
	var out []int64
	var rid int64
	for iter.Scan(&rid) {
		out = append(out, rid)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) CountThreadMembers(ctx context.Context, roomID int64) (int, error) {
	var n int
	if err := r.session.Session.Query("SELECT COUNT(*) FROM thread_members WHERE room_id = ?", roomID).WithContext(ctx).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ---- counters ---------------------------------------------------------------------------

func (r *repo) AddThreadMessages(ctx context.Context, roomID int64, delta int64) error {
	return r.session.Session.Query("UPDATE thread_counters SET message_count = message_count + ? WHERE room_id = ?", delta, roomID).WithContext(ctx).Exec()
}

func (r *repo) ThreadMessageCounts(ctx context.Context, roomIDs []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}
	iter := r.session.Session.Query("SELECT room_id, message_count FROM thread_counters WHERE room_id IN ?", roomIDs).WithContext(ctx).Iter()
	var rid, n int64
	for iter.Scan(&rid, &n) {
		if n < 0 {
			n = 0
		}
		out[rid] = n
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- auto-archive queue -----------------------------------------------------------------

func (r *repo) EnqueueThreadArchive(ctx context.Context, roomID int64, dueAt time.Time) error {
	dueAt = dueAt.UTC()
	return r.session.Session.Query("INSERT INTO thread_archive_queue (bucket, due_at, room_id) VALUES (?, ?, ?)", ThreadArchiveBucket(dueAt), dueAt, roomID).WithContext(ctx).Exec()
}

func (r *repo) ListThreadArchiveDue(ctx context.Context, bucket string, before time.Time) ([]ThreadArchiveDue, error) {
	iter := r.session.Session.Query("SELECT bucket, due_at, room_id FROM thread_archive_queue WHERE bucket = ? AND due_at <= ?", bucket, before.UTC()).WithContext(ctx).Iter()
	var out []ThreadArchiveDue
	var d ThreadArchiveDue
	for iter.Scan(&d.Bucket, &d.DueAt, &d.RoomID) {
		out = append(out, d)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) DeleteThreadArchiveDue(ctx context.Context, d ThreadArchiveDue) error {
	return r.session.Session.Query("DELETE FROM thread_archive_queue WHERE bucket = ? AND due_at = ? AND room_id = ?", d.Bucket, d.DueAt.UTC(), d.RoomID).WithContext(ctx).Exec()
}
