// Package instance owns the things that belong to a StrafeChat instance as a whole rather
// than to any space: who administers it, and the invite codes that admit new accounts when
// registration is closed.
package instance

import (
	"errors"
	"time"
)

// Limits chosen to match the space invite limits, so the two feel like one feature.
const (
	MaxInviteAgeSeconds = 30 * 24 * 60 * 60 // 30 days
	MaxInviteUses       = 1000
	MaxNoteLength       = 100
)

var (
	ErrInviteNotFound = errors.New("instance invite not found")
	ErrInvalidInvite  = errors.New("invite expiry must be at most 30 days and uses at most 1000")
	ErrNoteTooLong    = errors.New("invite note is too long")
	ErrNotAdmin       = errors.New("not an instance administrator")
)

// Invite admits one new *account* to this instance. Not to be confused with a space
// invite, which admits an existing account to a space.
type Invite struct {
	Code string `db:"code" json:"code"`
	// 0 when the invite came from the environment rather than a person.
	CreatedBy int64      `db:"created_by" json:"created_by,string"`
	Note      string     `db:"note" json:"note"`
	MaxUses   int        `db:"max_uses" json:"max_uses"`
	Uses      int        `db:"uses" json:"uses"`
	ExpiresAt *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
}

func (i *Invite) Expired(now time.Time) bool {
	return i.ExpiresAt != nil && !i.ExpiresAt.After(now)
}

// Exhausted reports whether the invite has no uses left. MaxUses 0 means unlimited.
func (i *Invite) Exhausted() bool {
	return i.MaxUses > 0 && i.Uses >= i.MaxUses
}

func (i *Invite) Usable(now time.Time) bool {
	return !i.Expired(now) && !i.Exhausted()
}

// CreateInviteInput is the body of POST /instance/invites. A nil pointer means "never
// expires, unlimited uses", which is the right default for a small private instance.
type CreateInviteInput struct {
	MaxAgeSeconds int    `json:"max_age_seconds"`
	MaxUses       int    `json:"max_uses"`
	Note          string `json:"note"`
}
