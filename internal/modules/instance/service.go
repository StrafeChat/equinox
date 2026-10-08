package instance

import (
	"context"
	"crypto/rand"
	"math/big"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
)

type Service struct {
	cfg  *config.Config
	repo Repository
	// mod is nil until SetModeration runs; every moderation method checks for it.
	mod *ModerationDeps
	// systemUserID is the instance's official account (see notices.go). 0 until
	// ProvisionSystemAccount runs, which disables notices.
	systemUserID int64
}

func NewService(cfg *config.Config, repo Repository) *Service {
	return &Service{cfg: cfg, repo: repo}
}

// IsAdmin is true for the account that claimed this instance (the first one registered)
// and for anything listed in INSTANCE_ADMINS. The environment is checked first and without
// a query, so an operator locked out of their account can always let themselves back in by
// editing .env - the reason that escape hatch exists at all.
func (s *Service) IsAdmin(ctx context.Context, userID int64) (bool, error) {
	for _, adminID := range s.cfg.Flags.InstanceAdmins {
		if adminID == userID {
			return true, nil
		}
	}
	return s.repo.IsInstanceAdmin(ctx, userID)
}

func (s *Service) requireAdmin(ctx context.Context, actorID int64) error {
	ok, err := s.IsAdmin(ctx, actorID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotAdmin
	}
	return nil
}

func (s *Service) CreateInvite(ctx context.Context, actorID int64, in *CreateInviteInput) (*Invite, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	maxAge, maxUses, note := 0, 0, ""
	if in != nil {
		maxAge, maxUses, note = in.MaxAgeSeconds, in.MaxUses, strings.TrimSpace(in.Note)
	}
	if maxAge < 0 || maxAge > MaxInviteAgeSeconds || maxUses < 0 || maxUses > MaxInviteUses {
		return nil, ErrInvalidInvite
	}
	if len([]rune(note)) > MaxNoteLength {
		return nil, ErrNoteTooLong
	}
	code, err := s.generateCode(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inv := &Invite{Code: code, CreatedBy: actorID, Note: note, MaxUses: maxUses, CreatedAt: now}
	if maxAge > 0 {
		exp := now.Add(time.Duration(maxAge) * time.Second)
		inv.ExpiresAt = &exp
	}
	if err := s.repo.CreateInvite(ctx, inv); err != nil {
		return nil, err
	}
	s.audit(ctx, actorID, AuditInviteCreate, "invite", 0, code)
	return inv, nil
}

// ListInvites returns the live invites, dropping (and deleting) any that have expired or
// run out - the listing is the only place they get cleaned up, which is fine for a list
// this small and keeps a spent code from lingering in the operator's view.
func (s *Service) ListInvites(ctx context.Context, actorID int64) ([]Invite, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	all, err := s.repo.ListInvites(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	live := make([]Invite, 0, len(all))
	for _, inv := range all {
		if inv.Usable(now) {
			live = append(live, inv)
			continue
		}
		_ = s.repo.DeleteInvite(ctx, inv.Code)
	}
	return live, nil
}

func (s *Service) RevokeInvite(ctx context.Context, actorID int64, code string) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	inv, err := s.repo.GetInviteByCode(ctx, code)
	if err != nil {
		return err
	}
	if inv == nil {
		return ErrInviteNotFound
	}
	if err := s.repo.DeleteInvite(ctx, code); err != nil {
		return err
	}
	s.audit(ctx, actorID, AuditInviteRevoke, "invite", 0, code)
	return nil
}

// CheckInvite reports whether a code would be accepted right now, without spending it, so
// the registration form can say "that code is not valid" before the user fills everything
// in. It deliberately returns nothing but a boolean.
func (s *Service) CheckInvite(ctx context.Context, code string) (bool, error) {
	inv, err := s.repo.GetInviteByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return false, err
	}
	return inv != nil && inv.Usable(time.Now().UTC()), nil
}

// Consume spends one use of the code. The false return means "this code will not admit
// anyone" - expired, exhausted or never existed - and is a value rather than an error
// because the auth module, which calls this, cannot import this package: the handler here
// imports auth for GetUser, and that would be a cycle. A non-nil error is infrastructure
// failing, which must not be reported to the user as a bad invite.
//
// The increment is a compare-and-set, so two people redeeming the last use of an invite at
// the same moment cannot both succeed.
func (s *Service) Consume(ctx context.Context, code string) (bool, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return false, nil
	}
	now := time.Now().UTC()
	// A few attempts, because a losing compare-and-set means somebody else incremented
	// between our read and our write, not that the invite is spent.
	for attempt := 0; attempt < 5; attempt++ {
		inv, err := s.repo.GetInviteByCode(ctx, code)
		if err != nil {
			return false, err
		}
		if inv == nil {
			return false, nil
		}
		if !inv.Usable(now) {
			_ = s.repo.DeleteInvite(ctx, code)
			return false, nil
		}
		applied, err := s.repo.TakeInviteUse(ctx, code, inv.Uses)
		if err != nil {
			return false, err
		}
		if applied {
			if inv.MaxUses > 0 && inv.Uses+1 >= inv.MaxUses {
				// Spent: drop it so it stops appearing in the operator's list.
				_ = s.repo.DeleteInvite(ctx, code)
			}
			return true, nil
		}
	}
	// Five lost races in a row on one code means it is being hammered; treat it as spent
	// rather than looping.
	return false, nil
}

// IsBootstrapped reports whether this instance already has its first account.
func (s *Service) IsBootstrapped(ctx context.Context) (bool, error) {
	return s.repo.IsBootstrapped(ctx)
}

// ClaimBootstrap tries to make userID the account that owns this instance. Exactly one
// caller can win, which is what stops a stranger racing the operator for admin on a
// freshly deployed instance.
func (s *Service) ClaimBootstrap(ctx context.Context, userID int64) (bool, error) {
	return s.repo.ClaimBootstrap(ctx, userID)
}

// ReleaseBootstrap undoes a claim whose registration then failed, so the next attempt can
// still be the first account.
func (s *Service) ReleaseBootstrap(ctx context.Context) error {
	return s.repo.ReleaseBootstrap(ctx)
}

func (s *Service) SetInstanceAdmin(ctx context.Context, userID int64, admin bool) error {
	return s.repo.SetInstanceAdmin(ctx, userID, admin)
}

// generateCode returns a short code in the same shape as a space invite's, so the two read
// alike to a user pasting one into a box.
func (s *Service) generateCode(ctx context.Context) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const length = 10
	for attempts := 0; attempts < 5; attempts++ {
		var b strings.Builder
		for i := 0; i < length; i++ {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return "", err
			}
			b.WriteByte(alphabet[n.Int64()])
		}
		code := b.String()
		existing, err := s.repo.GetInviteByCode(ctx, code)
		if err != nil {
			return "", err
		}
		if existing == nil {
			return code, nil
		}
	}
	return id.Format(id.Next()), nil
}
