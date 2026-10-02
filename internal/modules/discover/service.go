package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// Admin is what the review side needs from the instance module: who may review, and the
// instance audit log. An interface so this package does not import instance.
type Admin interface {
	IsAdmin(ctx context.Context, userID int64) (bool, error)
	RecordAudit(ctx context.Context, actorID int64, action, targetType string, targetID int64, reason string)
}

type Service struct {
	repo   Repository
	spaces *spaces.Service
	apps   applications.Repository
	users  auth.UserRepository
	admin  Admin
	redis  *redis.Client
	prefix string
}

func NewService(repo Repository, spaceSvc *spaces.Service, apps applications.Repository, users auth.UserRepository, admin Admin, rdb *redis.Client, cachePrefix string) *Service {
	return &Service{repo: repo, spaces: spaceSvc, apps: apps, users: users, admin: admin, redis: rdb, prefix: cachePrefix}
}

// directoryTTL: the space directory is read far more than it changes, and a member count
// per listed space is a count per space, so that page is built at most this often. The
// bot directory is cheap (one application and one user per listing) and is built every
// time, so a bot that stops being public leaves it at once.
const directoryTTL = time.Minute

var tagRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} _\-]*$`)

func validKind(kind string) bool { return kind == KindSpace || kind == KindBot }

func cleanInput(in ApplyInput) (string, []string, error) {
	tagline := strings.TrimSpace(in.Tagline)
	if utf8.RuneCountInString(tagline) > MaxTagline || strings.ContainsAny(tagline, "\n\r") {
		return "", nil, ErrInvalidTagline
	}
	seen := map[string]struct{}{}
	tags := make([]string, 0, len(in.Tags))
	for _, raw := range in.Tags {
		tg := strings.ToLower(strings.Join(strings.Fields(raw), " "))
		if tg == "" {
			continue
		}
		if utf8.RuneCountInString(tg) > MaxTagLen || !tagRe.MatchString(tg) {
			return "", nil, ErrInvalidTags
		}
		if _, dup := seen[tg]; dup {
			continue
		}
		seen[tg] = struct{}{}
		tags = append(tags, tg)
	}
	if len(tags) > MaxTags {
		return "", nil, ErrInvalidTags
	}
	return tagline, tags, nil
}

// authorize: the actor may manage this listing and the thing can be listed here at all -
// a space hosted on this instance they may manage, or a public bot of an application
// they own. A mirrored space is listed on its origin; a private bot nobody else may add.
func (s *Service) authorize(ctx context.Context, actorID int64, kind string, id int64) error {
	switch kind {
	case KindSpace:
		sp, err := s.spaces.GetSpaceForUser(ctx, actorID, id)
		if err != nil {
			return err
		}
		if sp.Federation != nil {
			return ErrNotListable
		}
		return s.spaces.CanManageSpace(ctx, actorID, id)
	case KindBot:
		app, err := s.apps.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if app == nil {
			return applications.ErrNotFound
		}
		if app.OwnerID != actorID {
			return applications.ErrNotOwner
		}
		if !app.HasBot() || !app.BotPublic {
			return ErrNotListable
		}
		return nil
	}
	return ErrInvalidKind
}

// Status is the applicant's view of their listing (nil when they never applied).
func (s *Service) Status(ctx context.Context, actorID int64, kind string, id int64) (*Listing, error) {
	if err := s.authorize(ctx, actorID, kind, id); err != nil {
		return nil, err
	}
	l, err := s.repo.Get(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if l != nil && l.Tags == nil {
		l.Tags = []string{}
	}
	return l, nil
}

// Apply files or updates a listing. A new or declined one goes (back) to the queue; the
// words of an approved one may be edited without losing the listing - an administrator
// can always remove it.
func (s *Service) Apply(ctx context.Context, actorID int64, kind string, id int64, in ApplyInput) (*Listing, error) {
	if err := s.authorize(ctx, actorID, kind, id); err != nil {
		return nil, err
	}
	tagline, tags, err := cleanInput(in)
	if err != nil {
		return nil, err
	}
	existing, err := s.repo.Get(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	l := &Listing{Kind: kind, ID: id, Status: StatusPending, Tagline: tagline, Tags: tags, RequestedBy: actorID, RequestedAt: now}
	if existing != nil && existing.Status == StatusApproved {
		l.Status = StatusApproved
		l.RequestedAt = existing.RequestedAt
		l.ReviewedBy = existing.ReviewedBy
		l.ReviewedAt = existing.ReviewedAt
	}
	if err := s.repo.Put(ctx, l); err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	return l, nil
}

// Withdraw takes a listing (or an application for one) off Discover.
func (s *Service) Withdraw(ctx context.Context, actorID int64, kind string, id int64) error {
	if err := s.authorize(ctx, actorID, kind, id); err != nil {
		return err
	}
	if err := s.repo.Delete(ctx, kind, id); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

// ---- administrators -----------------------------------------------------------------------

func (s *Service) requireAdmin(ctx context.Context, actorID int64) error {
	ok, err := s.admin.IsAdmin(ctx, actorID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotAdmin
	}
	return nil
}

// Queue is every listing in one status, oldest application first, with what it lists
// and who asked.
func (s *Service) Queue(ctx context.Context, actorID int64, status string) ([]Entry, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	switch status {
	case StatusPending, StatusApproved, StatusDenied:
	default:
		return nil, ErrInvalidStatus
	}
	out := []Entry{}
	for _, kind := range []string{KindSpace, KindBot} {
		list, err := s.repo.List(ctx, kind)
		if err != nil {
			return nil, err
		}
		for i := range list {
			if list[i].Status != status {
				continue
			}
			if e, ok := s.entry(ctx, &list[i], true); ok {
				out = append(out, e)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RequestedAt.Before(out[j].RequestedAt) })
	if len(out) > MaxListings {
		out = out[:MaxListings]
	}
	return out, nil
}

// Review approves or declines a listing. Declining an approved one is how it comes off
// Discover; the note travels to the applicant either way.
func (s *Service) Review(ctx context.Context, actorID int64, kind string, id int64, decision, note string) (*Listing, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	if !validKind(kind) {
		return nil, ErrInvalidKind
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > MaxNote {
		return nil, ErrInvalidNote
	}
	l, err := s.repo.Get(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, ErrNotFound
	}
	var action string
	switch decision {
	case "approve":
		if l.Status == StatusApproved {
			return l, nil
		}
		l.Status = StatusApproved
		action = AuditApprove
	case "deny":
		switch l.Status {
		case StatusApproved:
			action = AuditRemove
		case StatusPending:
			action = AuditDeny
		default:
			return l, nil
		}
		l.Status = StatusDenied
	default:
		return nil, ErrInvalidDecision
	}
	now := time.Now().UTC()
	l.ReviewedBy = actorID
	l.ReviewedAt = &now
	l.Note = note
	if l.Tags == nil {
		l.Tags = []string{}
	}
	if err := s.repo.Put(ctx, l); err != nil {
		return nil, err
	}
	target := targetSpace
	if kind == KindBot {
		target = targetApplication
	}
	s.admin.RecordAudit(ctx, actorID, action, target, id, note)
	s.invalidate(ctx)
	return l, nil
}

// ---- the directory ------------------------------------------------------------------------

// Directory is the approved listings of one kind, optionally narrowed by a search over
// name, tagline, description and tags.
func (s *Service) Directory(ctx context.Context, kind, query string) ([]Entry, error) {
	if !validKind(kind) {
		return nil, ErrInvalidKind
	}
	entries, err := s.approved(ctx, kind)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return entries, nil
	}
	out := []Entry{}
	for _, e := range entries {
		if e.matches(q) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (e *Entry) matches(q string) bool {
	hay := []string{e.Tagline}
	hay = append(hay, e.Tags...)
	if e.Space != nil {
		hay = append(hay, e.Space.Name, e.Space.Description)
	}
	if e.Bot != nil {
		hay = append(hay, e.Bot.Name, e.Bot.Description)
		if e.Bot.Bot != nil {
			hay = append(hay, e.Bot.Bot.Username, e.Bot.Bot.DisplayName)
		}
	}
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), q) {
			return true
		}
	}
	return false
}

func (s *Service) approved(ctx context.Context, kind string) ([]Entry, error) {
	if kind == KindSpace {
		if cached, ok := s.cacheGet(ctx, kind); ok {
			return cached, nil
		}
	}
	list, err := s.repo.List(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for i := range list {
		if list[i].Status != StatusApproved {
			continue
		}
		if e, ok := s.entry(ctx, &list[i], false); ok {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if kind == KindSpace && a.Space != nil && b.Space != nil && a.Space.MemberCount != b.Space.MemberCount {
			return a.Space.MemberCount > b.Space.MemberCount
		}
		return strings.ToLower(a.name()) < strings.ToLower(b.name())
	})
	if len(out) > MaxListings {
		out = out[:MaxListings]
	}
	if kind == KindSpace {
		s.cacheSet(ctx, kind, out)
	}
	return out, nil
}

func (e *Entry) name() string {
	if e.Space != nil {
		return e.Space.Name
	}
	if e.Bot != nil {
		return e.Bot.Name
	}
	return ""
}

// entry attaches what a listing lists. A listing whose space or application is gone is
// dropped on the way past; one for a bot that stopped being public is just not shown.
// admin adds the applicant and keeps the review fields the public view leaves out.
func (s *Service) entry(ctx context.Context, l *Listing, admin bool) (Entry, bool) {
	e := Entry{Listing: *l}
	if e.Tags == nil {
		e.Tags = []string{}
	}
	switch l.Kind {
	case KindSpace:
		sp, err := s.spaces.GetSpace(ctx, l.ID)
		if err != nil {
			return e, false
		}
		if sp == nil {
			_ = s.repo.Delete(ctx, l.Kind, l.ID)
			return e, false
		}
		members, online, err := s.spaces.PublicCounts(ctx, sp.ID)
		if err != nil {
			logger.Err("discover", err, map[string]any{"space_id": sp.ID})
		}
		e.Space = &SpaceCard{ID: id.Format(sp.ID), Name: sp.Name, NameAcronym: sp.NameAcronym, Icon: sp.Icon, Banner: sp.Banner, Description: sp.Description, MemberCount: members, OnlineCount: online}
	case KindBot:
		app, err := s.apps.GetByID(ctx, l.ID)
		if err != nil {
			return e, false
		}
		if app == nil {
			_ = s.repo.Delete(ctx, l.Kind, l.ID)
			return e, false
		}
		if !app.HasBot() || !app.BotPublic {
			return e, false
		}
		card := &BotCard{ApplicationID: id.Format(app.ID), Name: app.Name, Description: app.Description, Icon: app.Icon}
		if bot, _ := s.users.GetByID(ctx, app.BotUserID); bot != nil {
			card.Bot = userRef(bot)
		}
		e.Bot = card
	default:
		return e, false
	}
	if admin {
		if u, _ := s.users.GetByID(ctx, l.RequestedBy); u != nil {
			e.RequestedByUser = userRef(u)
		}
	} else {
		e.RequestedBy = 0
		e.ReviewedBy = 0
		e.Note = ""
	}
	return e, true
}

func userRef(u *auth.User) *UserRef {
	return &UserRef{ID: id.Format(u.ID), Username: u.Username, Discriminator: fmt.Sprintf("%04d", u.Discriminator), DisplayName: u.DisplayName, Avatar: u.Avatar, Bot: u.Bot}
}

// JoinSpace is the Discover page's join: a space an administrator listed takes anyone
// on this instance who is not banned from it, no invite needed.
func (s *Service) JoinSpace(ctx context.Context, actorID, spaceID int64) (*spaces.Space, error) {
	l, err := s.repo.Get(ctx, KindSpace, spaceID)
	if err != nil {
		return nil, err
	}
	if l == nil || l.Status != StatusApproved {
		return nil, ErrNotApproved
	}
	sp, err := s.spaces.JoinListed(ctx, actorID, spaceID)
	if err != nil {
		return nil, err
	}
	// The card's member count changed; joins and leaves by other routes show within the
	// cache's minute.
	s.invalidate(ctx)
	return sp, nil
}

// ---- cache ----------------------------------------------------------------------------------

func (s *Service) cacheKey(kind string) string { return s.prefix + "discover:" + kind }

func (s *Service) cacheGet(ctx context.Context, kind string) ([]Entry, bool) {
	if s.redis == nil {
		return nil, false
	}
	raw, err := s.redis.Get(ctx, s.cacheKey(kind)).Bytes()
	if err != nil {
		return nil, false
	}
	var out []Entry
	if json.Unmarshal(raw, &out) != nil {
		return nil, false
	}
	return out, true
}

func (s *Service) cacheSet(ctx context.Context, kind string, entries []Entry) {
	if s.redis == nil {
		return
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = s.redis.Set(ctx, s.cacheKey(kind), raw, directoryTTL).Err()
}

func (s *Service) invalidate(ctx context.Context) {
	if s.redis == nil {
		return
	}
	_ = s.redis.Del(ctx, s.cacheKey(KindSpace)).Err()
}
