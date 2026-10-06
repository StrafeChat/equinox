package instance

import (
	"context"
	"errors"
	netmail "net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/mail"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// SpaceRemover is what the instance needs from the spaces module to take a space down.
// The spaces service implements it; it is an interface here only so tests can fake it.
type SpaceRemover interface {
	TakeDownSpace(ctx context.Context, actorID, spaceID int64) error
	// SetOfficial marks a space official (or not) and returns the updated space; the
	// instance does the admin check before calling it.
	SetOfficial(ctx context.Context, spaceID int64, official bool) (*spaces.Space, error)
}

// ModerationDeps is everything moderation reaches into beyond the instance's own tables.
// Set through SetModeration so NewService keeps its two-argument shape for the invite
// tests, and so a build without moderation wired in still serves invites.
type ModerationDeps struct {
	Repo     ModerationRepository
	Users    auth.UserRepository
	Sessions auth.SessionRepository
	Spaces   spaces.Repository
	Messages messages.Repository
	Rooms    rooms.Repository
	Remover  SpaceRemover
	Redis    *redis.Client
	Region   string
	// TwoFactor issues replacement recovery codes for an admin recovery action; Mailer
	// delivers them to the user. Either may be nil (2FA store absent / email off).
	TwoFactor auth.TwoFactorRepository
	Mailer    mail.Mailer
}

func (s *Service) SetModeration(d ModerationDeps) { s.mod = &d }

// SessionRevokedEvent is the gateway event a banned account receives on its own user
// channel just before its sockets are closed, so the client can say why rather than
// silently bouncing to the login page.
const SessionRevokedEvent = "SESSION_REVOKED"

// ------------------------------------------------------------------------------ bans ----

// IsBanned is what login asks. An expired ban is cleared on the way past so the bans
// page never shows one that no longer applies.
func (s *Service) IsBanned(ctx context.Context, userID int64) (*Ban, error) {
	if s.mod == nil {
		return nil, nil
	}
	b, err := s.mod.Repo.GetBan(ctx, userID)
	if err != nil || b == nil {
		return nil, err
	}
	if b.Expired(time.Now().UTC()) {
		_ = s.mod.Repo.DeleteBan(ctx, userID)
		return nil, nil
	}
	return b, nil
}

// BanReason is the auth module's view of IsBanned (see auth.BanChecker).
func (s *Service) BanReason(ctx context.Context, userID int64) (bool, string, error) {
	b, err := s.IsBanned(ctx, userID)
	if err != nil || b == nil {
		return false, "", err
	}
	return true, b.Reason, nil
}

func (s *Service) BanUser(ctx context.Context, actorID, userID int64, in BanInput) (*Ban, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	return s.banUser(ctx, actorID, userID, in)
}

// banUser is the permission-free core, shared with report resolution (already gated).
func (s *Service) banUser(ctx context.Context, actorID, userID int64, in BanInput) (*Ban, error) {
	if userID == actorID {
		return nil, ErrCannotBanSelf
	}
	reason := strings.TrimSpace(in.Reason)
	if len([]rune(reason)) > MaxBanReason || in.MaxAgeSeconds < 0 || in.MaxAgeSeconds > MaxBanAgeSeconds {
		return nil, ErrInvalidBan
	}
	u, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	if u.IsRemote() {
		// A shadow row has no sessions here and no login here; there is nothing to ban.
		return nil, ErrCannotBanRemote
	}
	if existing, err := s.IsBanned(ctx, userID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrAlreadyBanned
	}

	now := time.Now().UTC()
	b := &Ban{UserID: userID, BannedBy: actorID, Reason: reason, CreatedAt: now}
	if in.MaxAgeSeconds > 0 {
		exp := now.Add(time.Duration(in.MaxAgeSeconds) * time.Second)
		b.ExpiresAt = &exp
	}
	// Read before the sessions are revoked below - a revoked session is no longer "live".
	var sessionIPs []string
	if in.BanIPs {
		sessionIPs = s.liveSessionIPs(ctx, userID)
	}
	if err := s.mod.Repo.CreateBan(ctx, b); err != nil {
		return nil, err
	}

	// The ban is written first, so even if what follows fails the account cannot log in
	// again; a session that survives is caught by the next start of the gateway.
	if err := s.mod.Sessions.RevokeAllForUser(ctx, userID); err != nil {
		logger.Err("instance", err, map[string]any{"stage": "revoke_sessions", "user_id": userID})
	}
	if len(sessionIPs) > 0 {
		b.BannedIPs = s.banIPs(ctx, actorID, sessionIPs, reason, b.ExpiresAt)
	}
	if s.mod.Redis != nil {
		stargate.PublishToUser(ctx, s.mod.Redis, userID, SessionRevokedEvent, map[string]interface{}{
			"reason": "banned",
			"detail": reason,
		}, s.mod.Region)
	}
	s.audit(ctx, actorID, AuditUserBan, TargetUser, userID, reason)
	return b, nil
}

// SetUserBadges replaces a user's profile-badge bitfield. Only the badge bits may be set
// (auth.AllBadges); anything else is rejected so a caller cannot flip an unrelated user
// flag through this path. Returns the flags actually stored.
func (s *Service) SetUserBadges(ctx context.Context, actorID, userID int64, flags int) (int, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return 0, err
	}
	if flags&^auth.AllBadges != 0 {
		return 0, ErrInvalidBadges
	}
	u, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return 0, err
	}
	if u == nil {
		return 0, ErrUserNotFound
	}
	if u.IsRemote() {
		// A shadow row's profile is owned by its home instance; badges are ours to grant
		// only for our own accounts.
		return 0, ErrCannotBanRemote
	}
	updated, err := s.mod.Users.UpdateProfile(ctx, userID, &auth.ProfileUpdate{Flags: &flags})
	if err != nil {
		return 0, err
	}
	s.audit(ctx, actorID, AuditUserBadges, TargetUser, userID, badgeAuditReason(flags))
	if updated != nil {
		return auth.PublicFlags(updated), nil
	}
	return flags, nil
}

