package spaces

import (
	"context"
	"sort"
	"strings"
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
		"everyone_role_id", "widget_enabled", "widget_room_id", "widget_invite_code",
		"created_at", "updated_at",
	},
	PartKey: []string{"id"},
})

var spaceAuditLogTable = table.New(table.Metadata{
	Name:    "space_audit_log",
	Columns: []string{"space_id", "id", "action_type", "user_id", "target_id", "changes", "reason", "created_at"},
	PartKey: []string{"space_id"},
	SortKey: []string{"id"},
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

// Invites live in two tables kept in step: space_invite_by_code (PRIMARY KEY (code)) is
// the join lookup, space_invites ((space_id, code)) is the per-space listing for the
// settings UI. Both carry the same columns.
var spaceInviteByCodeTable = table.New(table.Metadata{
	Name:    "space_invite_by_code",
	Columns: []string{"code", "space_id", "inviter_id", "max_uses", "current_uses", "expires_at", "created_at"},
	PartKey: []string{"code"},
})

var spaceInvitesBySpaceTable = table.New(table.Metadata{
	Name:    "space_invites",
	Columns: []string{"space_id", "code", "inviter_id", "max_uses", "current_uses", "expires_at", "created_at"},
	PartKey: []string{"space_id"},
	SortKey: []string{"code"},
})

var spaceBansTable = table.New(table.Metadata{
	Name:    "space_bans",
	Columns: []string{"space_id", "user_id", "reason", "banned_by", "created_at"},
	PartKey: []string{"space_id"},
	SortKey: []string{"user_id"},
})

type Repository interface {
	Create(ctx context.Context, s *Space) error
	GetByID(ctx context.Context, id int64) (*Space, error)
	// GetByIDs fetches several spaces in one query; the result is in input order with
	// nil for ids that do not exist.
	GetByIDs(ctx context.Context, ids []int64) ([]*Space, error)

	// Custom emoji (space_emoji + the by-id lookup table, kept in step).
	ListEmojis(ctx context.Context, spaceID int64) ([]SpaceEmoji, error)
	GetEmoji(ctx context.Context, spaceID, emojiID int64) (*SpaceEmoji, error)
	GetEmojiByID(ctx context.Context, emojiID int64) (*SpaceEmojiRef, error)
	CreateEmoji(ctx context.Context, e *SpaceEmoji) error
	UpdateEmojiName(ctx context.Context, spaceID, emojiID int64, name string, now time.Time) error
	DeleteEmoji(ctx context.Context, spaceID, emojiID int64) error
	AddMember(ctx context.Context, spaceID, userID int64, roleIDs []int64) error
	ListSpacesByUser(ctx context.Context, userID int64) ([]SpaceMemberRow, error)
	AddSpaceToUserSet(ctx context.Context, userID int64, spaceID int64) error
	IsMember(ctx context.Context, spaceID, userID int64) (bool, error)
	ListMembers(ctx context.Context, spaceID int64) ([]SpaceMember, error)
	ListMembersPage(ctx context.Context, spaceID, after int64, limit int) ([]SpaceMember, error)
	CountMembers(ctx context.Context, spaceID int64) (int, error)
	CreateInvite(ctx context.Context, inv *SpaceInvite) error
	GetInviteByCode(ctx context.Context, code string) (*SpaceInvite, error)
	ListInvites(ctx context.Context, spaceID int64) ([]SpaceInvite, error)
	// SetInviteUses writes the use counter to both invite tables.
	SetInviteUses(ctx context.Context, spaceID int64, code string, uses int) error
	DeleteInvite(ctx context.Context, spaceID int64, code string) error

	// UpdateSpaceFields writes the given spaces columns (column name -> value; nil = null).
	// Column names come from code, never from a request.
	UpdateSpaceFields(ctx context.Context, spaceID int64, set map[string]interface{}) error

	InsertAuditEntry(ctx context.Context, e *SpaceAuditEntry) error
	// ListAuditEntries returns up to limit entries newer-first, older than beforeID when
	// beforeID > 0.
	ListAuditEntries(ctx context.Context, spaceID, beforeID int64, limit int) ([]SpaceAuditEntry, error)

	UpdateEveryoneRoleID(ctx context.Context, spaceID, roleID int64) error
	GetMember(ctx context.Context, spaceID, userID int64) (*SpaceMember, error)
	SetMemberRoleIDs(ctx context.Context, spaceID, userID int64, roleIDs []int64) error
	// RemoveMember deletes the space_members row and the matching spaces_by_user row (kick, ban, leave).
	RemoveMember(ctx context.Context, spaceID, userID int64) error

	CreateBan(ctx context.Context, ban *SpaceBan) error
	GetBan(ctx context.Context, spaceID, userID int64) (*SpaceBan, error)
	DeleteBan(ctx context.Context, spaceID, userID int64) error
	ListBans(ctx context.Context, spaceID int64) ([]SpaceBan, error)

	InsertSpaceRole(ctx context.Context, r *SpaceRole) error
	GetSpaceRole(ctx context.Context, spaceID, roleID int64) (*SpaceRole, error)
	ListSpaceRoles(ctx context.Context, spaceID int64) ([]SpaceRole, error)
	UpdateSpaceRole(ctx context.Context, r *SpaceRole) error
	// Data-migration support (see backfill.go). ForEachSpaceRole scans every role in
	// the keyspace; ClaimDataMigration records name and reports whether this call was
	// the one that claimed it.
	ForEachSpaceRole(ctx context.Context, fn func(spaceID, roleID int64, name string, permissions int64) error) error
	UpdateSpaceRolePermissions(ctx context.Context, spaceID, roleID int64, permissions int64, updatedAt time.Time) error
	ClaimDataMigration(ctx context.Context, name string) (bool, error)
	ReleaseDataMigration(ctx context.Context, name string) error
	DeleteSpaceRole(ctx context.Context, spaceID, roleID int64) error

	UpsertRoomRoleOverride(ctx context.Context, o *SpaceRoomRoleOverride) error
	ListRoomRoleOverrides(ctx context.Context, spaceID, roomID int64) ([]SpaceRoomRoleOverride, error)
	DeleteRoomRoleOverride(ctx context.Context, spaceID, roomID, roleID int64) error

	UpsertRoomUserOverride(ctx context.Context, o *SpaceRoomUserOverride) error
	ListRoomUserOverrides(ctx context.Context, spaceID, roomID int64) ([]SpaceRoomUserOverride, error)
	DeleteRoomUserOverride(ctx context.Context, spaceID, roomID, userID int64) error

	UpdateSpaceIcon(ctx context.Context, spaceID int64, icon string, updatedAt time.Time) error
	UpdateSpaceBanner(ctx context.Context, spaceID int64, banner string, updatedAt time.Time) error
	UpdateSpaceName(ctx context.Context, spaceID int64, name, nameAcronym string, updatedAt time.Time) error

	// DeleteSpace removes the space row and every table keyed only by space_id: roles,
	// bans, the per-space invite listing and the audit log. Rows that live in another
	// partition (each member's spaces_by_user, invites by code, emoji by id) and the
	// space's rooms belong to other repositories or other partitions, so the service
	// clears those first - see Service.DeleteSpace.
	DeleteSpace(ctx context.Context, spaceID int64) error
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

func (r *repo) GetByIDs(ctx context.Context, ids []int64) ([]*Space, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, sid := range ids {
		if _, ok := seen[sid]; !ok {
			seen[sid] = struct{}{}
			unique = append(unique, sid)
		}
	}
	stmt := "SELECT " + strings.Join(spacesTable.Metadata().Columns, ", ") + " FROM spaces WHERE id IN ?"
	q := r.session.Query(stmt, nil).WithContext(ctx).Bind(unique)
	defer q.Release()
	var rows []Space
	if err := q.Select(&rows); err != nil {
		return nil, err
	}
	byID := make(map[int64]*Space, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	out := make([]*Space, len(ids))
	for i, sid := range ids {
		out[i] = byID[sid]
	}
	return out, nil
}

func (r *repo) AddMember(ctx context.Context, spaceID, userID int64, roleIDs []int64) error {
	now := time.Now().UTC()
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := spaceMembersTable.Insert()
	b.Query(stmt, spaceID, userID, roleIDs, "", now)
	stmt, _ = spacesByUserTable.Insert()
	b.Query(stmt, userID, spaceID, now)
	return r.session.ExecuteBatch(b)
}

// RemoveMember deletes the member's space_members row and matching spaces_by_user row
// (used by kick, ban, and leave - all three are "this user is no longer a member"), and
// takes the space out of the users.spaces set that AddSpaceToUserSet maintains.
func (r *repo) RemoveMember(ctx context.Context, spaceID, userID int64) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := spaceMembersTable.Delete()
	b.Query(stmt, spaceID, userID)
	stmt, _ = spacesByUserTable.Delete()
	b.Query(stmt, userID, spaceID)
	b.Query("UPDATE users SET spaces = spaces - ? WHERE id = ?", []int64{spaceID}, userID)
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

// ListMembersPage returns up to limit members whose user id is above after, in user id
// order - the clustering order, so a page is one range read.
func (r *repo) ListMembersPage(ctx context.Context, spaceID, after int64, limit int) ([]SpaceMember, error) {
	const stmt = "SELECT space_id, user_id, role_ids, nickname, joined_at FROM space_members WHERE space_id = ? AND user_id > ? LIMIT ?"
	q := r.session.Query(stmt, nil).WithContext(ctx).Bind(spaceID, after, limit)
	var out []SpaceMember
	if err := q.SelectRelease(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// CountMembers counts a space's members without loading them.
func (r *repo) CountMembers(ctx context.Context, spaceID int64) (int, error) {
	q := r.session.Query("SELECT COUNT(*) FROM space_members WHERE space_id = ?", nil).WithContext(ctx).Bind(spaceID)
	defer q.Release()
	iter := q.Iter()
	var n int64
	iter.Scan(&n)
	if err := iter.Close(); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (r *repo) CreateInvite(ctx context.Context, inv *SpaceInvite) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := spaceInviteByCodeTable.Insert()
	b.Query(stmt, inv.Code, inv.SpaceID, inv.InviterID, inv.MaxUses, inv.CurrentUses, inv.ExpiresAt, inv.CreatedAt)
	stmt, _ = spaceInvitesBySpaceTable.Insert()
	b.Query(stmt, inv.SpaceID, inv.Code, inv.InviterID, inv.MaxUses, inv.CurrentUses, inv.ExpiresAt, inv.CreatedAt)
	return r.session.ExecuteBatch(b)
}

func (r *repo) GetInviteByCode(ctx context.Context, code string) (*SpaceInvite, error) {
	stmt, names := spaceInviteByCodeTable.Get()
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

func (r *repo) ListInvites(ctx context.Context, spaceID int64) ([]SpaceInvite, error) {
	stmt, names := spaceInvitesBySpaceTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(spaceID).Iter()
	defer iter.Close()
	var out []SpaceInvite
	var row SpaceInvite
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) SetInviteUses(ctx context.Context, spaceID int64, code string, uses int) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	b.Query("UPDATE space_invite_by_code SET current_uses = ? WHERE code = ?", uses, code)
	b.Query("UPDATE space_invites SET current_uses = ? WHERE space_id = ? AND code = ?", uses, spaceID, code)
	return r.session.ExecuteBatch(b)
}

func (r *repo) DeleteInvite(ctx context.Context, spaceID int64, code string) error {
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	stmt, _ := spaceInviteByCodeTable.Delete()
	b.Query(stmt, code)
	stmt, _ = spaceInvitesBySpaceTable.Delete()
	b.Query(stmt, spaceID, code)
	return r.session.ExecuteBatch(b)
}

func (r *repo) UpdateSpaceFields(ctx context.Context, spaceID int64, set map[string]interface{}) error {
	if len(set) == 0 {
		return nil
	}
	cols := make([]string, 0, len(set))
	for c := range set {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	args := make([]interface{}, 0, len(cols)+1)
	var b strings.Builder
	b.WriteString("UPDATE spaces SET ")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c)
		b.WriteString(" = ?")
		args = append(args, set[c])
	}
	b.WriteString(" WHERE id = ?")
	args = append(args, spaceID)
	q := r.session.Session.Query(b.String(), args...).WithContext(ctx)
	err := q.Exec()
	q.Release()
	return err
}

func (r *repo) InsertAuditEntry(ctx context.Context, e *SpaceAuditEntry) error {
	stmt, names := spaceAuditLogTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(e).ExecRelease()
}

func (r *repo) ListAuditEntries(ctx context.Context, spaceID, beforeID int64, limit int) ([]SpaceAuditEntry, error) {
	cols := strings.Join(spaceAuditLogTable.Metadata().Columns, ", ")
	var q *gocqlx.Queryx
	if beforeID > 0 {
		q = r.session.Query("SELECT "+cols+" FROM space_audit_log WHERE space_id = ? AND id < ? LIMIT ?", nil).
			WithContext(ctx).Bind(spaceID, beforeID, limit)
	} else {
		q = r.session.Query("SELECT "+cols+" FROM space_audit_log WHERE space_id = ? LIMIT ?", nil).
			WithContext(ctx).Bind(spaceID, limit)
	}
	defer q.Release()
	var out []SpaceAuditEntry
	if err := q.Select(&out); err != nil {
		return nil, err
	}
	return out, nil
}

var spaceRolesTable = table.New(table.Metadata{
	Name: "space_roles",
	Columns: []string{
		"space_id", "id", "name", "permissions", "position", "color", "hoist", "mentionable", "bot_id", "created_at", "updated_at",
	},
	PartKey: []string{"space_id"},
	SortKey: []string{"id"},
})

var spaceRoomRoleOverridesTable = table.New(table.Metadata{
	Name:    "space_room_role_overrides",
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

func (r *repo) UpdateSpaceBanner(ctx context.Context, spaceID int64, banner string, updatedAt time.Time) error {
	q := r.session.Session.Query(
		"UPDATE spaces SET banner = ?, updated_at = ? WHERE id = ?",
		banner, updatedAt, spaceID,
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

func (r *repo) ForEachSpaceRole(ctx context.Context, fn func(spaceID, roleID int64, name string, permissions int64) error) error {
	q := r.session.Session.Query("SELECT space_id, id, name, permissions FROM space_roles").WithContext(ctx)
	defer q.Release()
	iter := q.Iter()
	var spaceID, roleID, perms int64
	var name string
	for iter.Scan(&spaceID, &roleID, &name, &perms) {
		if err := fn(spaceID, roleID, name, perms); err != nil {
			_ = iter.Close()
			return err
		}
	}
	return iter.Close()
}

func (r *repo) UpdateSpaceRolePermissions(ctx context.Context, spaceID, roleID int64, permissions int64, updatedAt time.Time) error {
	q := r.session.Session.Query(
		"UPDATE space_roles SET permissions = ?, updated_at = ? WHERE space_id = ? AND id = ?",
		permissions, updatedAt, spaceID, roleID,
	).WithContext(ctx)
	defer q.Release()
	return q.Exec()
}

func (r *repo) ClaimDataMigration(ctx context.Context, name string) (bool, error) {
	q := r.session.Session.Query(
		"INSERT INTO data_migrations (name, applied_at) VALUES (?, ?) IF NOT EXISTS",
		name, time.Now().UTC(),
	).WithContext(ctx)
	defer q.Release()
	// MapScanCAS, not ScanCAS: on a losing race the existing row comes back and has to
	// be scanned somewhere; with no destinations ScanCAS fails *after* a winning insert,
	// which reported the claim as an error while leaving it in place.
	applied, err := q.MapScanCAS(map[string]interface{}{})
	if err != nil {
		return false, err
	}
	return applied, nil
}

func (r *repo) ReleaseDataMigration(ctx context.Context, name string) error {
	q := r.session.Session.Query("DELETE FROM data_migrations WHERE name = ?", name).WithContext(ctx)
	defer q.Release()
	return q.Exec()
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

func (r *repo) CreateBan(ctx context.Context, ban *SpaceBan) error {
	stmt, names := spaceBansTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(ban).ExecRelease()
}

func (r *repo) GetBan(ctx context.Context, spaceID, userID int64) (*SpaceBan, error) {
	stmt, names := spaceBansTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var ban SpaceBan
	if err := q.Bind(spaceID, userID).GetRelease(&ban); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &ban, nil
}

func (r *repo) DeleteBan(ctx context.Context, spaceID, userID int64) error {
	stmt, names := spaceBansTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	return q.Bind(spaceID, userID).ExecRelease()
}

func (r *repo) ListBans(ctx context.Context, spaceID int64) ([]SpaceBan, error) {
	stmt, names := spaceBansTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(spaceID).Iter()
	defer iter.Close()
	var out []SpaceBan
	var row SpaceBan
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

var spaceEmojiTable = table.New(table.Metadata{
	Name:    "space_emoji",
	Columns: []string{"space_id", "id", "name", "image_hash", "url", "animated", "creator_id", "created_at", "updated_at"},
	PartKey: []string{"space_id"},
	SortKey: []string{"id"},
})

var spaceEmojiByIDTable = table.New(table.Metadata{
	Name:    "space_emoji_by_id",
	Columns: []string{"id", "space_id", "name", "url", "animated"},
	PartKey: []string{"id"},
})

func (r *repo) ListEmojis(ctx context.Context, spaceID int64) ([]SpaceEmoji, error) {
	stmt, names := spaceEmojiTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(spaceID).Iter()
	defer iter.Close()
	var out []SpaceEmoji
	var row SpaceEmoji
	for iter.StructScan(&row) {
		out = append(out, row)
	}
	return out, iter.Close()
}

func (r *repo) GetEmoji(ctx context.Context, spaceID, emojiID int64) (*SpaceEmoji, error) {
	var e SpaceEmoji
	stmt, names := spaceEmojiTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(spaceID, emojiID).GetRelease(&e); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &e, nil
}

func (r *repo) GetEmojiByID(ctx context.Context, emojiID int64) (*SpaceEmojiRef, error) {
	var e SpaceEmojiRef
	stmt, names := spaceEmojiByIDTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(emojiID).GetRelease(&e); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &e, nil
}

func (r *repo) CreateEmoji(ctx context.Context, e *SpaceEmoji) error {
	stmt, names := spaceEmojiTable.Insert()
	if err := r.session.Query(stmt, names).WithContext(ctx).BindStruct(e).ExecRelease(); err != nil {
		return err
	}
	ref := &SpaceEmojiRef{ID: e.ID, SpaceID: e.SpaceID, Name: e.Name, URL: e.URL, Animated: e.Animated}
	stmt, names = spaceEmojiByIDTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(ref).ExecRelease()
}

func (r *repo) UpdateEmojiName(ctx context.Context, spaceID, emojiID int64, name string, now time.Time) error {
	stmt, names := spaceEmojiTable.Update("name", "updated_at")
	if err := r.session.Query(stmt, names).WithContext(ctx).Bind(name, now, spaceID, emojiID).ExecRelease(); err != nil {
		return err
	}
	stmt, names = spaceEmojiByIDTable.Update("name")
	return r.session.Query(stmt, names).WithContext(ctx).Bind(name, emojiID).ExecRelease()
}

func (r *repo) DeleteEmoji(ctx context.Context, spaceID, emojiID int64) error {
	stmt, names := spaceEmojiTable.Delete()
	if err := r.session.Query(stmt, names).WithContext(ctx).Bind(spaceID, emojiID).ExecRelease(); err != nil {
		return err
	}
	stmt, names = spaceEmojiByIDTable.Delete()
	return r.session.Query(stmt, names).WithContext(ctx).Bind(emojiID).ExecRelease()
}

// DeleteSpace wipes the tables partitioned by space_id alone and then the space row
// itself. Partition-wide deletes, so the number of roles/bans/invites/audit entries does
// not matter; what lives in other partitions is cleared by the caller beforehand.
func (r *repo) DeleteSpace(ctx context.Context, spaceID int64) error {
	for _, cql := range []string{
		"DELETE FROM space_members WHERE space_id = ?",
		"DELETE FROM space_roles WHERE space_id = ?",
		"DELETE FROM space_bans WHERE space_id = ?",
		"DELETE FROM space_invites WHERE space_id = ?",
		"DELETE FROM space_emoji WHERE space_id = ?",
		"DELETE FROM space_audit_log WHERE space_id = ?",
	} {
		if err := r.session.Query(cql, nil).WithContext(ctx).Bind(spaceID).ExecRelease(); err != nil {
			return err
		}
	}
	stmt, names := spacesTable.Delete()
	return r.session.Query(stmt, names).WithContext(ctx).Bind(spaceID).ExecRelease()
}
