package federation

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

// RoomMapping ties a local room to its global identity.
type RoomMapping struct {
	RoomID       int64     `db:"room_id"`
	OriginDomain string    `db:"origin_domain"`
	OriginRoomID int64     `db:"origin_room_id"`
	CreatedAt    time.Time `db:"created_at"`
}

type roomByOrigin struct {
	OriginDomain string `db:"origin_domain"`
	OriginRoomID int64  `db:"origin_room_id"`
	RoomID       int64  `db:"room_id"`
}

// MessageMapping ties a local message to the id its origin instance uses.
type MessageMapping struct {
	RoomID          int64  `db:"room_id"`
	MessageID       int64  `db:"message_id"`
	OriginDomain    string `db:"origin_domain"`
	OriginMessageID int64  `db:"origin_message_id"`
}

type messageByOrigin struct {
	OriginDomain    string `db:"origin_domain"`
	OriginMessageID int64  `db:"origin_message_id"`
	RoomID          int64  `db:"room_id"`
	MessageID       int64  `db:"message_id"`
}

// Peer is a remote instance we've talked to.
type Peer struct {
	Domain        string    `db:"domain"`
	KeyID         string    `db:"key_id"`
	PublicKey     string    `db:"public_key"`
	FederationURL string    `db:"federation_url"`
	APIURL        string    `db:"api_url"`
	FirstSeen     time.Time `db:"first_seen"`
	LastSeen      time.Time `db:"last_seen"`
	Blocked       bool      `db:"blocked"`
}

var (
	roomsTable = table.New(table.Metadata{
		Name:    "federated_rooms",
		Columns: []string{"room_id", "origin_domain", "origin_room_id", "created_at"},
		PartKey: []string{"room_id"},
	})
	roomsByOriginTable = table.New(table.Metadata{
		Name:    "federated_rooms_by_origin",
		Columns: []string{"origin_domain", "origin_room_id", "room_id"},
		PartKey: []string{"origin_domain"},
		SortKey: []string{"origin_room_id"},
	})
	messagesTable = table.New(table.Metadata{
		Name:    "federated_messages",
		Columns: []string{"room_id", "message_id", "origin_domain", "origin_message_id"},
		PartKey: []string{"room_id"},
		SortKey: []string{"message_id"},
	})
	messagesByOriginTable = table.New(table.Metadata{
		Name:    "federated_messages_by_origin",
		Columns: []string{"origin_domain", "origin_message_id", "room_id", "message_id"},
		PartKey: []string{"origin_domain"},
		SortKey: []string{"origin_message_id"},
	})
	peersTable = table.New(table.Metadata{
		Name:    "federation_peers",
		Columns: []string{"domain", "key_id", "public_key", "federation_url", "api_url", "first_seen", "last_seen", "blocked"},
		PartKey: []string{"domain"},
	})
)

type Repository interface {
	GetRoomMapping(ctx context.Context, roomID int64) (*RoomMapping, error)
	GetRoomByOrigin(ctx context.Context, originDomain string, originRoomID int64) (*RoomMapping, error)
	PutRoomMapping(ctx context.Context, m *RoomMapping) error

	GetMessageMapping(ctx context.Context, roomID, messageID int64) (*MessageMapping, error)
	GetMessageByOrigin(ctx context.Context, originDomain string, originMessageID int64) (*MessageMapping, error)
	PutMessageMapping(ctx context.Context, m *MessageMapping) error

	GetPeer(ctx context.Context, domain string) (*Peer, error)
	UpsertPeer(ctx context.Context, p *Peer) error
	ListPeers(ctx context.Context) ([]Peer, error)
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) GetRoomMapping(ctx context.Context, roomID int64) (*RoomMapping, error) {
	var m RoomMapping
	stmt, names := roomsTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(roomID).GetRelease(&m); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *repo) GetRoomByOrigin(ctx context.Context, originDomain string, originRoomID int64) (*RoomMapping, error) {
	var row roomByOrigin
	stmt, names := roomsByOriginTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(originDomain, originRoomID).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &RoomMapping{RoomID: row.RoomID, OriginDomain: row.OriginDomain, OriginRoomID: row.OriginRoomID}, nil
}

func (r *repo) PutRoomMapping(ctx context.Context, m *RoomMapping) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	stmt, names := roomsTable.Insert()
	if err := r.session.Query(stmt, names).WithContext(ctx).BindStruct(m).ExecRelease(); err != nil {
		return err
	}
	row := roomByOrigin{OriginDomain: m.OriginDomain, OriginRoomID: m.OriginRoomID, RoomID: m.RoomID}
	stmt, names = roomsByOriginTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(&row).ExecRelease()
}

func (r *repo) GetMessageMapping(ctx context.Context, roomID, messageID int64) (*MessageMapping, error) {
	var m MessageMapping
	stmt, names := messagesTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(roomID, messageID).GetRelease(&m); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *repo) GetMessageByOrigin(ctx context.Context, originDomain string, originMessageID int64) (*MessageMapping, error) {
	var row messageByOrigin
	stmt, names := messagesByOriginTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(originDomain, originMessageID).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &MessageMapping{RoomID: row.RoomID, MessageID: row.MessageID, OriginDomain: row.OriginDomain, OriginMessageID: row.OriginMessageID}, nil
}

func (r *repo) PutMessageMapping(ctx context.Context, m *MessageMapping) error {
	stmt, names := messagesTable.Insert()
	if err := r.session.Query(stmt, names).WithContext(ctx).BindStruct(m).ExecRelease(); err != nil {
		return err
	}
	row := messageByOrigin{OriginDomain: m.OriginDomain, OriginMessageID: m.OriginMessageID, RoomID: m.RoomID, MessageID: m.MessageID}
	stmt, names = messagesByOriginTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(&row).ExecRelease()
}

func (r *repo) GetPeer(ctx context.Context, domain string) (*Peer, error) {
	var p Peer
	stmt, names := peersTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(domain).GetRelease(&p); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (r *repo) UpsertPeer(ctx context.Context, p *Peer) error {
	stmt, names := peersTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(p).ExecRelease()
}

func (r *repo) ListPeers(ctx context.Context) ([]Peer, error) {
	stmt, names := peersTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var out []Peer
	if err := q.SelectRelease(&out); err != nil {
		return nil, err
	}
	return out, nil
}
