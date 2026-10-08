package messages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// Raid protection and light automod for space channels. Both read the space's settings
// through SpaceChannelAuth.SendPolicy and both skip members the moderators have vouched
// for: anyone with Manage Messages or Administrator, and (for the verification level,
// as on Discord) anyone holding a role beyond @everyone. Every check fails open on an
// infrastructure error - a store hiccup must not silence a space - and logs it.

var (
	// ErrVerificationLevel is what a *VerificationError matches with errors.Is.
	ErrVerificationLevel = errors.New("this space requires more from new members before they can send messages")
	// ErrAutomod is what an *AutomodError matches with errors.Is.
	ErrAutomod = errors.New("message blocked by this space's automod")
)

// Verification requirements, in the order the levels add them.
const (
	RequireVerifiedEmail = "email"       // level 1
	RequireAccountAge    = "account_age" // level 2
	RequireMemberAge     = "member_age"  // level 3

	// VerificationAccountAge / VerificationMemberAge are Discord's thresholds.
	VerificationAccountAge = 5 * time.Minute
	VerificationMemberAge  = 10 * time.Minute
)

// Automod rules, named on the wire so the client can say which one fired.
const (
	AutomodRuleRepeated = "repeated"
	AutomodRuleInvite   = "invite"
	AutomodRuleMentions = "mentions"

	// The repeated-message rule: the same text (case- and whitespace-insensitive) this many
	// times within the window is blocked from the threshold on. Short messages are ignored -
	// "lol" three times is chat, not spam.
	repeatThreshold = 3
	repeatWindow    = time.Minute
	repeatMinRunes  = 12
)

// VerificationError says which requirement the sender does not meet yet.
type VerificationError struct {
	Requirement string
	Level       int
}

func (e *VerificationError) Error() string        { return ErrVerificationLevel.Error() }
func (e *VerificationError) Is(target error) bool { return target == ErrVerificationLevel }

// AutomodError names the rule that blocked the message.
type AutomodError struct{ Rule string }

func (e *AutomodError) Error() string        { return ErrAutomod.Error() }
func (e *AutomodError) Is(target error) bool { return target == ErrAutomod }

