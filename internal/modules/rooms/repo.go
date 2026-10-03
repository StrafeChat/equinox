package rooms

import (
	"context"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/qb"
	"github.com/scylladb/gocqlx/v3/table"
)

var roomsTable = table.New(table.Metadata{
	Name:    "rooms",
	Columns: []string{"id", "type", "space_id", "parent_id", "name", "topic", "slowmode_seconds", "position", "creator_id", "e2ee_enabled", "last_message_id", "user_limit", "bitrate", "created_at", "updated_at"},
	PartKey: []string{"id"},
})

// insertRoomStmt spells out the INSERT columns (see insertRoomsByUserStmt for why) - the
// voice settings columns were added after every other column and the positional binds
// below must match this list exactly.
var insertRoomStmt, _ = qb.Insert("rooms").
	Columns(
		"id", "type", "space_id", "parent_id", "name", "topic", "slowmode_seconds", "position",
		"creator_id", "e2ee_enabled", "last_message_id", "user_limit", "bitrate", "created_at", "updated_at",
	).ToCql()

var participantsTable = table.New(table.Metadata{
	Name:    "room_participants",
	Columns: []string{"room_id", "user_id", "joined_at"},
	PartKey: []string{"room_id"},
	SortKey: []string{"user_id"},
})

var roomsByUserTable = table.New(table.Metadata{
	Name:    "rooms_by_user",
	Columns: []string{"user_id", "room_id", "last_message_id", "last_read_message_id", "mention_count", "joined_at", "mention_count_baseline", "muted", "muted_until", "notify_mode"},
	PartKey: []string{"user_id"},
	SortKey: []string{"room_id"},
})

// The columns rooms_by_user rows are *created* with. Spelled out rather than taken from
// roomsByUserTable.Insert(), which builds an INSERT over every column in the metadata:
// adding one there (per-room mute settings did exactly that) changes the arity every
// positional bind has to match, and nothing catches the drift until the query runs. The
// columns left out are per-user preferences that are absent-means-default anyway.
var insertRoomsByUserStmt, _ = qb.Insert("rooms_by_user").
	Columns(
		"user_id",
		"room_id",
		"last_message_id",
		"last_read_message_id",
		"mention_count",
		"joined_at",
		"mention_count_baseline",
	).ToCql()

var pmRoomsTable = table.New(table.Metadata{
	Name:    "pm_rooms",
	Columns: []string{"user_a_id", "user_b_id", "room_id", "created_at"},
	PartKey: []string{"user_a_id"},
	SortKey: []string{"user_b_id"},
})

var roomsBySpaceTable = table.New(table.Metadata{
	Name:    "rooms_by_space",
	Columns: []string{"space_id", "room_id", "position", "created_at"},
	PartKey: []string{"space_id"},
	SortKey: []string{"room_id"},
})

// room_mention_counts is a counter table (see migrations/017_mentions.cql) - counter
// columns can't be mixed with normal columns via gocqlx's table helper, so it's queried
// with raw CQL below rather than through the table.Table helpers the other tables use.

