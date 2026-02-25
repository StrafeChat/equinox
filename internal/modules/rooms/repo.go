package rooms

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var roomsTable = table.New(table.Metadata{
	Name:    "rooms",
	Columns: []string{"id", "type", "space_id", "parent_id", "name", "topic", "position", "last_message_id", "created_at", "updated_at"},
	PartKey: []string{"id"},
})

var participantsTable = table.New(table.Metadata{
	Name:    "room_participants",
	Columns: []string{"room_id", "user_id", "joined_at"},
	PartKey: []string{"room_id"},
	SortKey: []string{"user_id"},
})

var roomsByUserTable = table.New(table.Metadata{
	Name:    "rooms_by_user",
	Columns: []string{"user_id", "room_id", "last_message_id", "joined_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"room_id"},
})

var pmRoomsTable = table.New(table.Metadata{
	Name:    "pm_rooms",
	Columns: []string{"user_a_id", "user_b_id", "room_id", "created_at"},
	PartKey: []string{"user_a_id"},
	SortKey: []string{"user_b_id"},
})

type Repository interface {
	Create(ctx context.Context, r *Room, participantIDs []int64) error
	GetByID(ctx context.Context, id int64) (*Room, error)
	GetParticipants(ctx context.Context, roomID int64) ([]int64, error)
	GetPMRoom(ctx context.Context, userA, userB int64) (*Room, error)
	ListByUser(ctx context.Context, userID int64) ([]RoomRow, error)
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

	stmt, names := roomsTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	if err := q.BindStruct(room).ExecRelease(); err != nil {
		return err
	}

	b := r.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	stmt, names = participantsTable.Insert()
	for _, uid := range participantIDs {
		b.Query(stmt, room.ID, uid, now)
	}
	if err := r.session.ExecuteBatch(b); err != nil {
		return err
	}

	for _, uid := range participantIDs {
		stmt, names = roomsByUserTable.Insert()
		q = r.session.Query(stmt, names).WithContext(ctx)
		if err := q.Bind(uid, room.ID, room.LastMessageID, now).ExecRelease(); err != nil {
			return err
		}
	}

	if room.Type == TypePM && len(participantIDs) == 2 {
		ua, ub := minMax(participantIDs[0], participantIDs[1])
		stmt, names = pmRoomsTable.Insert()
		q = r.session.Query(stmt, names).WithContext(ctx)
		return q.Bind(ua, ub, room.ID, now).ExecRelease()
	}

	return nil
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
