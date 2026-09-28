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
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInviteOnly         = errors.New("registration is invite-only")
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

type service struct {
	cfg   *config.Config
	repo  UserRepository
	srepo SessionRepository
}

func NewService(cfg *config.Config, repo UserRepository, srepo SessionRepository) Service {
	return &service{cfg: cfg, repo: repo, srepo: srepo}
}

func (s *service) Register(ctx context.Context, in RegisterInput) (*User, error) {
	if s.cfg.Flags.InviteOnly {
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
			Status:       "offline",
			CustomStatus: "",
		},
		DateOfBirth: in.DateOfBirth,
		Locale:      "en-US",
	}

	if err := s.repo.Create(ctx, u); err != nil {
		return nil, err
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