type Repository interface {
	Create(ctx context.Context, r *Room, participantIDs []int64) error
	AddParticipant(ctx context.Context, roomID, userID int64) error
	RemoveParticipant(ctx context.Context, roomID, userID int64) error
	GetByID(ctx context.Context, id int64) (*Room, error)
	// GetByIDs fetches several rooms in one query; the result is in input order with nil
	// for ids that do not exist.
	GetByIDs(ctx context.Context, ids []int64) ([]*Room, error)
	GetParticipants(ctx context.Context, roomID int64) ([]int64, error)
	GetPMRoom(ctx context.Context, userA, userB int64) (*Room, error)
	GetRoomRow(ctx context.Context, userID, roomID int64) (*RoomRow, error)
	ListByUser(ctx context.Context, userID int64) ([]RoomRow, error)
	UpdateLastMessageID(ctx context.Context, roomID int64, participants []int64, msgID int64) error
	// ClearLastMessageID nulls last_message_id on the room and every participant's rooms_by_user
	// row - used when the room's newest (and only) message is deleted, leaving it empty.
	ClearLastMessageID(ctx context.Context, roomID int64, participants []int64) error
	UpdateReadState(ctx context.Context, userID, roomID, lastReadMessageID int64) error
	// IncrementMentionCounts bumps room_mention_counts by 1 for each user (e.g. everyone a new message mentions).
	IncrementMentionCounts(ctx context.Context, roomID int64, userIDs []int64) error
	// SetMentionCountBaseline snapshots the counter's current total as of an ack - see
	// RoomRow.DisplayMentionCount. Deliberately not a DELETE on the counter row: Cassandra/
	// Scylla counters can silently lose an increment that lands shortly after a delete
	// (tombstone interaction), so a resettable "baseline" on the non-counter table is the
	// idiomatic workaround.
	SetMentionCountBaseline(ctx context.Context, userID, roomID, baseline int64) error
	// GetMentionCount reads a single user+room mention count (raw total, not baseline-adjusted).
	GetMentionCount(ctx context.Context, userID, roomID int64) (int, error)
	// GetMentionCounts reads every room's raw mention total for a user in one partition query.
	GetMentionCounts(ctx context.Context, userID int64) (map[int64]int, error)
	UpdateRoomName(ctx context.Context, roomID int64, name string) error
	UpdateRoomE2EEEnabled(ctx context.Context, roomID int64, enabled bool) error
	UpdateSpaceRoom(ctx context.Context, roomID int64, name, topic string, slowmodeSeconds int) error
	UpdateVoiceSettings(ctx context.Context, roomID int64, userLimit, bitrate int) error
	DeleteSpaceRoom(ctx context.Context, spaceID, roomID int64) error
	ListBySpace(ctx context.Context, spaceID int64) ([]RoomBySpaceRow, error)
	CreateSpaceRoom(ctx context.Context, room *Room) error
	UpdateSpaceRoomPosition(ctx context.Context, spaceID, roomID int64, position int) error
	// UpdateSpaceRoomParentAndPosition updates parent_id and position in rooms, and position in rooms_by_space (space channel only).
	UpdateSpaceRoomParentAndPosition(ctx context.Context, spaceID, roomID int64, parentID *int64, position int) error
	// SetRoomNotifySettings writes this user's mute/notify-mode override for roomID.
	SetRoomNotifySettings(ctx context.Context, userID, roomID int64, muted bool, mutedUntil *time.Time, notifyMode int) error
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func minMax(a, b int64) (min, max int64) {
	if a < b {
		return a, b
	}
	return b, a
}

func (r *repo) Create(ctx context.Context, room *Room, participantIDs []int64) error {
	now := time.Now().UTC()
	room.CreatedAt = now
	room.UpdatedAt = now

	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	b.Query(insertRoomStmt, room.ID, room.Type, room.SpaceID, room.ParentID, room.Name, room.Topic, room.SlowmodeSeconds, room.Position, room.CreatorID, room.E2EEEnabled, room.LastMessageID, room.UserLimit, room.Bitrate, room.CreatedAt, room.UpdatedAt)

	stmt, _ := participantsTable.Insert()
	for _, uid := range participantIDs {
		b.Query(stmt, room.ID, uid, now)
	}
	for _, uid := range participantIDs {
		b.Query(insertRoomsByUserStmt, uid, room.ID, room.LastMessageID, nil, 0, now, nil)
	}
	if room.Type == TypePM {
		if len(participantIDs) == 2 {
			ua, ub := minMax(participantIDs[0], participantIDs[1])
			stmt, _ = pmRoomsTable.Insert()
			b.Query(stmt, ua, ub, room.ID, now)
		} else if len(participantIDs) == 1 {
			// Notes room: self-PM with one participant
			uid := participantIDs[0]
			stmt, _ = pmRoomsTable.Insert()
			b.Query(stmt, uid, uid, room.ID, now)
		}
	}
	return r.session.ExecuteBatch(b)
}

func (r *repo) AddParticipant(ctx context.Context, roomID, userID int64) error {
	room, err := r.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return err
	}
	now := time.Now().UTC()
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := participantsTable.Insert()
	b.Query(stmt, roomID, userID, now)
	b.Query(insertRoomsByUserStmt, userID, roomID, room.LastMessageID, nil, 0, now, nil)
	return r.session.ExecuteBatch(b)
}

