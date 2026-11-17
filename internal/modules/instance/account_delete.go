package instance

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/voice"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// An instance administrator deleting a local account or bot.
//
// The account's access is cut first (sessions and bot token revoked, SESSION_REVOKED so the
// gateway drops its sockets), everything that is only the account's is torn down through
// the modules' own leave / unfriend / revoke paths - so spaces, peers, voice and encrypted
// groups hear about it the way they already do - peers get the signed account-deleted
// notice, and last the users row goes together with its users_by_username and
// users_by_email claims, in one logged batch: the name and the address are free the moment
// the request returns. Messages, reactions and pins stay and render as Deleted User.
//
// Identity everywhere (E2EE, federation, client stores) is the snowflake id / FID, never
// reused, so a new account with the same name can never inherit anything; the one
// handle-keyed cache, user:name:<name>, is purged by the users repository.
//
// Every step is idempotent: a run that failed part-way is finished by the same request.
// Only cutting access and the identity batch must succeed (DeletionStageError); every other
// step is logged and reported in the result's warnings.

const (
	// deletionTimeout bounds one run. It runs detached from the request (a closed browser
	// tab must not abort a deletion half-way).
	deletionTimeout = 2 * time.Minute
	// deletionLockTTL frees the Redis lock if the process dies mid-run.
	deletionLockTTL = 10 * time.Minute
)

// purgeOptions shape one run of the pipeline.
type purgeOptions struct {
	takeDownSpaces     bool
	deleteApplications bool
	// deleteBotApp deletes a bot's application with it (a bot deleted on its own); false
	// when the caller deletes the application itself (an owner's cascade, or the developer
	// deleting their application).
	deleteBotApp bool
	// auditNote is appended to the audit reason (who asked, when not an administrator).
	auditNote string
}

// ------------------------------------------------------------------------- preview ----

