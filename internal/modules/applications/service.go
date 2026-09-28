package applications

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// usernameRe mirrors auth's username grammar; a bot's username is derived from the app name.
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{2,32}$`)

type Service struct {
	repo  Repository
	users auth.UserRepository
}

func NewService(repo Repository, users auth.UserRepository) *Service {
	return &Service{repo: repo, users: users}
}

// Secrets is the raw material returned once at creation and never stored: the client secret
// and, when a bot exists, its token.
type Secrets struct {
	ClientSecret string `json:"client_secret,omitempty"`
	BotToken     string `json:"bot_token,omitempty"`
}

func (s *Service) assertOwner(ctx context.Context, actorID, appID int64) (*Application, error) {
	a, err := s.repo.GetByID(ctx, appID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	if a.OwnerID != actorID {
		return nil, ErrNotOwner
	}
	return a, nil
}

func (s *Service) Create(ctx context.Context, ownerID int64, name string) (*Application, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxName {
		return nil, "", ErrInvalidName
	}
	existing, err := s.repo.ListByOwner(ctx, ownerID)
	if err != nil {
		return nil, "", err
	}
	if len(existing) >= MaxAppsPerUser {
		return nil, "", ErrTooMany
	}
	secret, secretHash := newToken()
	now := time.Now().UTC()
	a := &Application{
		ID:           id.Next(),
		OwnerID:      ownerID,
		Name:         name,
		SecretHash:   secretHash,
		RedirectURIs: []string{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.repo.Create(ctx, a); err != nil {
		return nil, "", err
	}
	return a, secret, nil
}

func (s *Service) List(ctx context.Context, ownerID int64) ([]Application, error) {
	return s.repo.ListByOwner(ctx, ownerID)
}

func (s *Service) Get(ctx context.Context, actorID, appID int64) (*Application, error) {
	return s.assertOwner(ctx, actorID, appID)
}

func (s *Service) Update(ctx context.Context, actorID, appID int64, in UpdateInput) (*Application, error) {
	a, err := s.assertOwner(ctx, actorID, appID)
	if err != nil {
		return nil, err
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || len([]rune(n)) > MaxName {
			return nil, ErrInvalidName
		}
		a.Name = n
	}
	if in.Description != nil {
		if len([]rune(*in.Description)) > MaxDescription {
			return nil, ErrInvalidField
		}
		a.Description = strings.TrimSpace(*in.Description)
	}
	if in.Icon != nil {
		a.Icon = *in.Icon
	}
	if in.RedirectURIs != nil {
		clean, err := cleanRedirects(*in.RedirectURIs)
		if err != nil {
			return nil, err
		}
		a.RedirectURIs = clean
	}
	a.UpdatedAt = time.Now().UTC()
	if err := s.repo.Update(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) Delete(ctx context.Context, actorID, appID int64) error {
	a, err := s.assertOwner(ctx, actorID, appID)
	if err != nil {
		return err
	}
	// The bot user row is left in place but its token is dropped with the app, so it can no
	// longer authenticate. It stays a member of any space it was added to until removed there.
	return s.repo.Delete(ctx, a.ID, a.OwnerID)
}

func (s *Service) ResetSecret(ctx context.Context, actorID, appID int64) (string, error) {
	a, err := s.assertOwner(ctx, actorID, appID)
	if err != nil {
		return "", err
	}
	secret, secretHash := newToken()
	a.SecretHash = secretHash
	a.UpdatedAt = time.Now().UTC()
	if err := s.repo.Update(ctx, a); err != nil {
		return "", err
	}
	return secret, nil
}

// AddBot creates the application's bot account (a real user with bot = true) and its first
// token. One bot per application.
func (s *Service) AddBot(ctx context.Context, actorID, appID int64) (*auth.User, string, error) {
	a, err := s.assertOwner(ctx, actorID, appID)
	if err != nil {
		return nil, "", err
	}
	if a.HasBot() {
		return nil, "", ErrHasBot
	}
	botUsername := sanitizeUsername(a.Name)
	disc, err := s.pickDiscriminator(ctx, botUsername)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	botID := id.Next()
	bot := &auth.User{
		ID: botID,
		// A synthetic, unique, non-login email: the account create path claims an
		// email-lookup row, and every bot sharing "" would collide on the first one. A bot
		// never signs in with it (it has no password), and it is not exposed.
		Email:         fmt.Sprintf("bot+%d@bots.invalid", botID),
		Username:      botUsername,
		Discriminator: disc,
		DisplayName:   a.Name,
		Bot:           true,
		Locale:        "en-US",
		Presence:      auth.UserPresence{Online: false, Status: "online"},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.users.Create(ctx, bot); err != nil {
		return nil, "", err
	}
	a.BotUserID = bot.ID
	a.UpdatedAt = now
	if err := s.repo.Update(ctx, a); err != nil {
		return nil, "", err
	}
	token, tokenHash := newToken()
	if err := s.repo.SetBotToken(ctx, a.ID, bot.ID, tokenHash); err != nil {
		return nil, "", err
	}
	return bot, token, nil
}

func (s *Service) ResetBotToken(ctx context.Context, actorID, appID int64) (string, error) {
	a, err := s.assertOwner(ctx, actorID, appID)
	if err != nil {
		return "", err
	}
	if !a.HasBot() {
		return "", ErrNoBot
	}
	token, tokenHash := newToken()
	if err := s.repo.SetBotToken(ctx, a.ID, a.BotUserID, tokenHash); err != nil {
		return "", err
	}
	return token, nil
}

// ResolveBotToken is what the auth middleware calls for `Authorization: Bot <token>`: it
// returns the bot user for a valid token, or nil.
func (s *Service) ResolveBotToken(ctx context.Context, rawToken string) (*auth.User, error) {
	h := hashToken(rawToken)
	if h == "" {
		return nil, nil
	}
	botUserID, _, err := s.repo.ResolveBotToken(ctx, h)
	if err != nil || botUserID == 0 {
		return nil, err
	}
	return s.users.GetByID(ctx, botUserID)
}

func (s *Service) pickDiscriminator(ctx context.Context, username string) (int, error) {
	used, err := s.users.DiscriminatorsForUsername(ctx, username)
	if err != nil {
		return 0, err
	}
	taken := make(map[int]struct{}, len(used))
	for _, d := range used {
		taken[d] = struct{}{}
	}
	for i := 0; i < 9999; i++ {
		var b [2]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		d := int(binary.BigEndian.Uint16(b[:])%9999) + 1
		if _, ok := taken[d]; !ok {
			return d, nil
		}
	}
	return 0, ErrInvalidField
}

// ---- helpers ----

// newToken returns a random token (hex) and its sha-256 hash (hex). Only the hash is stored.
func newToken() (raw, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	raw = hex.EncodeToString(b)
	return raw, hashToken(raw)
}

func hashToken(raw string) string {
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sanitizeUsername(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteByte('_')
		}
	}
	u := b.String()
	if len(u) < 2 {
		u = "bot"
	}
	if len(u) > 32 {
		u = u[:32]
	}
	if !usernameRe.MatchString(u) {
		u = "bot"
	}
	return u
}

func cleanRedirects(uris []string) ([]string, error) {
	if len(uris) > MaxRedirects {
		return nil, ErrInvalidRedirect
	}
	out := make([]string, 0, len(uris))
	seen := map[string]struct{}{}
	for _, raw := range uris {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, ErrInvalidRedirect
		}
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}
	return out, nil
}
