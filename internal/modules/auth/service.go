package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInviteOnly         = errors.New("registration is invite-only")
	ErrInviteRequired     = errors.New("an invite code is required to register on this instance")
	ErrInviteInvalid      = errors.New("that invite code is not valid")
	ErrEmailInUse         = errors.New("email already in use")
	ErrWeakPassword       = errors.New("password does not meet requirements")
	ErrPasswordTooLong    = errors.New("password must be at most 72 bytes")
	ErrInvalidUsername    = errors.New("username must be 2-32 letters, digits, '_', '.' or '-'")
	ErrDiscriminatorInUse = errors.New("discriminator already in use for this username")
	ErrInvalidCredentials = errors.New("invalid email or password")
)

// usernameRe is the same grammar federation handles use (name#0001@domain, see
// federation.ParseHandle): a username containing '#', '@', ':' or whitespace could never be
// addressed by handle, locally or from another instance.
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{2,32}$`)

// bcryptMaxPasswordBytes is bcrypt's input limit; longer passwords are silently truncated
// by the algorithm, so reject them instead of pretending the extra characters count.
const bcryptMaxPasswordBytes = 72

// dummyHash is compared against when the email is unknown so a login attempt costs the
// same whether or not the account exists - otherwise response time reveals which emails
// are registered.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)

type Service interface {
	Register(ctx context.Context, in RegisterInput) (*User, error)
	Login(ctx context.Context, email, password, ip, userAgent string) (*User, string, error)
	Logout(ctx context.Context, userID, sessionID int64) error
	LogoutAll(ctx context.Context, userID int64) error
}

// InviteGate is the instance module's half of registration: whether this instance has had
// its first account, the one-time claim that makes that account the administrator, and
// spending an invite code. It is an interface because auth cannot import the instance
// module - that module's handler imports auth for GetUser, and the two would form a cycle.
//
// Consume reports "this code will not admit anyone" as false rather than an error, so an
// infrastructure failure is never shown to a user as a bad invite.
type InviteGate interface {
	IsBootstrapped(ctx context.Context) (bool, error)
	ClaimBootstrap(ctx context.Context, userID int64) (bool, error)
	ReleaseBootstrap(ctx context.Context) error
	Consume(ctx context.Context, code string) (bool, error)
	SetInstanceAdmin(ctx context.Context, userID int64, admin bool) error
}

// BanChecker is the instance module's answer to "may this account sign in". Same reason
// it is an interface as InviteGate: auth cannot import instance.
type BanChecker interface {
	BanReason(ctx context.Context, userID int64) (banned bool, reason string, err error)
}

// BannedError carries the reason so the login page can show it. Compared with
// errors.As, never ==.
type BannedError struct{ Reason string }

func (e *BannedError) Error() string { return "account is banned" }

type service struct {
	cfg   *config.Config
	repo  UserRepository
	srepo SessionRepository
	bans  BanChecker
	// gate is nil in tests and in any build that has not wired the instance module; with
	// no gate, an invite-only instance simply refuses every registration, which is the
	// behaviour this flag had before invites existed.
	gate InviteGate
}

// SetInviteGate wires the instance module in. Separate from NewService so that every
// existing caller and test keeps working unchanged.
func (s *service) SetInviteGate(g InviteGate) { s.gate = g }

func (s *service) SetBanChecker(b BanChecker) { s.bans = b }

func NewService(cfg *config.Config, repo UserRepository, srepo SessionRepository) Service {
	return &service{cfg: cfg, repo: repo, srepo: srepo}
}

// GateSetter is implemented by the service returned from NewService. Route setup uses it
// to hand in the instance module without widening the Service interface.
type GateSetter interface {
	SetInviteGate(g InviteGate)
	SetBanChecker(b BanChecker)
}

func (s *service) Register(ctx context.Context, in RegisterInput) (*User, error) {
	// Without the instance module there is no way to redeem an invite, so invite-only can
	// only mean "closed" - what the flag did before invites existed.
	if s.cfg.Flags.InviteOnly && s.gate == nil {
		return nil, ErrInviteOnly
	}

	// Normalize email; zog validates format and required fields.
	in.Email = strings.ToLower(in.Email)
	in.Username = strings.TrimSpace(in.Username)
	if !usernameRe.MatchString(in.Username) {
		return nil, ErrInvalidUsername
	}
	if len(in.Password) > bcryptMaxPasswordBytes {
		return nil, ErrPasswordTooLong
	}

	// Fast path only: the repository's Create claims the email and username#discriminator
	// with lightweight transactions, which is what actually prevents two concurrent
	// registrations from both succeeding.
	exists, err := s.repo.EmailExists(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrEmailInUse
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	userID := id.Next()

	// The registration gate sits here, after every check that can cheaply say no, so an
	// invite is never spent on a request that was going to fail anyway.
	//
	// The first account on an instance is special: it is the operator's own, it becomes
	// the instance administrator, and it needs no invite because there is nobody to have
	// issued one yet. The claim is a lightweight transaction exactly so that a stranger
	// watching a fresh deployment cannot race the operator for that account.
	firstAccount := false
	if s.gate != nil {
		bootstrapped, err := s.gate.IsBootstrapped(ctx)
		if err != nil {
			return nil, err
		}
		if !bootstrapped {
			claimed, err := s.gate.ClaimBootstrap(ctx, userID)
			if err != nil {
				return nil, err
			}
			firstAccount = claimed
		}
	}
	registered := false
	if firstAccount {
		// Hand the claim back if this registration does not finish, or the instance is
		// left with no first account and no way to create one.
		defer func() {
			if !registered {
				_ = s.gate.ReleaseBootstrap(ctx)
			}
		}()
	}
	if s.cfg.Flags.InviteOnly && !firstAccount {
		code := strings.TrimSpace(in.Invite)
		if code == "" {
			return nil, ErrInviteRequired
		}
		ok, err := s.gate.Consume(ctx, code)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrInviteInvalid
		}
	}

	var discriminator int
	if in.Discriminator != nil {
		existing, err := s.repo.GetByUsernameDiscriminator(ctx, in.Username, *in.Discriminator)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return nil, ErrDiscriminatorInUse
		}
		discriminator = *in.Discriminator
	} else {
		d, err := s.pickUniqueDiscriminator(ctx, in.Username)
		if err != nil {
			return nil, err
		}
		discriminator = d
	}

	u := &User{
		ID:            userID,
		Email:         in.Email,
		PasswordHash:  string(hash),
		Username:      in.Username,
		Discriminator: discriminator,
		DisplayName:   in.Username,
		Bot:           false,
		System:        false,
		VerifiedEmail: false,
		Presence: UserPresence{
			Online:       false,
			// The user's *chosen* status; "offline" is not one of the choices
			// (online/idle/dnd/invisible), it is what a disconnected user reads as.
			Status:       "online",
			CustomStatus: "",
		},
		DateOfBirth: in.DateOfBirth,
		Locale:      "en-US",
	}

	if err := s.repo.Create(ctx, u); err != nil {
		return nil, err
	}
	registered = true

	if firstAccount {
		// Best effort on purpose: the account exists either way, and refusing the
		// registration now would be worse than an instance whose admin bit needs setting
		// through INSTANCE_ADMINS - which is what that setting is for.
		if err := s.gate.SetInstanceAdmin(ctx, userID, true); err != nil {
			logger.Err("auth", err, map[string]any{"stage": "instance_admin", "user_id": userID})
		}
	}

	return u, nil
}

func (s *service) pickUniqueDiscriminator(ctx context.Context, username string) (int, error) {
	used, err := s.repo.DiscriminatorsForUsername(ctx, username)
	if err != nil {
		return 0, err
	}
	usedSet := make(map[int]struct{}, len(used))
	for _, d := range used {
		usedSet[d] = struct{}{}
	}

	for i := 0; i < 9999; i++ {
		var b [2]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		d := int(binary.BigEndian.Uint16(b[:])%9999) + 1
		if _, taken := usedSet[d]; !taken {
			return d, nil
		}
	}
	return 0, ErrInvalidUsername
}

func (s *service) Login(ctx context.Context, email, password, ip, userAgent string) (*User, string, error) {
	email = strings.ToLower(email)

	u, err := s.repo.GetByEmail(ctx, email)
	if err != nil || u == nil || u.IsRemote() || u.PasswordHash == "" {
		// Burn the same bcrypt cost as a real comparison (see dummyHash).
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, "", ErrInvalidCredentials
	}

	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, "", ErrInvalidCredentials
	}

	// After the password, not before: a banned account still has to prove it is that
	// account before learning it is banned, or the endpoint tells anyone who is.
	if s.bans != nil {
		banned, reason, err := s.bans.BanReason(ctx, u.ID)
		if err != nil {
			return nil, "", err
		}
		if banned {
			return nil, "", &BannedError{Reason: reason}
		}
	}

	ttl := time.Duration(s.cfg.Session.TTLSeconds) * time.Second
	tokenBytes := s.cfg.Session.TokenBytes
	if tokenBytes < 16 {
		tokenBytes = 32
	}

	rawToken := make([]byte, tokenBytes)
	if _, err := rand.Read(rawToken); err != nil {
		return nil, "", err
	}
	tokenHex := hex.EncodeToString(rawToken)

	hash := sha256.Sum256(rawToken)
	tokenHash := hex.EncodeToString(hash[:])

	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	sessionID := id.Next()

	sess := &Session{
		UserID:     u.ID,
		SessionID:  sessionID,
		TokenHash:  tokenHash,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
		IPAddress:  ip,
		UserAgent:  userAgent,
		DeviceName: "",
	}

	if err := s.srepo.Create(ctx, sess); err != nil {
		return nil, "", err
	}

	return u, tokenHex, nil
}

func (s *service) Logout(ctx context.Context, userID, sessionID int64) error {
	return s.srepo.Revoke(ctx, userID, sessionID)
}

func (s *service) LogoutAll(ctx context.Context, userID int64) error {
	return s.srepo.RevokeAllForUser(ctx, userID)
}
