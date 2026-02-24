package auth

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"github.com/scylladb/gocqlx/v3/table"
)

type UserRepository interface {
	Create(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id int64) (*User, error)
	GetByIDs(ctx context.Context, ids []int64) ([]*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByUsernameDiscriminator(ctx context.Context, username string, discriminator int) (*User, error)
	EmailExists(ctx context.Context, email string) (bool, error)
	DiscriminatorsForUsername(ctx context.Context, username string) ([]int, error)
	UpdateRelationships(ctx context.Context, userID int64, add, remove []int64) error
}

var userTable = table.New(table.Metadata{
	Name:    "users",
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
		"date_of_birth",
		"verified_email",
		"about_me",
		"accent_color",
		"locale",
		"presence",
		"created_at",
		"updated_at",
	},
	PartKey: []string{"id"},
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

func (r *scyllaUserRepo) Create(ctx context.Context, u *User) error {
	u.CreatedAt = time.Now().UTC()
	u.UpdatedAt = u.CreatedAt

	stmt, names := userTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	if err := q.BindStruct(u).ExecRelease(); err != nil {
		return err
	}

	byEmail := UserByEmail{Email: u.Email, UserID: u.ID}
	stmt, names = usersByEmailTable.Insert()
	q = r.session.Query(stmt, names).WithContext(ctx)
	if err := q.BindStruct(&byEmail).ExecRelease(); err != nil {
		return err
	}

	byUD := UserByUsernameDiscriminator{
		Username:      u.Username,
		Discriminator: u.Discriminator,
		UserID:        u.ID,
	}
	stmt, names = usersByUsernameDiscriminatorTable.Insert()
	q = r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(&byUD).ExecRelease()
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

// GetByIDs fetches multiple users by ID. Returns a slice ordered by input; nil for missing users.
func (r *scyllaUserRepo) GetByIDs(ctx context.Context, ids []int64) ([]*User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]bool)
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	out := make([]*User, len(unique))
	for i, id := range unique {
		u, err := r.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out[i] = u
	}
	// Build id->user map and reorder to match input
	byID := make(map[int64]*User)
	for _, u := range out {
		if u != nil {
			byID[u.ID] = u
		}
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

func (r *scyllaUserRepo) UpdateRelationships(ctx context.Context, userID int64, add, remove []int64) error {
	for _, friendID := range add {
		q := r.session.Session.Query(
			"UPDATE users SET relationships = relationships + ? WHERE id = ?",
			[]int64{friendID}, userID,
		).WithContext(ctx)
		if err := q.Exec(); err != nil {
			return err
		}
	}
	for _, friendID := range remove {
		q := r.session.Session.Query(
			"UPDATE users SET relationships = relationships - ? WHERE id = ?",
			[]int64{friendID}, userID,
		).WithContext(ctx)
		if err := q.Exec(); err != nil {
			return err
		}
	}
	return nil
}