// SetUserEmail moves an account to a new address - the support action for "I lost access
// to my old mailbox". The address is normalised the way registration normalises it and
// must be unused. The administrator stands in for the verification link, so the new
// address counts as verified: an unverified one would lock the person out on an
// EMAIL_VERIFICATION instance, the opposite of what the admin was doing. Password and
// sessions are untouched. Both addresses go in the audit record (the dashboard already
// shows administrators the email).
func (s *Service) SetUserEmail(ctx context.Context, actorID, userID int64, email string) (*auth.User, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || len(email) > 254 {
		return nil, ErrInvalidEmail
	}
	if addr, err := netmail.ParseAddress(email); err != nil || addr.Address != email {
		return nil, ErrInvalidEmail
	}
	u, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	if u.IsRemote() {
		return nil, ErrCannotBanRemote
	}
	if u.Bot {
		return nil, ErrBotAccount
	}
	if u.Email == email {
		return u, nil
	}
	if err := s.mod.Users.UpdateEmail(ctx, userID, u.Email, email); err != nil {
		if errors.Is(err, auth.ErrEmailInUse) {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	if err := s.mod.Users.SetEmailVerified(ctx, userID, true); err != nil {
		logger.Err("instance", err, map[string]any{"stage": "email_verified", "user_id": userID})
	}
	s.audit(ctx, actorID, AuditUserEmail, TargetUser, userID, u.Email+" -> "+email)
	updated, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil || updated == nil {
		u.Email = email
		u.VerifiedEmail = true
		return u, nil
	}
	return updated, nil
}

// RegenerateUserRecoveryCodes issues a fresh set of 2FA recovery codes for a user (an admin
// support action for someone locked out of their authenticator) and, when email is configured
// and the account has an address, mails them. It returns the new codes and whether the email
// was sent, so the dashboard can show them for the admin to relay if mail is off. The old codes
// stop working immediately. The codes bypass only the 2FA step, never the password.
func (s *Service) RegenerateUserRecoveryCodes(ctx context.Context, actorID, userID int64) ([]string, bool, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, false, err
	}
	if s.mod == nil || s.mod.TwoFactor == nil {
		return nil, false, ErrRecoveryUnavailable
	}
	u, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	if u == nil {
		return nil, false, ErrUserNotFound
	}
	if u.IsRemote() || u.Bot {
		// A shadow row or bot has no 2FA / account recovery on this instance.
		return nil, false, ErrCannotBanRemote
	}
	codes, err := auth.AdminRegenerateRecoveryCodes(ctx, s.mod.TwoFactor, userID)
	if err != nil {
		return nil, false, err
	}
	emailed := false
	if s.mod.Mailer != nil && strings.Contains(u.Email, "@") {
		if err := s.sendRecoveryCodesEmail(ctx, u, codes); err != nil {
			logger.Err("instance", err, map[string]any{"stage": "recovery_email", "user_id": userID})
		} else {
			emailed = true
		}
	}
	s.audit(ctx, actorID, AuditUserRecovery, TargetUser, userID, "")
	// When the codes reached the user by email, don't hand them back to the admin too - they
	// belong to the user. Only when email is off (or failed) does the admin get them to relay.
	if emailed {
		return nil, true, nil
	}
	return codes, false, nil
}

func (s *Service) sendRecoveryCodesEmail(ctx context.Context, u *auth.User, codes []string) error {
	name := s.cfg.Mail.InstanceName
	if name == "" {
		name = "Strafe"
	}
	list := strings.Join(codes, "\n  ")
	text := "Hi " + u.Username + ",\n\n" +
		"An administrator generated new recovery codes for your account on " + name + ". " +
		"Your previous recovery codes no longer work.\n\n" +
		"Keep these somewhere safe. Each one can be used once to sign in if you lose your " +
		"authenticator (you will still need your password):\n\n  " + list + "\n\n" +
		"If you did not expect this, contact your instance administrator.\n"
	rows := ""
	for _, c := range codes {
		rows += "<li style=\"font-family:monospace;font-size:15px;letter-spacing:1px\">" + c + "</li>"
	}
	html := "<p>Hi " + u.Username + ",</p>" +
		"<p>An administrator generated new recovery codes for your account on <strong>" + name + "</strong>. " +
		"Your previous recovery codes no longer work.</p>" +
		"<p>Keep these somewhere safe. Each one can be used once to sign in if you lose your " +
		"authenticator (you will still need your password):</p>" +
		"<ul>" + rows + "</ul>" +
		"<p style=\"color:#666\">If you did not expect this, contact your instance administrator.</p>"
	return s.mod.Mailer.Send(ctx, mail.Message{
		To:      u.Email,
		Subject: "Your " + name + " recovery codes",
		Text:    text,
		HTML:    html,
	})
}

// SetSpaceOfficial marks a space as official - part of this instance - or clears it. Admin
// only. Returns the updated space so the caller can report the new state.
func (s *Service) SetSpaceOfficial(ctx context.Context, actorID, spaceID int64, official bool) (*spaces.Space, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	sp, err := s.mod.Remover.SetOfficial(ctx, spaceID, official)
	if err != nil {
		if errors.Is(err, spaces.ErrSpaceNotFound) {
			return nil, ErrSpaceNotFound
		}
		return nil, err
	}
	reason := "unset"
	if official {
		reason = "set"
	}
	s.audit(ctx, actorID, AuditSpaceOfficial, TargetSpace, spaceID, reason)
	return sp, nil
}

func (s *Service) UnbanUser(ctx context.Context, actorID, userID int64) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	b, err := s.mod.Repo.GetBan(ctx, userID)
	if err != nil {
		return err
	}
	if b == nil {
		return ErrNotBanned
	}
	if err := s.mod.Repo.DeleteBan(ctx, userID); err != nil {
		return err
	}
	s.audit(ctx, actorID, AuditUserUnban, TargetUser, userID, "")
	return nil
}