func (r *repo) RemoveParticipant(ctx context.Context, roomID, userID int64) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := participantsTable.Delete()
	b.Query(stmt, roomID, userID)
	stmt, _ = roomsByUserTable.Delete()
	b.Query(stmt, userID, roomID)
	return r.session.ExecuteBatch(b)
}

func (r *repo) GetByID(ctx context.Context, id int64) (*Room, error) {
	var room Room
	stmt, names := roomsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(id).GetRelease(&room); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &room, nil
}

func (r *repo) GetByIDs(ctx context.Context, ids []int64) ([]*Room, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, rid := range ids {
		if _, ok := seen[rid]; !ok {
			seen[rid] = struct{}{}
			unique = append(unique, rid)
		}
	}
	stmt := "SELECT " + strings.Join(roomsTable.Metadata().Columns, ", ") + " FROM rooms WHERE id IN ?"
	q := r.session.Query(stmt, nil).WithContext(ctx).Bind(unique)
	defer q.Release()
	var rows []Room
	if err := q.Select(&rows); err != nil {
		return nil, err
	}
	byID := make(map[int64]*Room, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	out := make([]*Room, len(ids))
	for i, rid := range ids {
		out[i] = byID[rid]
	}
	return out, nil
}

func (r *repo) GetParticipants(ctx context.Context, roomID int64) ([]int64, error) {
	stmt, names := participantsTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(roomID).Iter()
	defer iter.Close()

	var ids []int64
	var row struct {
		UserID int64 `db:"user_id"`
	}
	for iter.StructScan(&row) {
		ids = append(ids, row.UserID)
	}
	return ids, iter.Close()
}

func (r *repo) GetPMRoom(ctx context.Context, userA, userB int64) (*Room, error) {
	ua, ub := minMax(userA, userB)
	var row PMRoomsRow
	stmt, names := pmRoomsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(ua, ub).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return r.GetByID(ctx, row.RoomID)
}

func (r *repo) GetRoomRow(ctx context.Context, userID, roomID int64) (*RoomRow, error) {
	var row RoomRow
	stmt, names := roomsByUserTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(userID, roomID).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

func (r *repo) ListByUser(ctx context.Context, userID int64) ([]RoomRow, error) {
	stmt, names := roomsByUserTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID).Iter()
	defer iter.Close()

	var rows []RoomRow
	var row RoomRow
	for iter.StructScan(&row) {
		rows = append(rows, row)
	}
	return rows, iter.Close()
}

func (r *repo) UpdateLastMessageID(ctx context.Context, roomID int64, participants []int64, msgID int64) error {
	now := time.Now().UTC()
	stmtRoom, namesRoom := roomsTable.Update("last_message_id", "updated_at")
	q := r.session.Query(stmtRoom, namesRoom).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(msgID, now, roomID).ExecRelease(); err != nil {
		return err
	}
	if len(participants) == 0 {
		return nil
	}
	// Space channels can fan out to many members; chunk to stay within batch limits.
	const chunkSize = 50
	stmtUser, _ := roomsByUserTable.Update("last_message_id")
	for i := 0; i < len(participants); i += chunkSize {
		end := i + chunkSize
		if end > len(participants) {
			end = len(participants)
		}
		b := r.session.Batch(gocql.UnloggedBatch).WithContext(ctx)
		for _, uid := range participants[i:end] {
			b.Query(stmtUser, msgID, uid, roomID)
		}
		if err := r.session.ExecuteBatch(b); err != nil {
			return err
		}
	}
	return nil
}

func (r *repo) ClearLastMessageID(ctx context.Context, roomID int64, participants []int64) error {
	now := time.Now().UTC()
	stmtRoom, namesRoom := roomsTable.Update("last_message_id", "updated_at")
	q := r.session.Query(stmtRoom, namesRoom).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(nil, now, roomID).ExecRelease(); err != nil {
		return err
	}
	if len(participants) == 0 {
		return nil
	}
	const chunkSize = 50
	stmtUser, _ := roomsByUserTable.Update("last_message_id")
	for i := 0; i < len(participants); i += chunkSize {
		end := i + chunkSize
		if end > len(participants) {
			end = len(participants)
		}
		b := r.session.Batch(gocql.UnloggedBatch).WithContext(ctx)
		for _, uid := range participants[i:end] {
			b.Query(stmtUser, nil, uid, roomID)
		}
		if err := r.session.ExecuteBatch(b); err != nil {
			return err
		}
	}
	return nil
}

func (r *repo) UpdateReadState(ctx context.Context, userID, roomID, lastReadMessageID int64) error {
	stmt, names := roomsByUserTable.Update("last_read_message_id", "mention_count")
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(lastReadMessageID, 0, userID, roomID).ExecRelease()
}

func (r *repo) SetRoomNotifySettings(ctx context.Context, userID, roomID int64, muted bool, mutedUntil *time.Time, notifyMode int) error {
	stmt, names := roomsByUserTable.Update("muted", "muted_until", "notify_mode")
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(muted, mutedUntil, notifyMode, userID, roomID).ExecRelease()
}

// IncrementMentionCounts bumps room_mention_counts by 1 for each user. Counter updates
// can't share a batch with non-counter tables, so this is its own batch, chunked like
// UpdateLastMessageID since a space's mention fan-out (e.g. @everyone) can be large.
func (r *repo) IncrementMentionCounts(ctx context.Context, roomID int64, userIDs []int64) error {
	if len(userIDs) == 0 {
		return nil
	}
	const stmt = "UPDATE room_mention_counts SET count = count + 1 WHERE user_id = ? AND room_id = ?"
	const chunkSize = 50
	for i := 0; i < len(userIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(userIDs) {
			end = len(userIDs)
		}
		b := r.session.Batch(gocql.CounterBatch).WithContext(ctx)
		for _, uid := range userIDs[i:end] {
			b.Query(stmt, uid, roomID)
		}
		if err := r.session.ExecuteBatch(b); err != nil {
			return err
		}
	}
	return nil
}

func (r *repo) SetMentionCountBaseline(ctx context.Context, userID, roomID, baseline int64) error {
	stmt, names := roomsByUserTable.Update("mention_count_baseline")
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(baseline, userID, roomID).ExecRelease()
}

func (r *repo) GetMentionCount(ctx context.Context, userID, roomID int64) (int, error) {
	var count int
	q := r.session.Session.Query(
		"SELECT count FROM room_mention_counts WHERE user_id = ? AND room_id = ?",
		userID, roomID,
	).WithContext(ctx)
	defer q.Release()
	if err := q.Scan(&count); err != nil {
		if err == gocql.ErrNotFound {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

func (r *repo) GetMentionCounts(ctx context.Context, userID int64) (map[int64]int, error) {
	q := r.session.Session.Query(
		"SELECT room_id, count FROM room_mention_counts WHERE user_id = ?",
		userID,
	).WithContext(ctx)
	defer q.Release()
	iter := q.Iter()
	out := make(map[int64]int)
	var roomID int64
	var count int
	for iter.Scan(&roomID, &count) {
		out[roomID] = count
	}
	return out, iter.Close()
}

func (r *repo) UpdateRoomName(ctx context.Context, roomID int64, name string) error {
	now := time.Now().UTC()
	stmt, names := roomsTable.Update("name", "updated_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(name, now, roomID).ExecRelease()
}

func (r *repo) UpdateRoomE2EEEnabled(ctx context.Context, roomID int64, enabled bool) error {
	now := time.Now().UTC()
	stmt, names := roomsTable.Update("e2ee_enabled", "updated_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(enabled, now, roomID).ExecRelease()
}

func (r *repo) ListBySpace(ctx context.Context, spaceID int64) ([]RoomBySpaceRow, error) {
	stmt, names := roomsBySpaceTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(spaceID).Iter()
	defer iter.Close()
	var out []RoomBySpaceRow
	var row RoomBySpaceRow
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	return out, iter.Close()
}

// CreateSpaceRoom inserts a space room (text, voice, or section) and adds it to rooms_by_space. No participants.
func (r *repo) CreateSpaceRoom(ctx context.Context, room *Room) error {
	if room.SpaceID == nil {
		return nil
	}
	now := time.Now().UTC()
	room.CreatedAt = now
	room.UpdatedAt = now
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	b.Query(insertRoomStmt, room.ID, room.Type, room.SpaceID, room.ParentID, room.Name, room.Topic, room.SlowmodeSeconds, room.Position, room.CreatorID, room.E2EEEnabled, room.LastMessageID, room.UserLimit, room.Bitrate, room.CreatedAt, room.UpdatedAt)
	stmt, _ := roomsBySpaceTable.Insert()
	b.Query(stmt, *room.SpaceID, room.ID, room.Position, now)
	return r.session.ExecuteBatch(b)
}

func (r *repo) UpdateSpaceRoom(ctx context.Context, roomID int64, name, topic string, slowmodeSeconds int) error {
	now := time.Now().UTC()
	q := r.session.Session.Query(
		"UPDATE rooms SET name = ?, topic = ?, slowmode_seconds = ?, updated_at = ? WHERE id = ?",
		name, topic, slowmodeSeconds, now, roomID,
	).WithContext(ctx)
	err := q.Exec()
	q.Release()
	return err
}

// UpdateVoiceSettings writes a voice room's user limit and bitrate.
func (r *repo) UpdateVoiceSettings(ctx context.Context, roomID int64, userLimit, bitrate int) error {
	now := time.Now().UTC()
	q := r.session.Session.Query(
		"UPDATE rooms SET user_limit = ?, bitrate = ?, updated_at = ? WHERE id = ?",
		userLimit, bitrate, now, roomID,
	).WithContext(ctx)
	err := q.Exec()
	q.Release()
	return err
}

// UpdateSpaceRoomPosition updates ordering in both rooms and rooms_by_space.
func (r *repo) UpdateSpaceRoomPosition(ctx context.Context, spaceID, roomID int64, position int) error {
	now := time.Now().UTC()
	q1 := r.session.Session.Query(
		"UPDATE rooms SET position = ?, updated_at = ? WHERE id = ?",
		position, now, roomID,
	).WithContext(ctx)
	if err := q1.Exec(); err != nil {
		q1.Release()
		return err
	}
	q1.Release()
	q2 := r.session.Session.Query(
		"UPDATE rooms_by_space SET position = ? WHERE space_id = ? AND room_id = ?",
		position, spaceID, roomID,
	).WithContext(ctx)
	err := q2.Exec()
	q2.Release()
	return err
}

func (r *repo) UpdateSpaceRoomParentAndPosition(ctx context.Context, spaceID, roomID int64, parentID *int64, position int) error {
	now := time.Now().UTC()
	q1 := r.session.Session.Query(
		"UPDATE rooms SET parent_id = ?, position = ?, updated_at = ? WHERE id = ?",
		parentID, position, now, roomID,
	).WithContext(ctx)
	if err := q1.Exec(); err != nil {
		q1.Release()
		return err
	}
	q1.Release()
	q2 := r.session.Session.Query(
		"UPDATE rooms_by_space SET position = ? WHERE space_id = ? AND room_id = ?",
		position, spaceID, roomID,
	).WithContext(ctx)
	err := q2.Exec()
	q2.Release()
	return err
}

func (r *repo) DeleteSpaceRoom(ctx context.Context, spaceID, roomID int64) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := roomsTable.Delete()
	b.Query(stmt, roomID)
	stmt, _ = roomsBySpaceTable.Delete()
	b.Query(stmt, spaceID, roomID)
	// Remove all per-room override rows.
	b.Query("DELETE FROM space_room_role_overrides WHERE space_id = ? AND room_id = ?", spaceID, roomID)
	b.Query("DELETE FROM space_room_user_overrides WHERE space_id = ? AND room_id = ?", spaceID, roomID)
	return r.session.ExecuteBatch(b)
}