// Invite links the automod rule recognises: this instance's (and any Strafe instance's)
// /invite/<code>[@origin] URLs, and Discord's. The host part is required so a bare
// "/invite/abc" in prose is not a link.
var (
	strafeInviteRe  = regexp.MustCompile(`(?i)(?:https?://)?[a-z0-9][a-z0-9.-]*\.[a-z]{2,}(?::\d{1,5})?/invite/([a-z0-9]+)((?:@|%40)[a-z0-9.-]+(?:(?::|%3a)\d{1,5})?)?`)
	discordInviteRe = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?(?:discord\.gg|discord(?:app)?\.com/invite)/[a-z0-9-]+`)
)

// sendPolicy is the space's policy for this sender, or nil when nothing applies: not a
// space channel, nothing switched on, or the sender is exempt.
func (s *Service) sendPolicy(ctx context.Context, userID, roomID int64, room *rooms.Room) *spaces.SendPolicy {
	if room.SpaceID == nil || s.spaceAuth == nil || (room.Type != rooms.TypeSpaceText && room.Type != rooms.TypeSpaceVoice && room.Type != rooms.TypeThread) {
		return nil
	}
	p, err := s.spaceAuth.SendPolicy(ctx, *room.SpaceID, userID)
	if err != nil {
		logger.Err("messages", err, map[string]any{"stage": "send_policy", "space_id": *room.SpaceID})
		return nil
	}
	if p == nil || !p.Enforced() {
		return nil
	}
	perms, err := s.spaceAuth.EffectiveChannelPermissions(ctx, userID, *room.SpaceID, roomID)
	if err == nil && (permissions.Has(perms, permissions.PermManageMessages) || permissions.Has(perms, permissions.PermAdministrator)) {
		return nil
	}
	return p
}

// enforceVerificationLevel applies the space's verification level to a member with no
// role. A member of another instance is only held to the member-age rule: their account's
// age and email status live on their home instance, which vouched for them by federating.
func (s *Service) enforceVerificationLevel(ctx context.Context, userID int64, p *spaces.SendPolicy) error {
	if p == nil || p.VerificationLevel <= spaces.VerificationNone || p.HasRole {
		return nil
	}
	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil || u == nil {
		if err != nil {
			logger.Err("messages", err, map[string]any{"stage": "verification_level", "user_id": userID})
		}
		return nil
	}
	now := time.Now()
	if !u.IsRemote() {
		// An instance without email configured cannot have verified addresses; the level's
		// email requirement is then moot rather than a wall nobody can pass.
		emailPossible := s.cfg != nil && s.cfg.Mail.Enabled
		if p.VerificationLevel >= spaces.VerificationLow && emailPossible && !u.VerifiedEmail {
			return &VerificationError{Requirement: RequireVerifiedEmail, Level: p.VerificationLevel}
		}
		if p.VerificationLevel >= spaces.VerificationMedium && now.Sub(u.CreatedAt) < VerificationAccountAge {
			return &VerificationError{Requirement: RequireAccountAge, Level: p.VerificationLevel}
		}
	}
	if p.VerificationLevel >= spaces.VerificationHigh && !p.JoinedAt.IsZero() && now.Sub(p.JoinedAt) < VerificationMemberAge {
		return &VerificationError{Requirement: RequireMemberAge, Level: p.VerificationLevel}
	}
	return nil
}

// enforceAutomod runs the rules the space has on. The mention cap counts distinct user
// and role mentions, parsed or (E2EE) declared; the content rules need the plaintext and
// so only apply where the server can read it.
func (s *Service) enforceAutomod(ctx context.Context, userID int64, room *rooms.Room, p *spaces.SendPolicy, plaintext string, e2eeOff bool, mentionUserIDs, mentionRoleIDs []int64) error {
	if p == nil || p.AutomodFlags == 0 {
		return nil
	}
	if p.AutomodFlags&spaces.AutomodMassMentions != 0 {
		limit := p.AutomodMentionLimit
		if limit <= 0 {
			limit = spaces.DefaultAutomodMentionLimit
		}
		if distinctCount(mentionUserIDs)+distinctCount(mentionRoleIDs) > limit {
			return &AutomodError{Rule: AutomodRuleMentions}
		}
	}
	if !e2eeOff || plaintext == "" {
		return nil
	}
	if p.AutomodFlags&spaces.AutomodInviteLinks != 0 && s.hasForeignInvite(ctx, *room.SpaceID, plaintext) {
		return &AutomodError{Rule: AutomodRuleInvite}
	}
	if p.AutomodFlags&spaces.AutomodRepeatedMessages != 0 && s.repeatedMessage(ctx, userID, *room.SpaceID, plaintext) {
		return &AutomodError{Rule: AutomodRuleRepeated}
	}
	return nil
}

func distinctCount(ids []int64) int {
	seen := make(map[int64]struct{}, len(ids))
	for _, v := range ids {
		seen[v] = struct{}{}
	}
	return len(seen)
}

// hasForeignInvite: the text links to a space other than this one (or to a Discord
// server). An invite of this very space is fine; so is prose that merely looks like one.
func (s *Service) hasForeignInvite(ctx context.Context, spaceID int64, text string) bool {
	if discordInviteRe.MatchString(text) {
		return true
	}
	for _, m := range strafeInviteRe.FindAllStringSubmatch(text, -1) {
		code := m[1]
		if m[2] != "" {
			// code@origin names a space on another instance: never this one's own invite.
			return true
		}
		own, err := s.spaceAuth.InviteBelongsToSpace(ctx, code, spaceID)
		if err != nil {
			logger.Err("messages", err, map[string]any{"stage": "automod_invite", "space_id": spaceID})
			continue
		}
		if !own {
			return true
		}
	}
	return false
}

// repeatedMessage counts identical texts per sender per space in a Redis key that expires
// with the window; the count keeps rising while they keep trying, so a blocked spammer
// stays blocked rather than getting every third message through.
func (s *Service) repeatedMessage(ctx context.Context, userID, spaceID int64, text string) bool {
	if s.redis == nil {
		return false
	}
	norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
	if utf8.RuneCountInString(norm) < repeatMinRunes {
		return false
	}
	sum := sha256.Sum256([]byte(norm))
	h := hex.EncodeToString(sum[:8])
	key := "automod:rep:" + strconv.FormatInt(spaceID, 10) + ":" + strconv.FormatInt(userID, 10)
	count := 1
	if prev, err := s.redis.Get(ctx, key).Result(); err == nil {
		if i := strings.IndexByte(prev, ':'); i > 0 && prev[:i] == h {
			if n, perr := strconv.Atoi(prev[i+1:]); perr == nil {
				count = n + 1
			}
		}
	}
	if err := s.redis.Set(ctx, key, h+":"+strconv.Itoa(count), repeatWindow).Err(); err != nil {
		return false
	}
	return count >= repeatThreshold
}

// verificationMessage is the user-facing reason for a VerificationError.
func verificationMessage(e *VerificationError) string {
	switch e.Requirement {
	case RequireVerifiedEmail:
		return "verify your email address to send messages in this space"
	case RequireAccountAge:
		return "your account must be at least 5 minutes old to send messages in this space"
	case RequireMemberAge:
		return "you must have been a member of this space for 10 minutes to send messages"
	}
	return ErrVerificationLevel.Error()
}

// automodMessage is the user-facing reason for an AutomodError.
func automodMessage(e *AutomodError) string {
	switch e.Rule {
	case AutomodRuleRepeated:
		return "you have sent that message several times already - automod blocked it"
	case AutomodRuleInvite:
		return "invite links to other spaces are not allowed here"
	case AutomodRuleMentions:
		return "that message mentions too many people for this space"
	}
	return ErrAutomod.Error()
}