func (s *Service) ListBans(ctx context.Context, actorID int64) ([]Ban, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	all, err := s.mod.Repo.ListBans(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	live := make([]Ban, 0, len(all))
	for _, b := range all {
		if b.Expired(now) {
			_ = s.mod.Repo.DeleteBan(ctx, b.UserID)
			continue
		}
		live = append(live, b)
	}
	return live, nil
}

// ---------------------------------------------------------------------------- spaces ----

// SpaceDetail is what an administrator sees about a space they may not be a member of.
type SpaceDetail struct {
	Space       *spaces.Space
	Owner       *auth.User
	MemberCount int
	Reports     []ReportSummary
}

func (s *Service) GetSpaceDetail(ctx context.Context, actorID, spaceID int64) (*SpaceDetail, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	sp, err := s.mod.Spaces.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, ErrSpaceNotFound
	}
	members, err := s.mod.Spaces.ListMembers(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	owner, _ := s.mod.Users.GetByID(ctx, sp.OwnerID)
	reports, err := s.mod.Repo.ListReportsByTarget(ctx, TargetSpace, spaceID)
	if err != nil {
		return nil, err
	}
	return &SpaceDetail{Space: sp, Owner: owner, MemberCount: len(members), Reports: reports}, nil
}

func (s *Service) TakeDownSpace(ctx context.Context, actorID, spaceID int64, reason string) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	return s.takeDownSpace(ctx, actorID, spaceID, reason)
}

