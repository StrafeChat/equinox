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
		"verification_level", "default_message_notifications", "explicit_content_filter", "official",
		"features", "afk_room_id", "afk_timeout", "system_room_id", "system_room_flags",
		"rules_room_id", "max_presences", "max_members", "vanity_url_code",
		"birthday_channel_id", "birthday_message",
		"preferred_locale", "public_updates_room_id", "max_video_room_users",
		"everyone_role_id", "widget_enabled", "widget_room_id", "widget_invite_code",
		"automod_flags", "automod_mention_limit",
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

// space_members carries the membership (roles, joined_at) and the member's space profile
// (nickname through pronouns; see member_profile.go). Two statements below spell the columns
// out instead of going through this metadata - the INSERT and ListMembersPage's SELECT - and
// repo_columns_test.go keeps all three in step, since a mismatch only fails at runtime.
var spaceMembersTable = table.New(table.Metadata{
	Name:    "space_members",
	Columns: []string{"space_id", "user_id", "role_ids", "nickname", "avatar", "banner", "about_me", "bio", "pronouns", "joined_at"},
	PartKey: []string{"space_id"},
	SortKey: []string{"user_id"},
})

// insertMemberStmt writes every column of a membership, so a join (or a mirror's copy of one)
// always starts from exactly what it binds - nil profile columns become NULL and clear
// anything a removed membership left behind. The values come from memberRowValues.
const insertMemberStmt = "INSERT INTO space_members (space_id, user_id, role_ids, nickname, avatar, banner, about_me, bio, pronouns, joined_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

// listMembersPageStmt is ListMembersPage's range read; same columns as spaceMembersTable.
const listMembersPageStmt = "SELECT space_id, user_id, role_ids, nickname, avatar, banner, about_me, bio, pronouns, joined_at FROM space_members WHERE space_id = ? AND user_id > ? LIMIT ?"

// memberRowValues is insertMemberStmt's bind list, in its column order.
func memberRowValues(spaceID, userID int64, roleIDs []int64, p MemberProfile, joinedAt time.Time) []interface{} {
	return []interface{}{
		spaceID, userID, roleIDs,
		nilIfEmpty(p.Nick), nilIfEmpty(p.Avatar), nilIfEmpty(p.Banner),
		nilIfEmpty(p.AboutMe), nilIfEmpty(p.Bio), nilIfEmpty(p.Pronouns),
		joinedAt,
	}
}

// liveMember is the test every member read applies: a row without joined_at is not a
// member. Such a "ghost" row can appear when a profile or role write races the member's
// removal - Scylla does not order a lightweight transaction against a plain delete, so the
// written cells can outlive the row's tombstone, leaving a row with no joined_at and no
// roles. Read as a member, it would hand a kicked or banned user @everyone's access again
// (every permission check starts from @everyone). This filter, not the conditional writes,
// is what keeps a removed member removed; AddMember overwrites such a row cleanly on a rejoin.
func liveMember(m *SpaceMember) bool {
	return m != nil && !m.JoinedAt.IsZero()
}

