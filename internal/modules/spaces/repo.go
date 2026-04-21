package spaces

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

var spacesTable = table.New(table.Metadata{
	Name: "spaces",
	Columns: []string{
		"id", "name", "name_acronym", "description", "icon", "banner", "owner_id",
		"verification_level", "default_message_notifications", "explicit_content_filter",
		"features", "afk_room_id", "afk_timeout", "system_room_id", "system_room_flags",
		"rules_room_id", "max_presences", "max_members", "vanity_url_code",
		"preferred_locale", "public_updates_room_id", "max_video_room_users",
		"everyone_role_id", "created_at", "updated_at",
	},
	PartKey: []string{"id"},
})

var spaceMembersTable = table.New(table.Metadata{
	Name:    "space_members",
	Columns: []string{"space_id", "user_id", "role_ids", "nickname", "joined_at"},
	PartKey: []string{"space_id"},
	SortKey: []string{"user_id"},
})

var spacesByUserTable = table.New(table.Metadata{
	Name:    "spaces_by_user",
	Columns: []string{"user_id", "space_id", "joined_at"},
	PartKey: []string{"user_id"},
	SortKey: []string{"space_id"},
})

var spaceInvitesTable = table.New(table.Metadata{
	Name:    "space_invites",
	Columns: []string{"code", "space_id", "inviter_id", "created_at"},
	PartKey: []string{"code"},
})

type Repository interface {
	Create(ctx context.Context, s *Space) error
	GetByID(ctx context.Context, id int64) (*Space, error)
	AddMember(ctx context.Context, spaceID, userID int64, roleIDs []int64) error
	ListSpacesByUser(ctx context.Context, userID int64) ([]SpaceMemberRow, error)
	AddSpaceToUserSet(ctx context.Context, userID int64, spaceID int64) error
	IsMember(ctx context.Context, spaceID, userID int64) (bool, error)
	ListMembers(ctx context.Context, spaceID int64) ([]SpaceMember, error)
	CreateInvite(ctx context.Context, inv *SpaceInvite) error
	GetInviteByCode(ctx context.Context, code string) (*SpaceInvite, error)
	DeleteInvite(ctx context.Context, code string) error

	UpdateEveryoneRoleID(ctx context.Context, spaceID, roleID int64) error
	GetMember(ctx context.Context, spaceID, userID int64) (*SpaceMember, error)
	SetMemberRoleIDs(ctx context.Context, spaceID, userID int64, roleIDs []int64) error

	InsertSpaceRole(ctx context.Context, r *SpaceRole) error
	GetSpaceRole(ctx context.Context, spaceID, roleID int64) (*SpaceRole, error)
	ListSpaceRoles(ctx context.Context, spaceID int64) ([]SpaceRole, error)
	UpdateSpaceRole(ctx context.Context, r *SpaceRole) error
	DeleteSpaceRole(ctx context.Context, spaceID, roleID int64) error

	UpsertRoomRoleOverride(ctx context.Context, o *SpaceRoomRoleOverride) error
	ListRoomRoleOverrides(ctx context.Context, spaceID, roomID int64) ([]SpaceRoomRoleOverride, error)
	DeleteRoomRoleOverride(ctx context.Context, spaceID, roomID, roleID int64) error

	UpsertRoomUserOverride(ctx context.Context, o *SpaceRoomUserOverride) error
	ListRoomUserOverrides(ctx context.Context, spaceID, roomID int64) ([]SpaceRoomUserOverride, error)
	DeleteRoomUserOverride(ctx context.Context, spaceID, roomID, userID int64) error

	UpdateSpaceIcon(ctx context.Context, spaceID int64, icon string, updatedAt time.Time) error
	UpdateSpaceName(ctx context.Context, spaceID int64, name, nameAcronym string, updatedAt time.Time) error
}

type repo struct {
	session gocqlx.Session
}

func NewRepository(session gocqlx.Session) Repository {
	return &repo{session: session}
}