// PreviewAccountDeletion is what deleting an account would do: why it cannot be done at all
// (refusal), what stands in the way (blockers) and what would go (counts). Read-only.
func (s *Service) PreviewAccountDeletion(ctx context.Context, actorID, userID int64) (*DeletionPreview, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	if s.mod == nil || s.del == nil {
		return nil, ErrDeletionUnavailable
	}
	u, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	p := &DeletionPreview{User: u, Kind: kindOf(u)}
	if p.InstanceAdmin, err = s.IsAdmin(ctx, userID); err != nil {
		return nil, err
	}
	if p.Refusal, err = s.deletionRefusal(ctx, actorID, u); err != nil {
		return nil, err
	}
	if p.Refusal == RefusalRemote {
		// A shadow belongs to its home instance; there is nothing more to say about it here.
		return p, nil
	}
	snap, err := s.snapshotAccount(ctx, u)
	if err != nil {
		return nil, err
	}
	if p.Blockers, err = s.deletionBlockers(ctx, u, snap.spaceIDs); err != nil {
		return nil, err
	}
	p.Counts = snap.counts
	if u.Bot {
		app, err := s.del.Apps.AppForBot(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		if app == nil {
			p.Application = &BotApplication{Orphan: true}
		} else {
			owner, _ := s.mod.Users.GetByID(ctx, app.OwnerID)
			p.Application = &BotApplication{ID: app.ID, Name: app.Name, Owner: owner}
		}
	}
	return p, nil
}

func kindOf(u *auth.User) string {
	if u.Bot {
		return DeletionKindBot
	}
	return DeletionKindUser
}

// deletionRefusal is why an account can never be deleted here ("" when it can): oneself,
// the official account, an instance administrator (there is no demote endpoint), a shadow
// of another instance's account.
func (s *Service) deletionRefusal(ctx context.Context, actorID int64, u *auth.User) (string, error) {
	if u.ID == actorID {
		return RefusalSelf, nil
	}
	if u.IsRemote() {
		return RefusalRemote, nil
	}
	if u.System {
		return RefusalSystem, nil
	}
	systemID := s.systemUserID
	if systemID == 0 && s.repo != nil {
		if sid, err := s.repo.GetSystemAccountID(ctx); err == nil {
			systemID = sid
		}
	}
	if systemID != 0 && u.ID == systemID {
		return RefusalSystem, nil
	}
	admin, err := s.IsAdmin(ctx, u.ID)
	if err != nil {
		return "", err
	}
	if admin {
		return RefusalAdmin, nil
	}
	return "", nil
}

func refusalError(r string) error {
	switch r {
	case RefusalSelf:
		return ErrDeleteSelf
	case RefusalSystem:
		return ErrDeleteSystem
	case RefusalAdmin:
		return ErrDeleteAdmin
	case RefusalRemote:
		return ErrCannotBanRemote
	}
	return nil
}

// deletionBlockers lists the spaces and applications the account owns.
func (s *Service) deletionBlockers(ctx context.Context, u *auth.User, spaceIDs []int64) (DeletionBlockers, error) {
	var b DeletionBlockers
	owned, err := s.del.Spaces.OwnedSpaces(ctx, u.ID, spaceIDs)
	if err != nil {
		return b, err
	}
	for _, sp := range owned {
		b.OwnedSpaces = append(b.OwnedSpaces, OwnedSpaceBlocker{ID: sp.ID, Name: sp.Name, MemberCount: sp.MemberCount, HostedOn: sp.HostedOn})
	}
	apps, err := s.del.Apps.ListByOwner(ctx, u.ID)
	if err != nil {
		return b, err
	}
	for _, a := range apps {
		blk := OwnedApplicationBlocker{ID: a.ID, Name: a.Name, BotUserID: a.BotUserID}
		if a.HasBot() {
			if bot, err := s.mod.Users.GetByID(ctx, a.BotUserID); err == nil && bot != nil {
				blk.BotUsername = bot.Username
				if ids, err := s.del.Spaces.AccountSpaceIDs(ctx, bot.ID, bot.Spaces); err == nil {
					blk.BotSpaceCount = len(ids)
				}
			}
		}
		b.OwnedApplications = append(b.OwnedApplications, blk)
	}
	return b, nil
}

// countLiveSessions counts the account's unrevoked, unexpired sessions (0 on a read error:
// it is only a number in the preview and the audit line).
func (s *Service) countLiveSessions(ctx context.Context, userID int64) int {
	sessions, err := s.mod.Sessions.ListByUser(ctx, userID)
	if err != nil {
		return 0
	}
	now := time.Now().UTC()
	n := 0
	for _, sess := range sessions {
		if sess.RevokedAt.IsZero() && sess.ExpiresAt.After(now) {
			n++
		}
	}
	return n
}

// accountSnapshot is what the pipeline reads once, before anything is torn down.
type accountSnapshot struct {
	counts       DeletionCounts
	spaceIDs     []int64
	groupPMs     []int64
	requestPeers []int64
	audience     map[string]struct{}
}

func (s *Service) snapshotAccount(ctx context.Context, u *auth.User) (*accountSnapshot, error) {
	snap := &accountSnapshot{}
	c := &snap.counts
	c.Friends = len(u.Relationships)

	spaceIDs, err := s.del.Spaces.AccountSpaceIDs(ctx, u.ID, u.Spaces)
	if err != nil {
		return nil, err
	}
	snap.spaceIDs = spaceIDs
	c.Spaces = len(spaceIDs)

	incoming, outgoing, err := s.del.Relationships.RequestCounterparts(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	c.PendingRequests = len(incoming) + len(outgoing)
	snap.requestPeers = append(append([]int64{}, incoming...), outgoing...)

	rows, err := s.del.Rooms.ListByUser(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	roomIDs := make([]int64, 0, len(rows))
	for _, r := range rows {
		roomIDs = append(roomIDs, r.RoomID)
	}
	if len(roomIDs) > 0 {
		list, err := s.del.Rooms.GetByIDs(ctx, roomIDs)
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			if r == nil {
				continue
			}
			switch r.Type {
			case rooms.TypePM:
				c.PMs++
			case rooms.TypeGroupPM:
				c.GroupPMs++
				snap.groupPMs = append(snap.groupPMs, r.ID)
			}
		}
	}

	c.Sessions = s.countLiveSessions(ctx, u.ID)
	if c.Devices, err = s.del.Devices.CountDevices(ctx, u.ID); err != nil {
		return nil, err
	}
	if c.AuthorizedApps, err = s.del.OAuth.CountGrants(ctx, u.ID); err != nil {
		return nil, err
	}
	if c.PushSubscriptions, err = s.del.Push.CountSubscriptions(ctx, u.ID); err != nil {
		return nil, err
	}
	reports, err := s.mod.Repo.ListReportsByTarget(ctx, TargetUser, u.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range reports {
		if r.Status == ReportOpen {
			c.OpenReportsAbout++
		}
	}
	if s.del.Federation != nil {
		snap.audience = s.del.Federation.DeletionAudience(ctx, u, snap.requestPeers)
	}
	c.RemoteInstances = len(snap.audience)
	return snap, nil
}

// -------------------------------------------------------------------------- delete ----

// DeleteAccount is an administrator deleting a local account or bot (see the top of this
// file). The username must be confirmed exactly; owned spaces and applications block it
// unless take_down_spaces / delete_applications say otherwise, and a space owned on another
// instance always blocks.
func (s *Service) DeleteAccount(ctx context.Context, actorID, userID int64, in DeleteAccountInput) (*DeletionResult, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	if s.mod == nil || s.del == nil {
		return nil, ErrDeletionUnavailable
	}
	u, err := s.mod.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	refusal, err := s.deletionRefusal(ctx, actorID, u)
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return nil, refusalError(refusal)
	}
	if strings.TrimSpace(in.ConfirmUsername) != u.Username {
		return nil, ErrConfirmMismatch
	}
	return s.runDeletion(ctx, actorID, u, purgeOptions{
		takeDownSpaces:     in.TakeDownSpaces,
		deleteApplications: in.DeleteApplications,
		deleteBotApp:       true,
	})
}

// DeleteApplicationBot is the developer deleting their own application: its bot account
// goes through the same pipeline (kind bot), so the bot's name is freed. The applications
// module checked ownership and deletes the application itself afterwards. Not an
// administrator action, but it deletes an account, so it is in the instance audit log with
// the developer as the actor.
func (s *Service) DeleteApplicationBot(ctx context.Context, actorID, botUserID int64) error {
	if s.mod == nil || s.del == nil {
		return nil // nothing wired: the application module keeps its old behaviour
	}
	bot, err := s.mod.Users.GetByID(ctx, botUserID)
	if err != nil {
		return err
	}
	if bot == nil || !bot.Bot || bot.IsRemote() || bot.System {
		return nil
	}
	_, err = s.runDeletion(ctx, actorID, bot, purgeOptions{auditNote: "application deleted by its owner"})
	// The developer's request is answered by the applications module: say what stands in the
	// way in its terms (409s there), not as an internal error. A bot owning a space is the
	// one blocker a bot can have; taking a space down is an administrator's call, not a
	// side effect of deleting an application.
	var blocked *DeletionBlockedError
	switch {
	case errors.As(err, &blocked):
		return applications.ErrBotOwnsSpace
	case errors.Is(err, ErrDeletionInProgress):
		return applications.ErrBotBeingDeleted
	}
	return err
}

// runDeletion checks the blockers, takes the lock and runs the pipeline, detached from the
// request's context.
func (s *Service) runDeletion(ctx context.Context, actorID int64, u *auth.User, o purgeOptions) (*DeletionResult, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deletionTimeout)
	defer cancel()
	unlock, err := s.lockDeletion(ctx, actorID, u.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	res := &DeletionResult{UserID: u.ID, Username: u.Username, Kind: kindOf(u)}
	if err := s.purgeAccount(ctx, actorID, u, o, res); err != nil {
		return nil, err
	}
	return res, nil
}

// lockDeletion makes sure one account is deleted by one run at a time: a process-local
// claim, and a Redis one (NX, with a TTL in case the process dies) for the other API nodes.
func (s *Service) lockDeletion(ctx context.Context, actorID, userID int64) (func(), error) {
	if _, taken := s.deleting.LoadOrStore(userID, struct{}{}); taken {
		return nil, ErrDeletionInProgress
	}
	key := ""
	if rdb := s.mod.Redis; rdb != nil {
		key = auth.RedisKeyPrefix(s.cfg) + "account:delete:" + strconv.FormatInt(userID, 10)
		ok, err := rdb.SetNX(ctx, key, strconv.FormatInt(actorID, 10), deletionLockTTL).Result()
		if err != nil {
			s.deleting.Delete(userID)
			return nil, err
		}
		if !ok {
			s.deleting.Delete(userID)
			return nil, ErrDeletionInProgress
		}
	}
	return func() {
		if key != "" {
			_ = s.mod.Redis.Del(context.Background(), key).Err()
		}
		s.deleting.Delete(userID)
	}, nil
}

// purgeAccount is the pipeline for one account (nested for an owned application's bot).
func (s *Service) purgeAccount(ctx context.Context, actorID int64, u *auth.User, o purgeOptions, res *DeletionResult) error {
	warn := func(stage string, err error) {
		if err == nil {
			return
		}
		logger.Err("instance", err, map[string]any{"stage": "account_delete_" + stage, "user_id": u.ID})
		res.Warnings = append(res.Warnings, stage+": "+err.Error())
	}

	// 0. Blockers, from the spaces the account is in right now.
	spaceIDs, err := s.del.Spaces.AccountSpaceIDs(ctx, u.ID, u.Spaces)
	if err != nil {
		return err
	}
	blockers, err := s.deletionBlockers(ctx, u, spaceIDs)
	if err != nil {
		return err
	}
	if blockers.blocks(DeleteAccountInput{TakeDownSpaces: o.takeDownSpaces, DeleteApplications: o.deleteApplications}) {
		return &DeletionBlockedError{Blockers: blockers}
	}

	// 2. Cut access (must): sessions, a bot's token, then the sockets. The live sessions
	// are counted first - after this they are no longer live.
	liveSessions := s.countLiveSessions(ctx, u.ID)
	if err := s.mod.Sessions.RevokeAllForUser(ctx, u.ID); err != nil {
		return &DeletionStageError{Stage: "sessions", Err: err}
	}
	var botApp *applicationRef
	if u.Bot {
		app, err := s.del.Apps.AppForBot(ctx, u.ID)
		if err != nil {
			return &DeletionStageError{Stage: "bot_token", Err: err}
		}
		if app != nil {
			if err := s.del.Apps.RevokeBotToken(ctx, app.ID); err != nil {
				return &DeletionStageError{Stage: "bot_token", Err: err}
			}
			botApp = &applicationRef{id: app.ID, name: app.Name}
		}
	}
	s.publishToUser(ctx, u.ID, SessionRevokedEvent, map[string]interface{}{"reason": stargate.SessionRevokedDeleted})

	// 3. Snapshot, before anything is torn down.
	snap, err := s.snapshotAccount(ctx, u)
	if err != nil {
		return &DeletionStageError{Stage: "snapshot", Err: err}
	}
	snap.counts.Sessions = liveSessions
	res.Counts = snap.counts

	// 4. Live clients see the placeholder at once.
	if s.del.Broadcast != nil {
		s.del.Broadcast(ctx, u)
	}

	// 5. Owned spaces hosted here (only with take_down_spaces; the blockers said so).
	takenDown := map[int64]bool{}
	for _, sp := range blockers.OwnedSpaces {
		if sp.HostedOn != "" || !o.takeDownSpaces {
			continue
		}
		if err := s.takeDownSpace(ctx, actorID, sp.ID, "owner account "+u.Username+" deleted"); err != nil && !errors.Is(err, ErrSpaceNotFound) {
			warn("space_takedown", err)
			continue
		}
		takenDown[sp.ID] = true
		res.SpacesTakenDown = append(res.SpacesTakenDown, DeletedSpace{ID: sp.ID, Name: sp.Name})
	}

	// 6. Owned applications (only with delete_applications): each bot through this same
	// pipeline, then the application.
	for _, app := range blockers.OwnedApplications {
		if !o.deleteApplications {
			break
		}
		if app.BotUserID != 0 {
			if err := s.purgeOwnedBot(ctx, actorID, app.BotUserID, o, res); err != nil {
				warn("application_bot", err)
				continue
			}
		}
		if err := s.del.Apps.AdminDelete(ctx, app.ID); err != nil {
			warn("application", err)
			continue
		}
		s.audit(ctx, actorID, AuditApplicationDelete, TargetApplication, app.ID, app.Name+" (owner "+u.Username+" deleted)")
		res.ApplicationsDeleted = append(res.ApplicationsDeleted, DeletedApplication{ID: app.ID, Name: app.Name, BotUserID: app.BotUserID})
	}

	// 7. A call it is in.
	if s.del.Voice != nil {
		if err := s.del.Voice.Leave(ctx, u.ID); err != nil && !isNotInVoice(err) {
			warn("voice", err)
		}
	}

	// 8. Every space it is still in (the full leave, with relays; see spaces.RemoveAccount).
	for _, sid := range snap.spaceIDs {
		if takenDown[sid] {
			continue
		}
		w, err := s.del.Spaces.RemoveAccount(ctx, u.ID, sid)
		if w != "" {
			res.Warnings = append(res.Warnings, w)
		}
		warn("space_leave", err)
	}

	// 9. Thread memberships.
	if threads, err := s.del.Rooms.ListUserThreadIDs(ctx, u.ID); err != nil {
		warn("threads", err)
	} else {
		for _, tid := range threads {
			warn("thread_leave", s.del.Rooms.RemoveThreadMember(ctx, tid, u.ID))
		}
	}

	// 10. Group PMs: the self-leave (member_left line, SESSION_ROTATE for encrypted groups).
	// A 1:1 PM keeps the account as its other participant on purpose.
	for _, rid := range snap.groupPMs {
		if err := s.del.Groups.RemoveParticipant(ctx, u.ID, rid, u.ID); err != nil &&
			!errors.Is(err, rooms.ErrNotParticipant) && !errors.Is(err, rooms.ErrRoomNotFound) {
			warn("group_leave", err)
		}
	}

	// 11. Everything else that was only the account's.
	s.purgeAccountData(ctx, actorID, u, res, warn)

	// 12. Peers, through the durable outbox: after the relays the steps above queued.
	if s.del.Federation != nil && len(snap.audience) > 0 {
		s.del.Federation.AfterAccountDeleted(ctx, u, snap.audience)
	}

	// 13. Identity (must): users row + username/email claims, one logged batch.
	if err := s.mod.Users.DeleteAccount(ctx, u); err != nil {
		return &DeletionStageError{Stage: "identity", Err: err}
	}
	if err := s.mod.Sessions.DeleteAllForUser(ctx, u.ID); err != nil {
		warn("sessions_delete", err)
	}
	if s.del.Partitions != nil {
		warn("partitions", s.del.Partitions.DeleteUserPartitions(ctx, u.ID))
	}

	// 14. Caches the users repository does not own.
	if rdb := s.mod.Redis; rdb != nil {
		_ = rdb.Del(ctx, "stats:accounts", "stats:spaces").Err()
	}

	// 15. The record: who, what kind, how much - never content.
	action := AuditUserDelete
	if u.Bot {
		action = AuditBotDelete
	}
	reason := u.Username + " (" + kindOf(u) + ") - " + snap.counts.auditLine()
	if len(res.SpacesTakenDown) > 0 {
		reason += ", spaces taken down " + strconv.Itoa(len(res.SpacesTakenDown))
	}
	if len(res.ApplicationsDeleted) > 0 {
		reason += ", applications deleted " + strconv.Itoa(len(res.ApplicationsDeleted))
	}
	if o.auditNote != "" {
		reason += " - " + o.auditNote
	}
	s.audit(ctx, actorID, action, TargetUser, u.ID, reason)

	// A bot deleted on its own takes its application with it.
	if o.deleteBotApp && botApp != nil {
		if err := s.del.Apps.AdminDelete(ctx, botApp.id); err != nil {
			warn("application", err)
		} else {
			s.audit(ctx, actorID, AuditApplicationDelete, TargetApplication, botApp.id, botApp.name+" (its bot "+u.Username+" deleted)")
			res.ApplicationsDeleted = append(res.ApplicationsDeleted, DeletedApplication{ID: botApp.id, Name: botApp.name, BotUserID: u.ID})
		}
	}
	return nil
}

type applicationRef struct {
	id   int64
	name string
}

// purgeOwnedBot runs the pipeline for an owned application's bot (its own lock, its own
// bot_delete audit entry). Its warnings join the owner's.
func (s *Service) purgeOwnedBot(ctx context.Context, actorID, botUserID int64, o purgeOptions, res *DeletionResult) error {
	bot, err := s.mod.Users.GetByID(ctx, botUserID)
	if err != nil {
		return err
	}
	if bot == nil {
		return nil // already gone (a rerun)
	}
	unlock, err := s.lockDeletion(ctx, actorID, bot.ID)
	if err != nil {
		return err
	}
	defer unlock()
	botRes := &DeletionResult{UserID: bot.ID, Username: bot.Username, Kind: DeletionKindBot}
	err = s.purgeAccount(ctx, actorID, bot, purgeOptions{takeDownSpaces: o.takeDownSpaces}, botRes)
	res.Warnings = append(res.Warnings, botRes.Warnings...)
	res.SpacesTakenDown = append(res.SpacesTakenDown, botRes.SpacesTakenDown...)
	return err
}

// purgeAccountData is step 11: what only the account had, module by module.
func (s *Service) purgeAccountData(ctx context.Context, actorID int64, u *auth.User, res *DeletionResult, warn func(string, error)) {
	if _, err := s.del.Relationships.PurgeUser(ctx, u); err != nil {
		warn("relationships", err)
	}
	if _, err := s.del.Devices.PurgeUser(ctx, u.ID); err != nil {
		warn("devices", err)
	}
	if s.mod.TwoFactor != nil {
		warn("two_factor", s.mod.TwoFactor.DeleteAllForUser(ctx, u.ID))
	}
	if _, err := s.del.OAuth.PurgeUser(ctx, u.ID); err != nil {
		warn("oauth", err)
	}
	if _, err := s.del.Push.PurgeUser(ctx, u.ID); err != nil {
		warn("push", err)
	}
	if rdb := s.mod.Redis; rdb != nil {
		prefix := auth.RedisKeyPrefix(s.cfg)
		warn("email_state", auth.PurgeAccountRedisState(ctx, rdb, prefix, u.ID))
		warn("presence", stargate.ForgetUser(ctx, rdb, s.cfg.Database.Redis.CachePrefix, u.ID))
	}
	if !u.DateOfBirth.IsZero() {
		warn("birthday", s.mod.Users.SetBirthdayIndex(ctx, u.ID, int(u.DateOfBirth.Month()), u.DateOfBirth.Day(), false))
	}
	// A ban row keyed by an id that will never sign in again means nothing. An IP ban stays;
	// the result says the ban went, so the administrator can add one.
	if b, err := s.mod.Repo.GetBan(ctx, u.ID); err == nil && b != nil {
		if err := s.mod.Repo.DeleteBan(ctx, u.ID); err != nil {
			warn("ban", err)
		} else {
			res.BanRemoved = true
		}
	}
	s.closeReportsAbout(ctx, actorID, u.ID, warn)
	if s.del.DeleteProfileImages != nil {
		s.del.DeleteProfileImages(ctx, u)
	}
}

// closeReportsAbout resolves every open report about a deleted account (resolution
// account_deleted); the reporters get the usual "handled" notice. Reports the account filed
// stay as they are.
func (s *Service) closeReportsAbout(ctx context.Context, actorID, userID int64, warn func(string, error)) {
	reports, err := s.mod.Repo.ListReportsByTarget(ctx, TargetUser, userID)
	if err != nil {
		warn("reports", err)
		return
	}
	now := time.Now().UTC()
	for _, sum := range reports {
		if sum.Status != ReportOpen {
			continue
		}
		rep, err := s.mod.Repo.GetReport(ctx, sum.ID)
		if err != nil || rep == nil || rep.Status != ReportOpen {
			warn("report", err)
			continue
		}
		from := rep.Status
		rep.Status = ReportResolved
		rep.Resolution = ResolutionAccountDeleted
		rep.ResolutionNote = "account deleted"
		rep.ResolvedBy = &actorID
		rep.ResolvedAt = &now
		if err := s.mod.Repo.MoveReportStatus(ctx, rep, from); err != nil {
			warn("report", err)
			continue
		}
		s.notifyReportResolved(ctx, rep)
	}
}

// publishToUser sends an event to one account's own channel.
func (s *Service) publishToUser(ctx context.Context, userID int64, event string, data map[string]interface{}) {
	if s.del != nil && s.del.Publish != nil {
		s.del.Publish(ctx, userID, event, data)
		return
	}
	if s.mod != nil && s.mod.Redis != nil {
		stargate.PublishToUser(ctx, s.mod.Redis, userID, event, data, s.mod.Region)
	}
}

func isNotInVoice(err error) bool {
	return errors.Is(err, voice.ErrNotInVoice)
}