func (s *Service) takeDownSpace(ctx context.Context, actorID, spaceID int64, reason string) error {
	sp, err := s.mod.Spaces.GetByID(ctx, spaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return ErrSpaceNotFound
	}
	if err := s.mod.Remover.TakeDownSpace(ctx, actorID, spaceID); err != nil {
		return err
	}
	s.audit(ctx, actorID, AuditSpaceTakedown, TargetSpace, spaceID, strings.TrimSpace(reason))
	return nil
}

// ----------------------------------------------------------------------------- users ----

// SearchUsers finds accounts by the exact handles an administrator has to hand: an id, an
// email, or a username. Scylla has no substring search and this instance does not run one;
// exact lookups are what a support request actually contains.
func (s *Service) SearchUsers(ctx context.Context, actorID int64, query string) ([]*auth.User, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, ErrInvalidQuery
	}
	var out []*auth.User
	add := func(u *auth.User, err error) error {
		if err != nil {
			return err
		}
		if u != nil {
			out = append(out, u)
		}
		return nil
	}
	switch {
	case strings.Contains(q, "@"):
		if err := add(s.mod.Users.GetByEmail(ctx, strings.ToLower(q))); err != nil {
			return nil, err
		}
	default:
		if n, err := strconv.ParseInt(q, 10, 64); err == nil && n > 0 {
			if err := add(s.mod.Users.GetByID(ctx, n)); err != nil {
				return nil, err
			}
		}
		// A bare word is a username (unique, case-insensitive).
		if err := add(s.mod.Users.GetByUsername(ctx, strings.TrimPrefix(q, "@"))); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UserDetail is the support view: who they are, whether they are banned, where they are
// signed in from, what they belong to, and what has been said about them.
type UserDetail struct {
	User          *auth.User
	Ban           *Ban
	InstanceAdmin bool
	Sessions      []auth.Session
	Spaces        []*spaces.Space
	Reports       []ReportSummary
}

func (s *Service) GetUserDetail(ctx context.Context, actorID, userID int64) (*UserDetail, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	u, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	d := &UserDetail{User: u}
	if d.Ban, err = s.IsBanned(ctx, userID); err != nil {
		return nil, err
	}
	if d.InstanceAdmin, err = s.IsAdmin(ctx, userID); err != nil {
		return nil, err
	}
	sessions, err := s.mod.Sessions.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, sess := range sessions {
		if sess.RevokedAt.IsZero() && sess.ExpiresAt.After(now) {
			d.Sessions = append(d.Sessions, sess)
		}
	}
	if len(u.Spaces) > 0 {
		if d.Spaces, err = s.mod.Spaces.GetByIDs(ctx, u.Spaces); err != nil {
			return nil, err
		}
	}
	if d.Reports, err = s.mod.Repo.ListReportsByTarget(ctx, TargetUser, userID); err != nil {
		return nil, err
	}
	return d, nil
}

// --------------------------------------------------------------------------- reports ----

// CreateReport files a complaint. Any signed-in account may; the checks are that the
// target exists, the reason is one we know, and this person does not already have an open
// report about the same thing - which is what keeps a grudge from filling the queue.
func (s *Service) CreateReport(ctx context.Context, reporterID int64, in CreateReportInput) (*Report, error) {
	if s.mod == nil {
		return nil, ErrInvalidReport
	}
	if in.TargetType != TargetUser && in.TargetType != TargetSpace {
		return nil, ErrInvalidReport
	}
	targetID, err := id.Parse(strings.TrimSpace(in.TargetID))
	if err != nil || targetID <= 0 {
		return nil, ErrInvalidReport
	}
	if !validReason(in.Reason) {
		return nil, ErrInvalidReport
	}
	details := strings.TrimSpace(in.Details)
	if len([]rune(details)) > MaxReportDetails {
		return nil, ErrInvalidReport
	}
	if in.TargetType == TargetUser {
		if targetID == reporterID {
			return nil, ErrReportSelf
		}
		u, err := s.mod.Users.GetByID(ctx, targetID)
		if err != nil {
			return nil, err
		}
		if u == nil {
			return nil, ErrUserNotFound
		}
	} else {
		sp, err := s.mod.Spaces.GetByID(ctx, targetID)
		if err != nil {
			return nil, err
		}
		if sp == nil {
			return nil, ErrSpaceNotFound
		}
	}
	existing, err := s.mod.Repo.ListReportsByTarget(ctx, in.TargetType, targetID)
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		if e.ReporterID == reporterID && e.Status == ReportOpen {
			return nil, ErrDuplicateReport
		}
	}

	rep := &Report{
		ID:         id.Next(),
		ReporterID: reporterID,
		TargetType: in.TargetType,
		TargetID:   targetID,
		Reason:     in.Reason,
		Details:    details,
		Status:     ReportOpen,
		CreatedAt:  time.Now().UTC(),
	}
	rep.SpaceID = optionalID(in.SpaceID)
	rep.RoomID = optionalID(in.RoomID)
	rep.MessageID = optionalID(in.MessageID)
	if err := s.mod.Repo.CreateReport(ctx, rep); err != nil {
		return nil, err
	}
	return rep, nil
}

