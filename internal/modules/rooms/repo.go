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
	Columns: []string{"id", "type", "space_id", "parent_id", "name", "topic", "position", "creator_id", "e2ee_enabled", "last_message_id", "created_at", "updated_at"},
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
	Columns: []string{"user_id", "room_id", "last_message_id", "last_read_message_id", "mention_count", "joined_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"room_id"},
})

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

type Repository interface {
	Create(ctx context.Context, r *Room, participantIDs []int64) error
	AddParticipant(ctx context.Context, roomID, userID int64) error
	RemoveParticipant(ctx context.Context, roomID, userID int64) error
	GetByID(ctx context.Context, id int64) (*Room, error)
	GetParticipants(ctx context.Context, roomID int64) ([]int64, error)
	GetPMRoom(ctx context.Context, userA, userB int64) (*Room, error)
	GetRoomRow(ctx context.Context, userID, roomID int64) (*RoomRow, error)
	ListByUser(ctx context.Context, userID int64) ([]RoomRow, error)
	UpdateLastMessageID(ctx context.Context, roomID int64, participants []int64, msgID int64) error
	UpdateReadState(ctx context.Context, userID, roomID, lastReadMessageID int64) error
	UpdateRoomName(ctx context.Context, roomID int64, name string) error
	UpdateRoomE2EEEnabled(ctx context.Context, roomID int64, enabled bool) error
	ListBySpace(ctx context.Context, spaceID int64) ([]RoomBySpaceRow, error)
	CreateSpaceRoom(ctx context.Context, room *Room) error
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

	b := r.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := roomsTable.Insert()
	b.Query(stmt, room.ID, room.Type, room.SpaceID, room.ParentID, room.Name, room.Topic, room.Position, room.CreatorID, room.E2EEEnabled, room.LastMessageID, room.CreatedAt, room.UpdatedAt)

	stmt, _ = participantsTable.Insert()
	for _, uid := range participantIDs {
		b.Query(stmt, room.ID, uid, now)
	}
	stmt, _ = roomsByUserTable.Insert()
	for _, uid := range participantIDs {
		b.Query(stmt, uid, room.ID, room.LastMessageID, nil, 0, now)
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
	b := r.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := participantsTable.Insert()
	b.Query(stmt, roomID, userID, now)
	stmt, _ = roomsByUserTable.Insert()
	b.Query(stmt, userID, roomID, room.LastMessageID, nil, 0, now)
	return r.session.ExecuteBatch(b)
}

func (r *repo) RemoveParticipant(ctx context.Context, roomID, userID int64) error {
	b := r.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
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
	b := r.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := roomsTable.Update("last_message_id", "updated_at")
	now := time.Now().UTC()
	b.Query(stmt, msgID, now, roomID)
	stmt, _ = roomsByUserTable.Update("last_message_id")
	for _, uid := range participants {
		b.Query(stmt, msgID, uid, roomID)
	}
	return r.session.ExecuteBatch(b)
}

func (r *repo) UpdateReadState(ctx context.Context, userID, roomID, lastReadMessageID int64) error {
	stmt, names := roomsByUserTable.Update("last_read_message_id", "mention_count")
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(lastReadMessageID, 0, userID, roomID).ExecRelease()
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
	b := r.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := roomsTable.Insert()
	b.Query(stmt, room.ID, room.Type, room.SpaceID, room.ParentID, room.Name, room.Topic, room.Position, room.CreatorID, room.E2EEEnabled, room.LastMessageID, room.CreatedAt, room.UpdatedAt)
	stmt, _ = roomsBySpaceTable.Insert()
	b.Query(stmt, *room.SpaceID, room.ID, room.Position, now)
	return r.session.ExecuteBatch(b)
}
