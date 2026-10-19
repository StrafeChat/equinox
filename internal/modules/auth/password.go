package auth

import (
	"context"

	"golang.org/x/crypto/bcrypt"
)

// VerifyUserPassword re-checks an account's password for a step-up outside this module -
// the key-backup version endpoints call it through a hook, since a wrong "new backup" there
// is what lets a hijacked session harvest a user's room keys. It is the same check
// DisableTOTP and the other security-sensitive settings make, exported so a caller does not
// have to construct the whole auth Service to get it.
//
// Fails with ErrInvalidCredentials on a mismatch, an unknown user, or an account with no
// password of its own (remote shadows, bots).
func VerifyUserPassword(ctx context.Context, repo UserRepository, userID int64, password string) error {
	u, err := repo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrInvalidCredentials
	}
	return verifyPasswordFor(ctx, repo, u, password)
}

// verifyPasswordFor is the one place a re-auth compares a password. It deliberately
// re-fetches by email rather than trusting the *User in hand: GetByID may be cache-served,
// and the cache never carries PasswordHash (see cached_user_repo.go), so a cached user's
// hash reads as empty and would make every re-auth fail. GetByEmail is never cached, so it
// always sees the real column.
func verifyPasswordFor(ctx context.Context, repo UserRepository, u *User, password string) error {
	real, err := repo.GetByEmail(ctx, u.Email)
	if err != nil {
		return err
	}
	if real == nil || real.ID != u.ID || real.PasswordHash == "" {
		return ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(real.PasswordHash), []byte(password)) != nil {
		return ErrInvalidCredentials
	}
	return nil
}
