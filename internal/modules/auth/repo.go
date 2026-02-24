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
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByUsernameDiscriminator(ctx context.Context, username string, discriminator int) (*User, error)
	EmailExists(ctx context.Context, email string) (bool, error)
	DiscriminatorsForUsername(ctx context.Context, username string) ([]int, error)
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
	q.Release()

	byEmail := UserByEmail{Email: u.Email, UserID: u.ID}
	stmt, names = usersByEmailTable.Insert()
	q = r.session.Query(stmt, names).WithContext(ctx)
	if err := q.BindStruct(&byEmail).ExecRelease(); err != nil {
		return err
	}
	q.Release()

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