func optionalID(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	n, err := id.Parse(raw)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

func (s *Service) ListReports(ctx context.Context, actorID int64, status string) ([]Report, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	switch status {
	case ReportOpen, ReportResolved, ReportDismissed:
	default:
		status = ReportOpen
	}
	return s.mod.Repo.ListReportsByStatus(ctx, status, ReportPageSize)
}

// ReportDetail hydrates a report with the people and things it names, plus the cited
// message's text when the server can read it - a plaintext room - and a marker when it
// cannot, so the administrator knows the difference between "deleted" and "encrypted".
type ReportDetail struct {
	Report          *Report
	Reporter        *auth.User
	TargetUser      *auth.User
	TargetSpace     *spaces.Space
	Room            *rooms.Room
	Message         *messages.Message
	MessageReadable bool
}

func (s *Service) GetReport(ctx context.Context, actorID, reportID int64) (*ReportDetail, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	rep, err := s.mod.Repo.GetReport(ctx, reportID)
	if err != nil {
		return nil, err
	}
	if rep == nil {
		return nil, ErrReportNotFound
	}
	d := &ReportDetail{Report: rep}
	d.Reporter, _ = s.mod.Users.GetByID(ctx, rep.ReporterID)
	if rep.TargetType == TargetUser {
		d.TargetUser, _ = s.mod.Users.GetByID(ctx, rep.TargetID)
	} else {
		d.TargetSpace, _ = s.mod.Spaces.GetByID(ctx, rep.TargetID)
	}
	if rep.RoomID != nil {
		d.Room, _ = s.mod.Rooms.GetByID(ctx, *rep.RoomID)
		if rep.MessageID != nil {
			if m, err := s.mod.Messages.GetByID(ctx, *rep.RoomID, *rep.MessageID); err == nil && m != nil {
				d.Message = m
				d.MessageReadable = m.Plaintext != "" && m.DeletedAt == nil
			}
		}
	}
	return d, nil
}

// ResolveReport closes a report, doing what the action says first so a failure there
// leaves the report open rather than closed with nothing done.
func (s *Service) ResolveReport(ctx context.Context, actorID, reportID int64, in ResolveInput) (*Report, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	rep, err := s.mod.Repo.GetReport(ctx, reportID)
	if err != nil {
		return nil, err
	}
	if rep == nil {
		return nil, ErrReportNotFound
	}
	if rep.Status != ReportOpen {
		return nil, ErrReportClosed
	}
	note := strings.TrimSpace(in.Note)
	if len([]rune(note)) > MaxBanReason {
		return nil, ErrInvalidBan
	}

	from := rep.Status
	rep.Resolution = ResolutionNone
	action := AuditReportResolve
	switch in.Action {
	case "dismiss":
		rep.Status = ReportDismissed
		action = AuditReportDismiss
	case "resolve":
		rep.Status = ReportResolved
	case "ban_user":
		if rep.TargetType != TargetUser {
			return nil, ErrInvalidAction
		}
		if _, err := s.banUser(ctx, actorID, rep.TargetID, BanInput{Reason: note, MaxAgeSeconds: in.MaxAgeSeconds}); err != nil && err != ErrAlreadyBanned {
			return nil, err
		}
		rep.Status = ReportResolved
		rep.Resolution = ResolutionBanned
	case "remove_space":
		if rep.TargetType != TargetSpace {
			return nil, ErrInvalidAction
		}
		if err := s.takeDownSpace(ctx, actorID, rep.TargetID, note); err != nil && err != ErrSpaceNotFound {
			return nil, err
		}
		rep.Status = ReportResolved
		rep.Resolution = ResolutionSpaceRemoved
	default:
		return nil, ErrInvalidAction
	}
	now := time.Now().UTC()
	rep.ResolvedBy = &actorID
	rep.ResolvedAt = &now
	rep.ResolutionNote = note
	if err := s.mod.Repo.MoveReportStatus(ctx, rep, from); err != nil {
		return nil, err
	}
	s.audit(ctx, actorID, action, rep.TargetType, rep.TargetID, note)
	return rep, nil
}

// ----------------------------------------------------------------------------- audit ----

func (s *Service) audit(ctx context.Context, actorID int64, action, targetType string, targetID int64, reason string) {
	if s.mod == nil {
		return
	}
	e := &AuditEntry{ID: id.Next(), ActorID: actorID, Action: action, TargetType: targetType, TargetID: targetID, Reason: reason, CreatedAt: time.Now().UTC()}
	if err := s.mod.Repo.AppendAudit(ctx, e); err != nil {
		// Never fail the action for its record; say so loudly instead.
		logger.Err("instance", err, map[string]any{"stage": "audit", "action": action})
	}
}

// RecordAudit writes an instance audit entry for an administrator's action taken in
// another module (the Discover review queue).
func (s *Service) RecordAudit(ctx context.Context, actorID int64, action, targetType string, targetID int64, reason string) {
	s.audit(ctx, actorID, action, targetType, targetID, reason)
}

func (s *Service) ListAudit(ctx context.Context, actorID int64) ([]AuditEntry, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	return s.mod.Repo.ListAudit(ctx, AuditPageSize)
}

// Stats is the dashboard's front page: the counts an administrator glances at.
type Stats struct {
	OpenReports int  `json:"open_reports"`
	Bans        int  `json:"bans"`
	IPBans      int  `json:"ip_bans"`
	Invites     int  `json:"invites"`
	InviteOnly  bool `json:"invite_only"`
	// Instance-wide counts. -1 means "could not determine right now" (the count scan
	// failed or timed out), which the dashboard shows as a dash rather than a wrong zero.
	Accounts int64 `json:"accounts"`
	Online   int64 `json:"online"`
	Spaces   int64 `json:"spaces"`
}

func (s *Service) GetStats(ctx context.Context, actorID int64) (*Stats, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	open, err := s.mod.Repo.ListReportsByStatus(ctx, ReportOpen, ReportPageSize)
	if err != nil {
		return nil, err
	}
	bans, err := s.ListBans(ctx, actorID)
	if err != nil {
		return nil, err
	}
	ipBans, err := s.ListIPBans(ctx, actorID)
	if err != nil {
		return nil, err
	}
	invites, err := s.ListInvites(ctx, actorID)
	if err != nil {
		return nil, err
	}
	st := &Stats{
		OpenReports: len(open),
		Bans:        len(bans),
		IPBans:      len(ipBans),
		Invites:     len(invites),
		InviteOnly:  s.cfg.Flags.InviteOnly,
		Accounts:    s.cachedCount(ctx, "stats:accounts", s.mod.Repo.CountUsers),
		Spaces:      s.cachedCount(ctx, "stats:spaces", s.mod.Repo.CountSpaces),
		Online:      s.onlineCount(ctx),
	}
	return st, nil
}

// cachedCount reads a COUNT(*) once a minute at most: it serves the Redis-cached value when
// present and otherwise runs the scan (bounded, so a huge table cannot hang the request)
// and caches it. A scan that fails returns -1 - shown as a dash, never a misleading 0.
func (s *Service) cachedCount(ctx context.Context, key string, count func(context.Context) (int64, error)) int64 {
	if s.mod.Redis != nil {
		if v, err := s.mod.Redis.Get(ctx, key).Int64(); err == nil {
			return v
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	n, err := count(cctx)
	if err != nil {
		logger.Err("instance", err, map[string]any{"stage": "count", "key": key})
		return -1
	}
	if s.mod.Redis != nil {
		s.mod.Redis.Set(ctx, key, n, time.Minute)
	}
	return n
}

// onlineCount reads the size of the gateway's online-presence set. No fallback scan: the
// database's per-user online bool lags a crash, whereas the set is reset on gateway start.
func (s *Service) onlineCount(ctx context.Context) int64 {
	if s.mod.Redis == nil {
		return -1
	}
	n, err := s.mod.Redis.SCard(ctx, stargate.OnlinePresenceKey).Result()
	if err != nil {
		return -1
	}
	return n
}

// badgeAuditReason renders the badge set as a stable, readable list for the audit log.
func badgeAuditReason(flags int) string {
	names := []struct {
		bit  int
		name string
	}{
		{auth.BadgeFounder, "founder"}, {auth.BadgeStaff, "staff"}, {auth.BadgeSupport, "support"},
		{auth.BadgeContributor, "contributor"}, {auth.BadgeTranslator, "translator"},
		{auth.BadgeBugDiscloser, "bug_discloser"}, {auth.BadgeAlphaTester, "alpha_tester"},
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if flags&n.bit != 0 {
			out = append(out, n.name)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

// --------------------------------------------------------------- federation policy ----

var peerDomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// normalizePeerDomain lowercases, strips any scheme/path the admin may have pasted, and
// validates the bare host. Returns "" if it is not a plausible domain.
func normalizePeerDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	if i := strings.IndexByte(d, '/'); i >= 0 {
		d = d[:i]
	}
	if !peerDomainRe.MatchString(d) {
		return ""
	}
	return d
}

// ListFederationPolicy returns the editable runtime allow/block entries plus the read-only
// static env lists and this instance's own domain, for the admin dashboard.
func (s *Service) ListFederationPolicy(ctx context.Context, actorID int64) (*FederationPolicyView, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	if s.mod == nil {
		return nil, ErrFederationDisabled
	}
	entries, err := s.mod.Repo.ListPeerPolicy(ctx)
	if err != nil {
		return nil, err
	}
	return &FederationPolicyView{
		Enabled:  s.cfg.Federation.Enabled,
		Domain:   s.cfg.Federation.Domain,
		Entries:  entries,
		EnvAllow: s.cfg.Federation.Allowlist,
		EnvBlock: s.cfg.Federation.Blocklist,
	}, nil
}

// SetFederationPolicy adds (or overwrites) an allow/block rule for a peer domain.
func (s *Service) SetFederationPolicy(ctx context.Context, actorID int64, domain, kind string) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	if !s.cfg.Federation.Enabled || s.mod == nil {
		return ErrFederationDisabled
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != PolicyKindAllow && kind != PolicyKindBlock {
		return ErrInvalidPolicyKind
	}
	domain = normalizePeerDomain(domain)
	if domain == "" || domain == s.cfg.Federation.Domain {
		return ErrInvalidPeerDomain
	}
	e := &PeerPolicyEntry{Domain: domain, Kind: kind, AddedBy: actorID, CreatedAt: time.Now().UTC()}
	if err := s.mod.Repo.SetPeerPolicy(ctx, e); err != nil {
		return err
	}
	s.audit(ctx, actorID, AuditFederationPolicy, TargetFederation, 0, kind+" "+domain)
	s.reloadFederationPolicy(ctx)
	return nil
}

// RemoveFederationPolicy drops a peer domain's rule (back to the default for that domain).
func (s *Service) RemoveFederationPolicy(ctx context.Context, actorID int64, domain string) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	if !s.cfg.Federation.Enabled || s.mod == nil {
		return ErrFederationDisabled
	}
	domain = normalizePeerDomain(domain)
	if domain == "" {
		return ErrInvalidPeerDomain
	}
	if err := s.mod.Repo.RemovePeerPolicy(ctx, domain); err != nil {
		return err
	}
	s.audit(ctx, actorID, AuditFederationPolicy, TargetFederation, 0, "remove "+domain)
	s.reloadFederationPolicy(ctx)
	return nil
}

// LoadFederationPolicy reads the policy table into the shared config runtime lists, which is
// what IsAllowedPeer consults. Safe to call repeatedly.
func (s *Service) LoadFederationPolicy(ctx context.Context) error {
	if s.mod == nil || s.cfg.Federation.Runtime == nil {
		return nil
	}
	entries, err := s.mod.Repo.ListPeerPolicy(ctx)
	if err != nil {
		return err
	}
	var allow, block []string
	for _, e := range entries {
		switch e.Kind {
		case PolicyKindAllow:
			allow = append(allow, e.Domain)
		case PolicyKindBlock:
			block = append(block, e.Domain)
		}
	}
	s.cfg.Federation.Runtime.Set(allow, block)
	return nil
}

// reloadFederationPolicy refreshes this node's runtime lists and signals the other nodes.
func (s *Service) reloadFederationPolicy(ctx context.Context) {
	if err := s.LoadFederationPolicy(ctx); err != nil {
		logger.Err("instance", err, map[string]any{"stage": "fed_policy_reload"})
	}
	if s.mod != nil && s.mod.Redis != nil {
		if err := s.mod.Redis.Publish(ctx, config.FederationPolicyReloadChannel, "1").Err(); err != nil {
			logger.Err("instance", err, map[string]any{"stage": "fed_policy_publish"})
		}
	}
}

// StartFederationPolicy loads the policy at boot and subscribes to cross-node reload signals.
// No-op when federation is off or Redis is absent (then the boot load is the only sync).
func (s *Service) StartFederationPolicy(ctx context.Context) {
	if !s.cfg.Federation.Enabled || s.mod == nil {
		return
	}
	if err := s.LoadFederationPolicy(ctx); err != nil {
		logger.Err("instance", err, map[string]any{"stage": "fed_policy_load"})
	}
	if s.mod.Redis == nil {
		return
	}
	go func() {
		sub := s.mod.Redis.Subscribe(context.Background(), config.FederationPolicyReloadChannel)
		defer sub.Close()
		for range sub.Channel() {
			if err := s.LoadFederationPolicy(context.Background()); err != nil {
				logger.Err("instance", err, map[string]any{"stage": "fed_policy_reload_sub"})
			}
		}
	}()
}