// liveMembers drops ghost rows (see liveMember), reusing rows' backing array.
func liveMembers(rows []SpaceMember) []SpaceMember {
	out := rows[:0]
	for i := range rows {
		if liveMember(&rows[i]) {
			out = append(out, rows[i])
		}
	}
	return out
}

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
	// InsertEmojiRefIfAbsent writes a by-id ref only when the id is unknown (LWT).
	InsertEmojiRefIfAbsent(ctx context.Context, ref *SpaceEmojiRef) error
	CreateEmoji(ctx context.Context, e *SpaceEmoji) error
	UpdateEmojiName(ctx context.Context, spaceID, emojiID int64, name string, now time.Time) error
	DeleteEmoji(ctx context.Context, spaceID, emojiID int64) error
	// AddMember writes the whole member row (every column, so nothing from an earlier
	// membership survives) and the spaces_by_user row. A local join passes MemberProfile{};
	// a mirror copying the origin's member passes their space profile.
	AddMember(ctx context.Context, spaceID, userID int64, roleIDs []int64, p MemberProfile) error
	ListSpacesByUser(ctx context.Context, userID int64) ([]SpaceMemberRow, error)
	AddSpaceToUserSet(ctx context.Context, userID int64, spaceID int64) error
	// IsMember, MemberRoleIDs, GetMember, ListMembers and ListMembersPage all treat a row
	// without joined_at as absent (see liveMember).
	IsMember(ctx context.Context, spaceID, userID int64) (bool, error)
	// MemberRoleIDs is the narrow read permission checks use: only role_ids and joined_at,
	// not the profile text. ok is false when userID is not a member.
	MemberRoleIDs(ctx context.Context, spaceID, userID int64) (roleIDs []int64, ok bool, err error)
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
	// UpdateMemberProfile sets the profile columns named in p on the member row prev (as
	// read just before; nil field = leave alone, "" = clear to NULL). It is a lightweight
	// transaction tied to that membership - IF joined_at = prev.JoinedAt - so it can never
	// land on a row the member has since left (or left and rejoined); and when it sets the
	// avatar or banner it also compares that column with prev's value, so two concurrent
	// uploads cannot both believe they replaced the same image. false = nothing written:
	// re-read and decide (the member is gone, or the image changed underneath).
	UpdateMemberProfile(ctx context.Context, spaceID, userID int64, prev *SpaceMember, p MemberProfilePatch) (bool, error)
	// RemoveMember deletes the space_members row and the matching spaces_by_user row (kick, ban, leave).
	// Call it through Service.removeMemberRow, which also releases the member's space images.
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
	// the keyspace (botID is non-zero for a bot's managed role); ClaimDataMigration records
	// name and reports whether this call was the one that claimed it.
	ForEachSpaceRole(ctx context.Context, fn func(spaceID, roleID int64, name string, permissions, botID int64) error) error
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