func (r *repo) Create(ctx context.Context, s *Space) error {
	stmt, names := spacesTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(s).ExecRelease()
}

func (r *repo) GetByID(ctx context.Context, id int64) (*Space, error) {
	var s Space
	stmt, names := spacesTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(id).GetRelease(&s); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *repo) AddMember(ctx context.Context, spaceID, userID int64, roleIDs []int64) error {
	now := time.Now().UTC()
	b := r.session.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := spaceMembersTable.Insert()
	b.Query(stmt, spaceID, userID, roleIDs, "", now)
	stmt, _ = spacesByUserTable.Insert()
	b.Query(stmt, userID, spaceID, now)
	return r.session.ExecuteBatch(b)
}

func (r *repo) ListSpacesByUser(ctx context.Context, userID int64) ([]SpaceMemberRow, error) {
	stmt, names := spacesByUserTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(userID).Iter()
	defer iter.Close()
	var out []SpaceMemberRow
	var row SpaceMemberRow
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// AddSpaceToUserSet adds spaceID to the user's spaces set (users table).
func (r *repo) AddSpaceToUserSet(ctx context.Context, userID int64, spaceID int64) error {
	q := r.session.Session.Query(
		"UPDATE users SET spaces = spaces + ? WHERE id = ?",
		[]int64{spaceID}, userID,
	).WithContext(ctx)
	err := q.Exec()
	q.Release()
	return err
}

func (r *repo) IsMember(ctx context.Context, spaceID, userID int64) (bool, error) {
	stmt, names := spaceMembersTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var m SpaceMember
	if err := q.Bind(spaceID, userID).GetRelease(&m); err != nil {
		if err == gocql.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListMembers returns all members for a space.
func (r *repo) ListMembers(ctx context.Context, spaceID int64) ([]SpaceMember, error) {
	stmt, names := spaceMembersTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(spaceID).Iter()
	defer iter.Close()
	var out []SpaceMember
	var row SpaceMember
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) CreateInvite(ctx context.Context, inv *SpaceInvite) error {
	stmt, names := spaceInvitesTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.BindStruct(inv).ExecRelease()
}

func (r *repo) GetInviteByCode(ctx context.Context, code string) (*SpaceInvite, error) {
	stmt, names := spaceInvitesTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var inv SpaceInvite
	if err := q.Bind(code).GetRelease(&inv); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &inv, nil
}

func (r *repo) DeleteInvite(ctx context.Context, code string) error {
	stmt, names := spaceInvitesTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(code).ExecRelease()
}

var spaceRolesTable = table.New(table.Metadata{
	Name: "space_roles",
	Columns: []string{
		"space_id", "id", "name", "permissions", "position", "color", "hoist", "mentionable", "created_at", "updated_at",
	},
	PartKey: []string{"space_id"},
	SortKey: []string{"id"},
})

var spaceRoomRoleOverridesTable = table.New(table.Metadata{
	Name: "space_room_role_overrides",
	Columns: []string{"space_id", "room_id", "role_id", "allow_mask", "deny_mask", "created_at", "updated_at"},
	PartKey: []string{"space_id", "room_id"},
	SortKey: []string{"role_id"},
})

var spaceRoomUserOverridesTable = table.New(table.Metadata{
	Name:    "space_room_user_overrides",
	Columns: []string{"space_id", "room_id", "user_id", "allow_mask", "deny_mask", "created_at", "updated_at"},
	PartKey: []string{"space_id", "room_id"},
	SortKey: []string{"user_id"},
})

func (r *repo) UpdateEveryoneRoleID(ctx context.Context, spaceID, roleID int64) error {
	q := r.session.Session.Query(
		"UPDATE spaces SET everyone_role_id = ? WHERE id = ?",
		roleID, spaceID,
	).WithContext(ctx)
	err := q.Exec()
	q.Release()
	return err
}

func (r *repo) UpdateSpaceIcon(ctx context.Context, spaceID int64, icon string, updatedAt time.Time) error {
	q := r.session.Session.Query(
		"UPDATE spaces SET icon = ?, updated_at = ? WHERE id = ?",
		icon, updatedAt, spaceID,
	).WithContext(ctx)
	err := q.Exec()
	q.Release()
	return err
}

func (r *repo) UpdateSpaceName(ctx context.Context, spaceID int64, name, nameAcronym string, updatedAt time.Time) error {
	q := r.session.Session.Query(
		"UPDATE spaces SET name = ?, name_acronym = ?, updated_at = ? WHERE id = ?",
		name, nameAcronym, updatedAt, spaceID,
	).WithContext(ctx)
	err := q.Exec()
	q.Release()
	return err
}

func (r *repo) GetMember(ctx context.Context, spaceID, userID int64) (*SpaceMember, error) {
	stmt, names := spaceMembersTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var m SpaceMember
	if err := q.Bind(spaceID, userID).GetRelease(&m); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *repo) SetMemberRoleIDs(ctx context.Context, spaceID, userID int64, roleIDs []int64) error {
	q := r.session.Session.Query(
		"UPDATE space_members SET role_ids = ? WHERE space_id = ? AND user_id = ?",
		roleIDs, spaceID, userID,
	).WithContext(ctx)
	err := q.Exec()
	q.Release()
	return err
}

func (r *repo) InsertSpaceRole(ctx context.Context, role *SpaceRole) error {
	stmt, names := spaceRolesTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(role).ExecRelease()
}

func (r *repo) GetSpaceRole(ctx context.Context, spaceID, roleID int64) (*SpaceRole, error) {
	stmt, names := spaceRolesTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var row SpaceRole
	if err := q.Bind(spaceID, roleID).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

func (r *repo) ListSpaceRoles(ctx context.Context, spaceID int64) ([]SpaceRole, error) {
	stmt, names := spaceRolesTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(spaceID).Iter()
	defer iter.Close()
	var out []SpaceRole
	var row SpaceRole
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) UpdateSpaceRole(ctx context.Context, role *SpaceRole) error {
	stmt, names := spaceRolesTable.Update(
		"name", "permissions", "position", "color", "hoist", "mentionable", "updated_at",
	)
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(role).ExecRelease()
}

func (r *repo) DeleteSpaceRole(ctx context.Context, spaceID, roleID int64) error {
	stmt, names := spaceRolesTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(spaceID, roleID).ExecRelease()
}

func (r *repo) UpsertRoomRoleOverride(ctx context.Context, o *SpaceRoomRoleOverride) error {
	stmt, names := spaceRoomRoleOverridesTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(o).ExecRelease()
}

func (r *repo) ListRoomRoleOverrides(ctx context.Context, spaceID, roomID int64) ([]SpaceRoomRoleOverride, error) {
	stmt, names := spaceRoomRoleOverridesTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(spaceID, roomID).Iter()
	defer iter.Close()
	var out []SpaceRoomRoleOverride
	var row SpaceRoomRoleOverride
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) DeleteRoomRoleOverride(ctx context.Context, spaceID, roomID, roleID int64) error {
	stmt, names := spaceRoomRoleOverridesTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(spaceID, roomID, roleID).ExecRelease()
}

func (r *repo) UpsertRoomUserOverride(ctx context.Context, o *SpaceRoomUserOverride) error {
	stmt, names := spaceRoomUserOverridesTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(o).ExecRelease()
}

func (r *repo) ListRoomUserOverrides(ctx context.Context, spaceID, roomID int64) ([]SpaceRoomUserOverride, error) {
	stmt, names := spaceRoomUserOverridesTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(spaceID, roomID).Iter()
	defer iter.Close()
	var out []SpaceRoomUserOverride
	var row SpaceRoomUserOverride
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) DeleteRoomUserOverride(ctx context.Context, spaceID, roomID, userID int64) error {
	stmt, names := spaceRoomUserOverridesTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(spaceID, roomID, userID).ExecRelease()
}
