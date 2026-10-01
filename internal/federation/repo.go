package federation

import (
	"context"
	"strings"
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

// SpaceMapping ties a local space (a mirror, or a hosted space that has started
// federating) to its global identity.
type SpaceMapping struct {
	SpaceID       int64     `db:"space_id"`
	OriginDomain  string    `db:"origin_domain"`
	OriginSpaceID int64     `db:"origin_space_id"`
	CreatedAt     time.Time `db:"created_at"`
}

type spaceByOrigin struct {
	OriginDomain  string `db:"origin_domain"`
	OriginSpaceID int64  `db:"origin_space_id"`
	SpaceID       int64  `db:"space_id"`
}

// SpacePeer is an instance that mirrors a space this instance hosts.
type SpacePeer struct {
	SpaceID   int64     `db:"space_id"`
	Domain    string    `db:"domain"`
	CreatedAt time.Time `db:"created_at"`
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
	spacesTable = table.New(table.Metadata{
		Name:    "federated_spaces",
		Columns: []string{"space_id", "origin_domain", "origin_space_id", "created_at"},
		PartKey: []string{"space_id"},
	})
	spacesByOriginTable = table.New(table.Metadata{
		Name:    "federated_spaces_by_origin",
		Columns: []string{"origin_domain", "origin_space_id", "space_id"},
		PartKey: []string{"origin_domain"},
		SortKey: []string{"origin_space_id"},
	})
	spacePeersTable = table.New(table.Metadata{
		Name:    "federated_space_peers",
		Columns: []string{"space_id", "domain", "created_at"},
		PartKey: []string{"space_id"},
		SortKey: []string{"domain"},
	})
)

type Repository interface {
	GetRoomMapping(ctx context.Context, roomID int64) (*RoomMapping, error)
	// GetRoomMappings is GetRoomMapping for many rooms in one query (missing ids absent).
	GetRoomMappings(ctx context.Context, roomIDs []int64) (map[int64]*RoomMapping, error)
	GetRoomByOrigin(ctx context.Context, originDomain string, originRoomID int64) (*RoomMapping, error)
	PutRoomMapping(ctx context.Context, m *RoomMapping) error
	DeleteRoomMapping(ctx context.Context, m *RoomMapping) error

	GetSpaceMapping(ctx context.Context, spaceID int64) (*SpaceMapping, error)
	GetSpaceByOrigin(ctx context.Context, originDomain string, originSpaceID int64) (*SpaceMapping, error)
	PutSpaceMapping(ctx context.Context, m *SpaceMapping) error
	DeleteSpaceMapping(ctx context.Context, m *SpaceMapping) error
	// ListSpaceMappings scans every mapping (mirrors and hosted spaces alike).
	ListSpaceMappings(ctx context.Context) ([]SpaceMapping, error)

	// Space peers: the instances mirroring a space this instance hosts.
	ListSpacePeers(ctx context.Context, spaceID int64) ([]string, error)
	IsSpacePeer(ctx context.Context, spaceID int64, domain string) (bool, error)
	AddSpacePeer(ctx context.Context, spaceID int64, domain string) error
	RemoveSpacePeer(ctx context.Context, spaceID int64, domain string) error
	DeleteSpacePeers(ctx context.Context, spaceID int64) error

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

func (r *repo) GetRoomMappings(ctx context.Context, roomIDs []int64) (map[int64]*RoomMapping, error) {
	out := make(map[int64]*RoomMapping, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}
	q := r.session.Query("SELECT room_id, origin_domain, origin_room_id, created_at FROM federated_rooms WHERE room_id IN ?", nil).
		WithContext(ctx).Bind(roomIDs)
	defer q.Release()
	var rows []RoomMapping
	if err := q.Select(&rows); err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].RoomID] = &rows[i]
	}
	return out, nil
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