func (r *repo) AddMember(ctx context.Context, spaceID, userID int64, roleIDs []int64, p MemberProfile) error {
	now := time.Now().UTC()
	b := r.session.Batch(gocql.LoggedBatch).WithContext(ctx)
	b.Query(insertMemberStmt, memberRowValues(spaceID, userID, roleIDs, p, now)...)
	stmt, _ := spacesByUserTable.Insert()
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

// IsMember reads only joined_at: it runs on every gateway subscribe, message send and thread
// check, none of which need the rest of the row.
func (r *repo) IsMember(ctx context.Context, spaceID, userID int64) (bool, error) {
	q := r.session.Session.Query(
		"SELECT joined_at FROM space_members WHERE space_id = ? AND user_id = ?",
		spaceID, userID,
	).WithContext(ctx)
	defer q.Release()
	var joinedAt time.Time
	if err := q.Scan(&joinedAt); err != nil {
		if err == gocql.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return !joinedAt.IsZero(), nil
}

func (r *repo) MemberRoleIDs(ctx context.Context, spaceID, userID int64) ([]int64, bool, error) {
	q := r.session.Session.Query(
		"SELECT role_ids, joined_at FROM space_members WHERE space_id = ? AND user_id = ?",
		spaceID, userID,
	).WithContext(ctx)
	defer q.Release()
	var roleIDs []int64
	var joinedAt time.Time
	if err := q.Scan(&roleIDs, &joinedAt); err != nil {
		if err == gocql.ErrNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	if joinedAt.IsZero() {
		return nil, false, nil
	}
	return roleIDs, true, nil
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
		if liveMember(&row) {
			out = append(out, row)
		}
		// A fresh struct per row, so a ghost row can never pass the filter on the previous
		// row's joined_at.
		row = SpaceMember{}
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListMembersPage returns up to limit members whose user id is above after, in user id
// order - the clustering order, so a page is one range read. Ghost rows are skipped without
// shortening the page (a short page reads as the last one to the caller): when some were
// dropped, the read continues past them.
func (r *repo) ListMembersPage(ctx context.Context, spaceID, after int64, limit int) ([]SpaceMember, error) {
	var out []SpaceMember
	for len(out) < limit {
		want := limit - len(out)
		var rows []SpaceMember
		q := r.session.Query(listMembersPageStmt, nil).WithContext(ctx).Bind(spaceID, after, want)
		if err := q.SelectRelease(&rows); err != nil {
			return nil, err
		}
		n := len(rows)
		if n > 0 {
			after = rows[n-1].UserID
		}
		out = append(out, liveMembers(rows)...)
		if n < want {
			break
		}
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
	if !liveMember(&m) {
		return nil, nil
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

func (r *repo) UpdateMemberProfile(ctx context.Context, spaceID, userID int64, prev *SpaceMember, p MemberProfilePatch) (bool, error) {
	if !liveMember(prev) {
		return false, nil
	}
	stmt, args := memberProfileUpdate(spaceID, userID, prev, p)
	if stmt == "" {
		return true, nil
	}
	q := r.session.Session.Query(stmt, args...).WithContext(ctx)
	defer q.Release()
	// MapScanCAS for the same reason as ClaimDataMigration: a CAS that loses hands back the
	// current values of the condition columns, which have to be scanned somewhere.
	return q.MapScanCAS(map[string]interface{}{})
}

// memberProfileUpdate builds UpdateMemberProfile's statement: only the columns p sets, in
// column order, conditioned on prev's membership and, for an image it replaces, on prev's
// image. "" for no column set.
func memberProfileUpdate(spaceID, userID int64, prev *SpaceMember, p MemberProfilePatch) (string, []interface{}) {
	fields := []struct {
		col string
		val *string
	}{
		{"nickname", p.Nick}, {"avatar", p.Avatar}, {"banner", p.Banner},
		{"about_me", p.AboutMe}, {"bio", p.Bio}, {"pronouns", p.Pronouns},
	}
	var b strings.Builder
	args := make([]interface{}, 0, len(fields)+5)
	for _, f := range fields {
		if f.val == nil {
			continue
		}
		if len(args) == 0 {
			b.WriteString("UPDATE space_members SET ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(f.col)
		b.WriteString(" = ?")
		args = append(args, nilIfEmpty(*f.val))
	}
	if len(args) == 0 {
		return "", nil
	}
	b.WriteString(" WHERE space_id = ? AND user_id = ? IF joined_at = ?")
	args = append(args, spaceID, userID, prev.JoinedAt)
	// prev's "" is a NULL column (empty values are always written as NULL), and "= null" is
	// how a condition asks for that.
	if p.Avatar != nil {
		b.WriteString(" AND avatar = ?")
		args = append(args, nilIfEmpty(prev.Avatar))
	}
	if p.Banner != nil {
		b.WriteString(" AND banner = ?")
		args = append(args, nilIfEmpty(prev.Banner))
	}
	return b.String(), args
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

func (r *repo) ForEachSpaceRole(ctx context.Context, fn func(spaceID, roleID int64, name string, permissions, botID int64) error) error {
	q := r.session.Session.Query("SELECT space_id, id, name, permissions, bot_id FROM space_roles").WithContext(ctx)
	defer q.Release()
	iter := q.Iter()
	var spaceID, roleID, perms, botID int64
	var name string
	for iter.Scan(&spaceID, &roleID, &name, &perms, &botID) {
		if err := fn(spaceID, roleID, name, perms, botID); err != nil {
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

func (r *repo) InsertEmojiRefIfAbsent(ctx context.Context, ref *SpaceEmojiRef) error {
	q := r.session.Session.Query(
		"INSERT INTO space_emoji_by_id (id, space_id, name, url, animated) VALUES (?, ?, ?, ?, ?) IF NOT EXISTS",
		ref.ID, ref.SpaceID, ref.Name, ref.URL, ref.Animated,
	).WithContext(ctx)
	defer q.Release()
	// The applied flag is not an error either way: a row that exists is the point.
	return q.Exec()
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
