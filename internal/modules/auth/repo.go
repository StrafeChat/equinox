package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

// ProfileUpdate holds optional fields for PATCH /users/@me.
type ProfileUpdate struct {
	DisplayName *string         `json:"display_name,omitempty"`
	Bio         *string         `json:"bio,omitempty"`
	AboutMe     *string         `json:"about_me,omitempty"`
	Avatar      *string         `json:"avatar,omitempty"`
	Banner      *string         `json:"banner,omitempty"`
	AccentColor *string         `json:"accent_color,omitempty"`
	Presence    *PresenceUpdate `json:"presence,omitempty"`
	// Flags is the profile-badge bitfield (see badges.go). Set only by the instance
	// module's admin badge assignment; a nil pointer leaves it unchanged.
	Flags *int `json:"flags,omitempty"`
}

// PresenceUpdate holds optional presence fields (status: online, offline, dnd, idle).
type PresenceUpdate struct {
	Online       *bool   `json:"online,omitempty"`
	Status       *string `json:"status,omitempty"`
	CustomStatus *string `json:"custom_status,omitempty"`
}

type UserRepository interface {
	Create(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id int64) (*User, error)
	GetByIDs(ctx context.Context, ids []int64) ([]*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByUsernameDiscriminator(ctx context.Context, username string, discriminator int) (*User, error)
	EmailExists(ctx context.Context, email string) (bool, error)
	DiscriminatorsForUsername(ctx context.Context, username string) ([]int, error)
	UpdateRelationships(ctx context.Context, userID int64, add, remove []int64) error
	UpdateBlocks(ctx context.Context, userID int64, add, remove []int64) error
	UpdateProfile(ctx context.Context, userID int64, upd *ProfileUpdate) (*User, error)
	// SetTOTP writes the encrypted TOTP secret and enabled flag together, since they only
	// ever change as a pair (setup/enable writes both, disable clears both).
	SetTOTP(ctx context.Context, userID int64, secret string, enabled bool) error
	// SetEmailVerified records that the account's address was confirmed through a link
	// sent to it (or disproved - a future address change clears it).
	SetEmailVerified(ctx context.Context, userID int64, verified bool) error
	// SetPassword replaces the bcrypt hash. Sessions are the caller's business: a reset
	// revokes them, a routine change may not.
	SetPassword(ctx context.Context, userID int64, hash string) error

	// Federation shadows (see User.HomeDomain). UpsertShadow writes the users row and the
	// users_by_remote lookup; it never touches the email/username lookup tables.
	UpsertShadow(ctx context.Context, u *User) error
	GetByRemote(ctx context.Context, homeDomain string, remoteID int64) (*User, error)
}

var userTable = table.New(table.Metadata{
	Name: "users",
	Columns: []string{
		"id",
		"email",
		"password",
		"username",
		"discriminator",
		"display_name",
		"avatar",
		"banner",
		"bot",
		"bots",
		"system",
		"bio",
		"flags",
		"relationships",
		"spaces",
		"blocks",
		"date_of_birth",
		"verified_email",
		"about_me",
		"accent_color",
		"locale",
		"presence",
		"created_at",
		"updated_at",
		"home_domain",
		"remote_id",
		"totp_enabled",
		"totp_secret",
	},
	PartKey: []string{"id"},
})

var usersByRemoteTable = table.New(table.Metadata{
	Name:    "users_by_remote",
	Columns: []string{"home_domain", "remote_id", "user_id"},
	PartKey: []string{"home_domain"},
	SortKey: []string{"remote_id"},
})

var usersByEmailTable = table.New(table.Metadata{
	Name:    "users_by_email",
	Columns: []string{"email", "user_id"},
	PartKey: []string{"email"},
})

var usersByUsernameDiscriminatorTable = table.New(table.Metadata{
	Name:    "users_by_username_discriminator",
	Columns: []string{"username", "discriminator", "user_id"},
	PartKey: []string{"username"},
	SortKey: []string{"discriminator"},
})

type scyllaUserRepo struct {
	session gocqlx.Session
}

func NewUserRepository(session gocqlx.Session) UserRepository {
	return &scyllaUserRepo{session: session}
}

// Create registers a user. The email and username#discriminator lookup rows are claimed
// first with lightweight transactions (IF NOT EXISTS), so two registrations racing on the
// same address or tag cannot both succeed - the service's EmailExists check is only a
// fast path. Claims are released again if a later step fails.
func (r *scyllaUserRepo) Create(ctx context.Context, u *User) error {
	u.CreatedAt = time.Now().UTC()
	u.UpdatedAt = u.CreatedAt

	applied, err := r.claim(ctx,
		"INSERT INTO users_by_email (email, user_id) VALUES (?, ?) IF NOT EXISTS",
		u.Email, u.ID)
	if err != nil {
		return err
	}
	if !applied {
		return ErrEmailInUse
	}

	applied, err = r.claim(ctx,
		"INSERT INTO users_by_username_discriminator (username, discriminator, user_id) VALUES (?, ?, ?) IF NOT EXISTS",
		u.Username, u.Discriminator, u.ID)
	if err != nil {
		r.release(ctx, "DELETE FROM users_by_email WHERE email = ?", u.Email)
		return err
	}
	if !applied {
		r.release(ctx, "DELETE FROM users_by_email WHERE email = ?", u.Email)
		return ErrDiscriminatorInUse
	}

	stmt, names := userTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	if err := q.BindStruct(u).ExecRelease(); err != nil {
		r.release(ctx, "DELETE FROM users_by_email WHERE email = ?", u.Email)
		r.release(ctx, "DELETE FROM users_by_username_discriminator WHERE username = ? AND discriminator = ?", u.Username, u.Discriminator)
		return err
	}
	return nil
}

// claim runs an INSERT ... IF NOT EXISTS and reports whether it was applied.
func (r *scyllaUserRepo) claim(ctx context.Context, stmt string, args ...interface{}) (bool, error) {
	q := r.session.Session.Query(stmt, args...).WithContext(ctx)
	defer q.Release()
	return q.MapScanCAS(map[string]interface{}{})
}

// release best-effort deletes a lookup row claimed by a registration that then failed.
func (r *scyllaUserRepo) release(ctx context.Context, stmt string, args ...interface{}) {
	q := r.session.Session.Query(stmt, args...).WithContext(ctx)
	_ = q.Exec()
	q.Release()
}

func (r *scyllaUserRepo) UpsertShadow(ctx context.Context, u *User) error {
	if u == nil || u.HomeDomain == "" || u.RemoteID == nil {
		return errors.New("shadow user needs home_domain and remote_id")
	}
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	stmt, names := userTable.Insert()
	if err := r.session.Query(stmt, names).WithContext(ctx).BindStruct(u).ExecRelease(); err != nil {
		return err
	}
	ref := UserByRemote{HomeDomain: u.HomeDomain, RemoteID: *u.RemoteID, UserID: u.ID}
	stmt, names = usersByRemoteTable.Insert()
	return r.session.Query(stmt, names).WithContext(ctx).BindStruct(&ref).ExecRelease()
}

func (r *scyllaUserRepo) GetByRemote(ctx context.Context, homeDomain string, remoteID int64) (*User, error) {
	var row UserByRemote
	stmt, names := usersByRemoteTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(homeDomain, remoteID).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return r.GetByID(ctx, row.UserID)
}

func (r *scyllaUserRepo) GetByID(ctx context.Context, id int64) (*User, error) {
	var u User
	stmt, names := userTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(id).GetRelease(&u); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &u, nil
}

// GetByIDs fetches multiple users by ID in a single query. Returns a slice ordered by input; nil for missing users.
func (r *scyllaUserRepo) GetByIDs(ctx context.Context, ids []int64) ([]*User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]bool, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	stmt := "SELECT " + strings.Join(userTable.Metadata().Columns, ", ") + " FROM users WHERE id IN ?"
	q := r.session.Query(stmt, nil).WithContext(ctx).Bind(unique)
	defer q.Release()
	var rows []User
	if err := q.Select(&rows); err != nil {
		return nil, err
	}
	// Build id->user map and reorder to match input (nil for missing users)
	byID := make(map[int64]*User, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	result := make([]*User, len(ids))
	for i, id := range ids {
		result[i] = byID[id]
	}
	return result, nil
}

func (r *scyllaUserRepo) GetByEmail(ctx context.Context, email string) (*User, error) {
	var row UserByEmail
	stmt, names := usersByEmailTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(email).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return r.GetByID(ctx, row.UserID)
}

func (r *scyllaUserRepo) GetByUsernameDiscriminator(ctx context.Context, username string, discriminator int) (*User, error) {
	var row UserByUsernameDiscriminator
	stmt, names := usersByUsernameDiscriminatorTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(username, discriminator).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	return r.GetByID(ctx, row.UserID)
}

func (r *scyllaUserRepo) EmailExists(ctx context.Context, email string) (bool, error) {
	var row UserByEmail
	stmt, names := usersByEmailTable.Get()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	if err := q.Bind(email).GetRelease(&row); err != nil {
		if err == gocql.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *scyllaUserRepo) DiscriminatorsForUsername(ctx context.Context, username string) ([]int, error) {
	stmt, names := usersByUsernameDiscriminatorTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	iter := q.Bind(username).Iter()
	defer iter.Close()

	var out []int
	var row UserByUsernameDiscriminator
	for iter.StructScan(&row) {
		out = append(out, row.Discriminator)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *scyllaUserRepo) UpdateBlocks(ctx context.Context, userID int64, add, remove []int64) error {
	for _, blockedID := range add {
		q := r.session.Session.Query(
			"UPDATE users SET blocks = blocks + ? WHERE id = ?",
			[]int64{blockedID}, userID,
		).WithContext(ctx)
		err := q.Exec()
		q.Release()
		if err != nil {
			return err
		}
	}
	for _, blockedID := range remove {
		q := r.session.Session.Query(
			"UPDATE users SET blocks = blocks - ? WHERE id = ?",
			[]int64{blockedID}, userID,
		).WithContext(ctx)
		err := q.Exec()
		q.Release()
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *scyllaUserRepo) UpdateRelationships(ctx context.Context, userID int64, add, remove []int64) error {
	for _, friendID := range add {
		q := r.session.Session.Query(
			"UPDATE users SET relationships = relationships + ? WHERE id = ?",
			[]int64{friendID}, userID,
		).WithContext(ctx)
		err := q.Exec()
		q.Release()
		if err != nil {
			return err
		}
	}
	for _, friendID := range remove {
		q := r.session.Session.Query(
			"UPDATE users SET relationships = relationships - ? WHERE id = ?",
			[]int64{friendID}, userID,
		).WithContext(ctx)
		err := q.Exec()
		q.Release()
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *scyllaUserRepo) UpdateProfile(ctx context.Context, userID int64, upd *ProfileUpdate) (*User, error) {
	if upd == nil {
		return r.GetByID(ctx, userID)
	}
	u, err := r.GetByID(ctx, userID)
	if err != nil || u == nil {
		return u, err
	}
	u.UpdatedAt = time.Now().UTC()
	if upd.DisplayName != nil {
		u.DisplayName = *upd.DisplayName
	}
	if upd.Bio != nil {
		u.Bio = *upd.Bio
	}
	if upd.AboutMe != nil {
		u.AboutMe = *upd.AboutMe
	}
	if upd.Avatar != nil {
		u.Avatar = *upd.Avatar
	}
	if upd.Banner != nil {
		u.Banner = *upd.Banner
	}
	if upd.AccentColor != nil {
		u.AccentColor = *upd.AccentColor
	}
	if upd.Flags != nil {
		u.Flags = *upd.Flags
	}
	if upd.Presence != nil {
		if upd.Presence.Online != nil {
			u.Presence.Online = *upd.Presence.Online
		}
		if upd.Presence.Status != nil {
			u.Presence.Status = *upd.Presence.Status
		}
		if upd.Presence.CustomStatus != nil {
			u.Presence.CustomStatus = *upd.Presence.CustomStatus
		}
	}
	stmt, names := userTable.Update("display_name", "bio", "about_me", "avatar", "banner", "accent_color", "flags", "presence", "updated_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	return u, q.Bind(u.DisplayName, u.Bio, u.AboutMe, u.Avatar, u.Banner, u.AccentColor, u.Flags, u.Presence, u.UpdatedAt, u.ID).ExecRelease()
}

func (r *scyllaUserRepo) SetTOTP(ctx context.Context, userID int64, secret string, enabled bool) error {
	stmt, names := userTable.Update("totp_secret", "totp_enabled")
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(secret, enabled, userID).ExecRelease()
}

func (r *scyllaUserRepo) SetEmailVerified(ctx context.Context, userID int64, verified bool) error {
	stmt, names := userTable.Update("verified_email", "updated_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(verified, time.Now().UTC(), userID).ExecRelease()
}

func (r *scyllaUserRepo) SetPassword(ctx context.Context, userID int64, hash string) error {
	stmt, names := userTable.Update("password", "updated_at")
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(hash, time.Now().UTC(), userID).ExecRelease()
}