func (r *repo) DeleteRoomMapping(ctx context.Context, m *RoomMapping) error {
	stmt, names := roomsTable.Delete()
	if err := r.session.Query(stmt, names).WithContext(ctx).Bind(m.RoomID).ExecRelease(); err != nil {
		return err
	}
	stmt, names = roomsByOriginTable.Delete()
	return r.session.Query(stmt, names).WithContext(ctx).Bind(m.OriginDomain, m.OriginRoomID).ExecRelease()
}

func (r *repo) GetSpaceMapping(ctx context.Context, spaceID int64) (*SpaceMapping, error) {
	var m SpaceMapping
	stmt, names := spacesTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(spaceID).GetRelease(&m); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *repo) GetSpaceByOrigin(ctx context.Context, originDomain string, originSpaceID int64) (*SpaceMapping, error) {
	var row spaceByOrigin
	stmt, names := spacesByOriginTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(originDomain, originSpaceID).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &SpaceMapping{SpaceID: row.SpaceID, OriginDomain: row.OriginDomain, OriginSpaceID: row.OriginSpaceID}, nil
}

func (r *repo) PutSpaceMapping(ctx context.Context, m *SpaceMapping) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	stmt, names := spacesTable.Insert()
	if err := r.session.Query(stmt, names).WithContext(ctx).BindStruct(m).ExecRelease(); err != nil {
		return err
	}
	row := spaceByOrigin{OriginDomain: m.OriginDomain, OriginSpaceID: m.OriginSpaceID, SpaceID: m.SpaceID}
	stmt, names = spacesByOriginTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(&row).ExecRelease()
}

func (r *repo) DeleteSpaceMapping(ctx context.Context, m *SpaceMapping) error {
	stmt, names := spacesTable.Delete()
	if err := r.session.Query(stmt, names).WithContext(ctx).Bind(m.SpaceID).ExecRelease(); err != nil {
		return err
	}
	stmt, names = spacesByOriginTable.Delete()
	return r.session.Query(stmt, names).WithContext(ctx).Bind(m.OriginDomain, m.OriginSpaceID).ExecRelease()
}

func (r *repo) ListSpaceMappings(ctx context.Context) ([]SpaceMapping, error) {
	// A whole-table scan: Table.Select() would select by partition key and panic on the
	// missing bind value.
	stmt := "SELECT " + strings.Join(spacesTable.Metadata().Columns, ", ") + " FROM federated_spaces"
	q := r.session.Query(stmt, nil).WithContext(ctx)
	defer q.Release()
	var out []SpaceMapping
	if err := q.SelectRelease(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) ListSpacePeers(ctx context.Context, spaceID int64) ([]string, error) {
	stmt, names := spacePeersTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var rows []SpacePeer
	if err := q.Bind(spaceID).SelectRelease(&rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.Domain)
	}
	return out, nil
}

func (r *repo) IsSpacePeer(ctx context.Context, spaceID int64, domain string) (bool, error) {
	var p SpacePeer
	stmt, names := spacePeersTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(spaceID, domain).GetRelease(&p); err != nil {
		if err == gocql.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *repo) AddSpacePeer(ctx context.Context, spaceID int64, domain string) error {
	p := SpacePeer{SpaceID: spaceID, Domain: domain, CreatedAt: time.Now().UTC()}
	stmt, names := spacePeersTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(&p).ExecRelease()
}

func (r *repo) RemoveSpacePeer(ctx context.Context, spaceID int64, domain string) error {
	stmt, names := spacePeersTable.Delete()
	return r.session.Query(stmt, names).WithContext(ctx).Bind(spaceID, domain).ExecRelease()
}

func (r *repo) DeleteSpacePeers(ctx context.Context, spaceID int64) error {
	return r.session.Query("DELETE FROM federated_space_peers WHERE space_id = ?", nil).WithContext(ctx).Bind(spaceID).ExecRelease()
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
	stmt := "SELECT " + strings.Join(peersTable.Metadata().Columns, ", ") + " FROM federation_peers"
	q := r.session.Query(stmt, nil).WithContext(ctx)
	defer q.Release()
	var out []Peer
	if err := q.SelectRelease(&out); err != nil {
		return nil, err
	}
	return out, nil
}
