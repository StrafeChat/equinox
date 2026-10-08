package instance

import (
	"context"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
)

// The instance's official account posts moderation outcomes and admin notices to a user as
// a plain-text direct message. It is an ordinary users row flagged system=true with no
// password (it never logs in), provisioned once per keyspace and shared by every node.

const (
	// systemUsername is the handle the official account claims on first boot; an instance
	// where a person already holds it falls back to systemUsernameFallback.
	systemUsername         = "system"
	systemUsernameFallback = "strafe-system"
	// MaxNoticeRunes caps an admin-authored notice.
	MaxNoticeRunes = 2000
)

func (s *Service) instanceName() string {
	if s.cfg != nil && s.cfg.Mail.InstanceName != "" {
		return s.cfg.Mail.InstanceName
	}
	return "StrafeChat"
}

// systemEmailDomain is the host part of the official account's sentinel email. The address
// can never be used to log in (no password) or register (claimed), so it only needs to be
// a stable, unique string.
func (s *Service) systemEmailDomain() string {
	if s.cfg != nil && s.cfg.Federation.Domain != "" {
		return s.cfg.Federation.Domain
	}
	return "localhost"
}

// ProvisionSystemAccount makes sure the instance's official account exists and records its
// id on the service. Idempotent and safe on every boot; callers run it best-effort and log.
func (s *Service) ProvisionSystemAccount(ctx context.Context) error {
	if s.mod == nil || s.mod.Users == nil {
		return nil
	}
	existingID, err := s.repo.GetSystemAccountID(ctx)
	if err != nil {
		return err
	}
	if existingID != 0 {
		s.systemUserID = existingID
		return s.ensureSystemUser(ctx, existingID)
	}
	// Reserve an id first, so a failure to create the row below never leaves a half-made
	// account claiming the username - the next boot sees the claim and finishes the job.
	newID := id.Next()
	won, err := s.repo.ClaimSystemAccount(ctx, newID)
	if err != nil {
		return err
	}
	if !won {
		// Another node claimed it between our read and write.
		winner, gerr := s.repo.GetSystemAccountID(ctx)
		if gerr != nil {
			return gerr
		}
		s.systemUserID = winner
		return s.ensureSystemUser(ctx, winner)
	}
	s.systemUserID = newID
	return s.ensureSystemUser(ctx, newID)
}

// ensureSystemUser creates the users row for the official account if it is missing. The
// account has no password, so Login refuses it; its email is a sentinel that can never be
// used to register (claimed like any other, but unreachable).
func (s *Service) ensureSystemUser(ctx context.Context, userID int64) error {
	if userID == 0 {
		return nil
	}
	existing, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	now := time.Now().UTC()
	u := &auth.User{
		ID:            userID,
		Email:         "system@" + s.systemEmailDomain(),
		Username:      systemUsername,
		DisplayName:   s.instanceName(),
		System:        true,
		VerifiedEmail: true,
		Presence:      auth.UserPresence{Online: false, Status: "online"},
		Locale:        "en-US",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	err = s.mod.Users.Create(ctx, u)
	if err == auth.ErrUsernameTaken {
		// A person already has "system" on an instance that predates this feature.
		u.Username = systemUsernameFallback
		err = s.mod.Users.Create(ctx, u)
	}
	if err != nil {
		return err
	}
	logger.Info("instance", "provisioned official account user_id=%d username=%s", userID, u.Username)
	return nil
}

// notify delivers one plain-text message from the official account to a user, opening the
// official DM if it does not exist yet.
func (s *Service) notify(ctx context.Context, userID int64, text string) error {
	if s.systemUserID == 0 || userID == 0 || userID == s.systemUserID {
		return nil
	}
	if s.mod == nil || s.mod.RoomsSvc == nil || s.mod.MsgsSvc == nil {
		return nil
	}
	room, _, err := s.mod.RoomsSvc.EnsureSystemPM(ctx, s.systemUserID, userID)
	if err != nil {
		return err
	}
	_, err = s.mod.MsgsSvc.Create(ctx, s.systemUserID, room.ID, &messages.CreateMessageInput{Plaintext: text})
	return err
}

// notifyBestEffort sends a notice and swallows the error (logging it): a notice must never
// be the reason a moderation action fails.
func (s *Service) notifyBestEffort(ctx context.Context, userID int64, text string) {
	if err := s.notify(ctx, userID, text); err != nil {
		logger.Err("instance", err, map[string]any{"stage": "notice", "user_id": userID})
	}
}

// notifyReportResolved tells the reporter their report was handled, without leaking the
// internal resolution note or who was actioned.
func (s *Service) notifyReportResolved(ctx context.Context, rep *Report) {
	if rep == nil || rep.ReporterID == 0 {
		return
	}
	u, err := s.mod.Users.GetByID(ctx, rep.ReporterID)
	if err != nil || u == nil || u.IsRemote() || u.Bot || u.System {
		return
	}
	var msg string
	switch {
	case rep.Status == ReportDismissed:
		msg = "Thanks for your report. Our moderators reviewed it and decided no action was needed."
	case rep.Resolution != ResolutionNone:
		msg = "Thanks for your report. Our moderators reviewed it and took action."
	default:
		msg = "Thanks for your report. Our moderators have reviewed it."
	}
	s.notifyBestEffort(ctx, rep.ReporterID, msg)
}

// SendUserNotice is an admin sending a free-text official message to a user - a warning, an
// announcement, anything. Delivered by the instance's official account.
func (s *Service) SendUserNotice(ctx context.Context, actorID, userID int64, text string) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > MaxNoticeRunes {
		return ErrInvalidNotice
	}
	if s.systemUserID == 0 || s.mod == nil || s.mod.MsgsSvc == nil {
		return ErrNoticesUnavailable
	}
	u, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrUserNotFound
	}
	if u.IsRemote() || u.Bot || u.System {
		return ErrCannotNotice
	}
	if err := s.notify(ctx, userID, text); err != nil {
		return err
	}
	s.audit(ctx, actorID, AuditUserNotice, TargetUser, userID, text)
	return nil
}
