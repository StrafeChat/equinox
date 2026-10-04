package messages

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"mime"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/nebula"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	mentionUserRe     = regexp.MustCompile(`<@!?(\d+)>`)
	mentionRoleRe     = regexp.MustCompile(`<@&(\d+)>`)
	mentionEveryoneRe = regexp.MustCompile(`(^|\W)@(everyone|here)(\W|$)`)
)

// parseMentions extracts direct user mentions (<@id>/<@!id>), role mentions (<@&id>), and
// @everyone/@here from plaintext. Only meaningful for non-E2EE rooms - it's the server's
// one chance to read the content at all. @here is folded into the same "everyone" signal
// as @everyone (both gated by PermMentionEveryone, matching Discord's single permission
// bit for both); a separate "online members only" notify set isn't implemented.
func parseMentions(text string) (userIDs, roleIDs []int64, everyone bool) {
	for _, m := range mentionUserRe.FindAllStringSubmatch(text, -1) {
		if uid, err := id.Parse(m[1]); err == nil {
			userIDs = append(userIDs, uid)
		}
	}
	for _, m := range mentionRoleRe.FindAllStringSubmatch(text, -1) {
		if rid, err := id.Parse(m[1]); err == nil {
			roleIDs = append(roleIDs, rid)
		}
	}
	// Word-boundary aware, not a bare substring check: "check my email at bob@everyone.works"
	// (or any text that merely contains the substring) must not silently trigger a
	// permission-gated mass-notify to the whole room.
	everyone = mentionEveryoneRe.MatchString(text)
	return
}

// resolveMentionNotifySet unions direct mentions with an @everyone/@here expansion to every
// participant, deduped, excluding the sender (never notify yourself).
func resolveMentionNotifySet(senderID int64, participants, mentionUserIDs []int64, everyone bool) []int64 {
	seen := make(map[int64]struct{}, len(mentionUserIDs))
	var out []int64
	add := func(uid int64) {
		if uid == senderID {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	for _, uid := range mentionUserIDs {
		add(uid)
	}
	if everyone {
		for _, pid := range participants {
			add(pid)
		}
	}
	return out
}

var (
	ErrRoomNotFound    = errors.New("room not found")
	ErrNotParticipant  = errors.New("not a participant")
	ErrMessageNotFound = errors.New("message not found")
	ErrForbidden       = errors.New("forbidden")
	ErrInvalidInput    = errors.New("invalid message input")
	ErrContentTooLong  = errors.New("message is too long")
	ErrTooManyMentions = errors.New("too many mentions")
	// ErrSlowmode is what a *SlowmodeError matches with errors.Is.
	ErrSlowmode = errors.New("slowmode: wait before sending again")

	ErrUploadsDisabled     = errors.New("file uploads are not configured")
	ErrAttachmentTooLarge  = errors.New("attachment is too large")
	ErrTooManyAttachments  = errors.New("too many attachments")
	ErrAttachmentNotFound  = errors.New("attachment not found or already used")
	ErrAttachmentBadUpload = errors.New("could not store attachment")
)

const (
	// MaxAttachmentsPerMessage caps how many uploads one message may reference.
	MaxAttachmentsPerMessage = 10
	// MaxPlaintextRunes caps a plaintext message body.
	MaxPlaintextRunes = 4000
	// MaxCiphertextBytes caps an E2EE message: a Megolm event carrying a 4000-character
	// body plus ten attachments' metadata and keys fits comfortably; anything bigger is
	// someone using the messages table as blob storage.
	MaxCiphertextBytes = 64 * 1024
	// MaxMentionsPerMessage caps the client-declared mention lists of an E2EE message.
	MaxMentionsPerMessage = 100
)

// SlowmodeError reports how long the sender must wait; errors.Is(err, ErrSlowmode) holds.
type SlowmodeError struct {
	RetryAfter time.Duration
}

func (e *SlowmodeError) Error() string        { return ErrSlowmode.Error() }
func (e *SlowmodeError) Is(target error) bool { return target == ErrSlowmode }

// validateContent enforces the size caps on whichever body a message carries.
func validateContent(plaintext, ciphertext string) error {
	if utf8.RuneCountInString(plaintext) > MaxPlaintextRunes || len(ciphertext) > MaxCiphertextBytes {
		return ErrContentTooLong
	}
	return nil
}

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// safeFilename reduces an uploaded name to something that's a valid, unambiguous object
// key segment and safe to echo back into a URL/Content-Disposition: base name only, a
// conservative character set (spaces become underscores, like Discord), capped length,
// extension preserved.
func safeFilename(name string) string {
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, "\\", "/")))
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._ ")
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	if len(name) > 180 {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:180-len(ext)] + ext
	}
	return name
}

// safeContentType keeps a client-declared type only if it's a plausible media type, else
// derives one from the extension, else falls back to octet-stream.
func safeContentType(declared, filename string) string {
	declared = strings.TrimSpace(strings.ToLower(declared))
	if i := strings.IndexByte(declared, ';'); i >= 0 {
		declared = strings.TrimSpace(declared[:i])
	}
	if declared != "" && declared != "application/octet-stream" && strings.Count(declared, "/") == 1 && len(declared) <= 120 {
		return declared
	}
	if t := mime.TypeByExtension(strings.ToLower(path.Ext(filename))); t != "" {
		if i := strings.IndexByte(t, ';'); i >= 0 {
			t = strings.TrimSpace(t[:i])
		}
		return t
	}
	return "application/octet-stream"
}

// SpaceChannelAuth resolves membership and effective channel permission bits for space text channels.
type SpaceChannelAuth interface {
	IsMember(ctx context.Context, spaceID, userID int64) (bool, error)
	EffectiveChannelPermissions(ctx context.Context, userID, spaceID, roomID int64) (int64, error)
	// ListSpaceMemberUserIDs lists all members for fan-out of rooms_by_user.last_message_id on new messages.
	ListSpaceMemberUserIDs(ctx context.Context, spaceID int64) ([]int64, error)
	// ListSpaceMemberUserIDsByRoles expands a role mention (<@&roleId>) into the member
	// user IDs holding any of the given roles, for the mention-count notify set.
	ListSpaceMemberUserIDsByRoles(ctx context.Context, spaceID int64, roleIDs []int64) ([]int64, error)
}

// Federator relays message events to the other instances whose users share the room.
// Implemented by internal/federation; nil when this instance doesn't federate. The After*
// hooks are best-effort and must not block the request.
//
// The *Remote methods serve a channel of a space another instance hosts (RemoteOrigin
// names it): every write is made there, on behalf of the local user, and applied here
// from the origin's answer; history is read there too, so a member sees the whole
// channel and not just what arrived since they joined. They are synchronous: the
// origin's refusal (an *OriginError) or absence (ErrOriginUnavailable) is the answer.
type Federator interface {
	AfterMessageCreated(ctx context.Context, roomID int64, participants []int64, m *Message)
	AfterMessageEdited(ctx context.Context, roomID int64, participants []int64, m *Message)
	AfterMessageDeleted(ctx context.Context, roomID int64, participants []int64, msgID int64)
	AfterReactionAdded(ctx context.Context, roomID int64, participants []int64, msgID, userID int64, emoji string)
	AfterReactionRemoved(ctx context.Context, roomID int64, participants []int64, msgID, userID int64, emoji string)

	RemoteOrigin(ctx context.Context, room *rooms.Room) string
	CreateRemote(ctx context.Context, origin string, room *rooms.Room, userID int64, in *CreateMessageInput, attachments []Attachment) (*Message, error)
	EditRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64, in *EditMessageInput) (*Message, error)
	DeleteRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64) error
	ReactRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64, emoji string, remove bool) ([]ReactionSummary, error)
	ListRemote(ctx context.Context, origin string, room *rooms.Room, userID int64, beforeID *int64, limit int) ([]Message, map[int64][]ReactionSummary, error)
	GetRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64) (*Message, []ReactionSummary, error)
}

// OriginError is the hosting instance's refusal of a forwarded write, passed through to
// the user with the origin's status and message.
type OriginError struct {
	Status  int
	Message string
}

func (e *OriginError) Error() string { return e.Message }

// ErrOriginUnavailable: the instance hosting the channel's space did not answer.
var ErrOriginUnavailable = errors.New("the instance hosting this space could not be reached")

type Service struct {
	repo      Repository
	rooms     rooms.Repository
	userRepo  auth.UserRepository
	redis     *redis.Client
	cfg       *config.Config
	spaceAuth SpaceChannelAuth
	federator Federator
}

func NewService(repo Repository, roomsRepo rooms.Repository, userRepo auth.UserRepository, redis *redis.Client, cfg *config.Config, spaceAuth SpaceChannelAuth) *Service {
	return &Service{repo: repo, rooms: roomsRepo, userRepo: userRepo, redis: redis, cfg: cfg, spaceAuth: spaceAuth}
}

// SetFederator wires outbound federation for messages.
func (s *Service) SetFederator(f Federator) {
	s.federator = f
}

func (s *Service) region() string {
	if s.cfg == nil || s.cfg.Stargate.Region == "" {
		return "default"
	}
	return s.cfg.Stargate.Region
}

// CreateFederated stores a message relayed by another instance. `m` arrives with a local
// id, sender (a shadow user or 0 for system messages), already-mapped mentions and the
// origin's created_at; this does the same bookkeeping as Create (last message, mention
// counts, gateway event) without the authorization and relay steps.
func (s *Service) CreateFederated(ctx context.Context, roomID int64, participants []int64, m *Message) error {
	if err := validateContent(m.Plaintext, m.Ciphertext); err != nil {
		return err
	}
	if len(m.Mentions) > MaxMentionsPerMessage {
		m.Mentions = m.Mentions[:MaxMentionsPerMessage]
	}
	m.RoomID = roomID
	if err := s.repo.Insert(ctx, m); err != nil {
		return err
	}
	_ = s.rooms.UpdateLastMessageID(ctx, roomID, participants, m.ID)
	if m.SenderID != 0 {
		// Advance the sender's own read cursor like Create does, in case the sender's
		// shadow ever gets a rooms_by_user row read back (harmless otherwise).
		_ = s.rooms.UpdateReadState(ctx, m.SenderID, roomID, m.ID)
	}
	if notify := resolveMentionNotifySet(m.SenderID, participants, m.Mentions, m.MentionEveryone); len(notify) > 0 {
		_ = s.rooms.IncrementMentionCounts(ctx, roomID, notify)
	}
	if s.redis != nil && s.cfg != nil {
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_CREATE", s.messageEventPayloadEnriched(ctx, m), s.region())
	}
	return nil
}

// EditFederated applies an edit relayed by another instance.
func (s *Service) EditFederated(ctx context.Context, roomID, msgID int64, ciphertext, plaintext string) (*Message, error) {
	if ciphertext == "" && plaintext == "" {
		return nil, ErrInvalidInput
	}
	if err := validateContent(plaintext, ciphertext); err != nil {
		return nil, err
	}
	updated, err := s.repo.Update(ctx, roomID, msgID, ciphertext, plaintext)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, ErrMessageNotFound
	}
	if s.redis != nil && s.cfg != nil {
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_UPDATE", s.messageEventPayloadEnriched(ctx, updated), s.region())
	}
	return updated, nil
}

// DeleteFederated applies a delete relayed by another instance. Blobs live on the origin
// instance's CDN, so no attachment cleanup here.
func (s *Service) DeleteFederated(ctx context.Context, roomID, msgID int64) error {
	if err := s.repo.SoftDelete(ctx, roomID, msgID); err != nil {
		return err
	}
	if s.redis != nil && s.cfg != nil {
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_DELETE", map[string]interface{}{
			"room_id":    id.Format(roomID),
			"message_id": id.Format(msgID),
		}, s.region())
	}
	return nil
}

// checkChannelPerms resolves effective channel permissions in a single round trip
// and verifies every bit in need is present. No-op for PMs/group PMs and space
// voice rooms - only space text rooms carry per-channel permission overrides.
func (s *Service) checkChannelPerms(ctx context.Context, userID, roomID int64, room *rooms.Room, need ...int64) error {
	if room.SpaceID == nil || room.Type != rooms.TypeSpaceText || s.spaceAuth == nil || len(need) == 0 {
		return nil
	}
	perms, err := s.spaceAuth.EffectiveChannelPermissions(ctx, userID, *room.SpaceID, roomID)
	if err != nil {
		return err
	}
	for _, n := range need {
		// Implicit rule: if user cannot view the channel, all other room permissions are irrelevant.
		if n != permissions.PermViewRoom && !permissions.Has(perms, permissions.PermViewRoom) {
			return ErrForbidden
		}
		if !permissions.Has(perms, n) {
			return ErrForbidden
		}
	}
	return nil
}

// authorize loads the room once, checks the user can access it (participant, or
// space member for space channels), and checks every permission bit in need -
// all in a single pass. Replaces what used to be 2-4 redundant room fetches and
// up to two full EffectiveChannelPermissions resolutions per request.
func (s *Service) authorize(ctx context.Context, userID, roomID int64, need ...int64) (*rooms.Room, []int64, error) {
	room, err := s.rooms.GetByID(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	if room == nil {
		return nil, nil, ErrRoomNotFound
	}

	var participants []int64
	isSpaceMember := false
	if room.SpaceID != nil && s.spaceAuth != nil {
		isSpaceMember, _ = s.spaceAuth.IsMember(ctx, *room.SpaceID, userID)
	}
	if !isSpaceMember {
		participants, err = s.rooms.GetParticipants(ctx, roomID)
		if err != nil {
			return nil, nil, err
		}
		ok := false
		for _, p := range participants {
			if p == userID {
				ok = true
				break
			}
		}
		if !ok {
			return nil, nil, ErrNotParticipant
		}
	}

	if err := s.checkChannelPerms(ctx, userID, roomID, room, need...); err != nil {
		return nil, nil, err
	}
	return room, participants, nil
}

// roomE2EEOff reports whether a room's content should be stored as plaintext: group PMs
// default to E2EE on (must be explicitly turned off); space text/voice rooms default to
// E2EE off (must be explicitly turned on). Shared by Create and Edit so the two can't
// drift out of sync on what "this room's content is plaintext" means.
func roomE2EEOff(room *rooms.Room) bool {
	return (room.Type == rooms.TypeGroupPM && room.E2EEEnabled != nil && !*room.E2EEEnabled) ||
		((room.Type == rooms.TypeSpaceText || room.Type == rooms.TypeSpaceVoice) && (room.E2EEEnabled == nil || !*room.E2EEEnabled))
}

// enforceSlowmode applies a space text room's slowmode to the sender: one message per
// SlowmodeSeconds, except for members who may manage messages (moderators), matching how
// Discord exempts them. The window is a Redis key that expires on its own.
func (s *Service) enforceSlowmode(ctx context.Context, userID, roomID int64, room *rooms.Room) error {
	if room.SlowmodeSeconds <= 0 || room.SpaceID == nil || room.Type != rooms.TypeSpaceText || s.redis == nil {
		return nil
	}
	if s.spaceAuth != nil {
		if perms, err := s.spaceAuth.EffectiveChannelPermissions(ctx, userID, *room.SpaceID, roomID); err == nil &&
			permissions.Has(perms, permissions.PermManageMessages) {
			return nil
		}
	}
	key := "slowmode:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(roomID, 10)
	window := time.Duration(room.SlowmodeSeconds) * time.Second
	set, err := s.redis.SetNX(ctx, key, "1", window).Result()
	if err != nil {
		// Redis trouble must not block chat; the limit is a courtesy, not a security gate.
		return nil
	}
	if set {
		return nil
	}
	retry, _ := s.redis.TTL(ctx, key).Result()
	if retry <= 0 {
		retry = window
	}
	return &SlowmodeError{RetryAfter: retry}
}

// remoteOrigin names the instance hosting a space channel when it is not this one.
func (s *Service) remoteOrigin(ctx context.Context, room *rooms.Room) string {
	if s.federator == nil || room == nil || room.SpaceID == nil {
		return ""
	}
	return s.federator.RemoteOrigin(ctx, room)
}

// spaceParticipants is the member set a space channel fans out to (space channels have
// no room_participants rows).
func (s *Service) spaceParticipants(ctx context.Context, room *rooms.Room, participants []int64) []int64 {
	if room.SpaceID != nil && len(participants) == 0 && s.spaceAuth != nil {
		if room.Type == rooms.TypeSpaceText || room.Type == rooms.TypeSpaceVoice {
			ids, err := s.spaceAuth.ListSpaceMemberUserIDs(ctx, *room.SpaceID)
			if err == nil && len(ids) > 0 {
				return ids
			}
		}
	}
	return participants
}

func (s *Service) Create(ctx context.Context, userID, roomID int64, in *CreateMessageInput) (*Message, error) {
	return s.create(ctx, userID, roomID, in, nil)
}

// CreateFromRemote stores a message a member on another instance sent into a channel of
// a space this instance hosts: the same checks and bookkeeping as Create, with the
// attachments already resolved (they were uploaded to the sender's own instance, so
// there is nothing here to claim).
func (s *Service) CreateFromRemote(ctx context.Context, userID, roomID int64, in *CreateMessageInput, attachments []Attachment) (*Message, error) {
	if attachments == nil {
		attachments = []Attachment{}
	}
	return s.create(ctx, userID, roomID, in, attachments)
}

// create is Create with the attachments either still to be claimed (nil) or given.
func (s *Service) create(ctx context.Context, userID, roomID int64, in *CreateMessageInput, given []Attachment) (*Message, error) {
	if err := validateContent(in.Plaintext, in.Ciphertext); err != nil {
		return nil, err
	}
	if len(in.Mentions) > MaxMentionsPerMessage || len(in.MentionRoles) > MaxMentionsPerMessage {
		return nil, ErrTooManyMentions
	}
	room, participants, err := s.authorize(ctx, userID, roomID, permissions.PermSendMessages)
	if err != nil {
		return nil, err
	}
	// Attachments are claimed before content validation so an attachment-only message
	// (no text) is valid in a plaintext room; E2EE rooms always carry ciphertext because
	// the attachment metadata itself lives inside it.
	attachments := given
	if attachments == nil {
		attachments, err = s.claimAttachments(ctx, userID, roomID, in.Attachments)
		if err != nil {
			return nil, err
		}
	}
	if origin := s.remoteOrigin(ctx, room); origin != "" {
		// A channel of a space hosted elsewhere: the origin applies the write with its own
		// checks (slowmode, @everyone, bans) and tells every instance, this one included.
		return s.federator.CreateRemote(ctx, origin, room, userID, in, attachments)
	}
	if err := s.enforceSlowmode(ctx, userID, roomID, room); err != nil {
		return nil, err
	}
	participants = s.spaceParticipants(ctx, room, participants)
	e2eeOff := roomE2EEOff(room)
	var ciphertext, plaintext string
	if e2eeOff {
		plaintext = in.Plaintext
		if plaintext == "" && len(attachments) == 0 {
			return nil, ErrInvalidInput
		}
	} else {
		ciphertext = in.Ciphertext
		if ciphertext == "" {
			return nil, ErrInvalidInput
		}
	}

	// Non-E2EE rooms: server can read the content, so it's authoritative over the client
	// on mentions (client-declared values are ignored). E2EE rooms: server never sees
	// plaintext, so the client's declaration is the only signal - trusted as-is.
	var mentionUserIDs, mentionRoleIDs []int64
	var mentionEveryone bool
	if e2eeOff {
		mentionUserIDs, mentionRoleIDs, mentionEveryone = parseMentions(plaintext)
	} else {
		mentionUserIDs = parseIDStrings(in.Mentions)
		mentionRoleIDs = parseIDStrings(in.MentionRoles)
		mentionEveryone = in.MentionEveryone
	}
	// The @everyone permission gate applies regardless of where the flag came from: in an
	// E2EE space room the client's declaration is all the server has, and without this
	// check any member could mass-notify the whole space just by setting the flag.
	if mentionEveryone && room.SpaceID != nil && s.spaceAuth != nil {
		perms, permErr := s.spaceAuth.EffectiveChannelPermissions(ctx, userID, *room.SpaceID, roomID)
		if permErr != nil || !permissions.Has(perms, permissions.PermMentionEveryone) {
			// Sender lacks the permission: the literal "@everyone"/"@here" text is still
			// sent as content, it just doesn't notify anyone - matches Discord.
			mentionEveryone = false
		}
	}

	msgID := id.Next()
	m := &Message{
		RoomID:          roomID,
		ID:              msgID,
		SenderID:        userID,
		SenderDeviceID:  int64(in.SenderDeviceID),
		Ciphertext:      ciphertext,
		Plaintext:       plaintext,
		ReplyToID:       in.ReplyToID.Int64Ptr(),
		Mentions:        mentionUserIDs,
		MentionEveryone: mentionEveryone,
		MentionRoles:    mentionRoleIDs,
	}
	m.SetAttachments(attachments)
	if err := s.repo.Create(ctx, m); err != nil {
		return nil, err
	}
	for _, a := range attachments {
		if aid, perr := id.Parse(a.ID); perr == nil {
			_ = s.repo.SetAttachmentMessage(ctx, roomID, aid, msgID)
		}
	}
	if err := s.rooms.UpdateLastMessageID(ctx, roomID, participants, msgID); err != nil {
		// non-fatal: message is stored
	}
	// Sending also marks the room read for the sender up to their own new message -
	// otherwise the last_message_id bump above makes the room look unread to the
	// sender's own client(s) (and any other device they're signed into) until they
	// re-open it, since nothing else ever advances *their* read cursor past a message
	// they wrote themselves. MESSAGE_ACK is the same event multi-device read-sync
	// already uses, so no separate client-side handling is needed for this.
	if err := s.rooms.UpdateReadState(ctx, userID, roomID, msgID); err == nil {
		// Mirrors rooms.Service.Ack's baseline snapshot - without this, a user who was
		// ever @mentioned (or @everyone'd) in a space channel keeps showing that channel
		// as unread indefinitely, surviving even a reload, because the mention badge is
		// computed server-side as (room_mention_counts total - this baseline) and nothing
		// else ever advances the baseline. UpdateReadState above only clears the
		// (unused) legacy mention_count column, not the real baseline. Harmless no-op
		// for PMs/rooms with zero mention history, since 0 - 0 stays 0.
		if total, mcErr := s.rooms.GetMentionCount(ctx, userID, roomID); mcErr == nil {
			_ = s.rooms.SetMentionCountBaseline(ctx, userID, roomID, int64(total))
		}
		if s.redis != nil && s.cfg != nil {
			stargate.PublishToUser(ctx, s.redis, userID, "MESSAGE_ACK", map[string]interface{}{
				"room_id":              id.Format(roomID),
				"message_id":           id.Format(msgID),
				"last_read_message_id": id.Format(msgID),
			}, s.cfg.Stargate.Region)
		}
	}
	// Role mentions (<@&roleId>) were parsed into mentionRoleIDs above but, until this
	// expansion, were never turned into anyone's mention count - the message rendered the
	// role as a highlighted mention, but nobody holding that role ever got a badge for it.
	notifyUserIDs := mentionUserIDs
	if len(mentionRoleIDs) > 0 && room.SpaceID != nil && s.spaceAuth != nil {
		if fromRoles, err := s.spaceAuth.ListSpaceMemberUserIDsByRoles(ctx, *room.SpaceID, mentionRoleIDs); err == nil {
			notifyUserIDs = append(append([]int64{}, mentionUserIDs...), fromRoles...)
		}
	}
	if notify := resolveMentionNotifySet(userID, participants, notifyUserIDs, mentionEveryone); len(notify) > 0 {
		if err := s.rooms.IncrementMentionCounts(ctx, roomID, notify); err != nil {
			// non-fatal: message is stored, mention badges just won't reflect this one
		}
	}
	if s.redis != nil && s.cfg != nil {
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_CREATE", s.messageEventPayloadEnriched(ctx, m), s.cfg.Stargate.Region)
	}
	if s.federator != nil {
		s.federator.AfterMessageCreated(ctx, roomID, participants, m)
	}
	return m, nil
}

// Get returns one message with its reactions as userID sees them. In a channel hosted
// elsewhere a message this instance never stored is fetched from the origin.
func (s *Service) Get(ctx context.Context, userID, roomID, msgID int64) (*Message, []ReactionSummary, error) {
	room, _, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory)
	if err != nil {
		return nil, nil, err
	}
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil {
		return nil, nil, err
	}
	if msg == nil || (msg.DeletedAt != nil && !msg.DeletedAt.IsZero()) {
		if origin := s.remoteOrigin(ctx, room); origin != "" {
			return s.federator.GetRemote(ctx, origin, room, userID, msgID)
		}
		return nil, nil, ErrMessageNotFound
	}
	reactions, err := s.Reactions(ctx, userID, roomID, msgID)
	if err != nil {
		return nil, nil, err
	}
	return msg, reactions, nil
}

func (s *Service) List(ctx context.Context, userID, roomID int64, beforeID *int64, limit int) ([]Message, error) {
	msgs, _, err := s.ListWithReactions(ctx, userID, roomID, beforeID, limit)
	return msgs, err
}

// ListWithReactions is a page of history with each message's reaction summary as
// userID sees it. A channel of a space hosted elsewhere is read from the origin, which
// has all of it (this instance only holds what was relayed since a local member
// joined); if the origin cannot be reached, that local copy is served instead.
func (s *Service) ListWithReactions(ctx context.Context, userID, roomID int64, beforeID *int64, limit int) ([]Message, map[int64][]ReactionSummary, error) {
	room, _, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory)
	if err != nil {
		return nil, nil, err
	}
	if origin := s.remoteOrigin(ctx, room); origin != "" {
		msgs, reactions, rerr := s.federator.ListRemote(ctx, origin, room, userID, beforeID, limit)
		if rerr == nil {
			return msgs, reactions, nil
		}
		logger.Warn("messages", "history for room %d from %s: %v (serving the local copy)", roomID, origin, rerr)
	}
	msgs, err := s.repo.List(ctx, roomID, beforeID, limit)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].ID
	}
	reactions, err := s.ReactionsForMessages(ctx, roomID, ids, userID)
	if err != nil {
		// Non-fatal: history is still useful without reaction counts.
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		reactions = nil
	}
	return msgs, reactions, nil
}

// reactionsFor loads each message's reaction summary as userID sees it; a failure is
// non-fatal (history is still useful without counts), so it returns nil rather than erroring.
func (s *Service) reactionsFor(ctx context.Context, roomID int64, msgs []Message, userID int64) map[int64][]ReactionSummary {
	ids := make([]int64, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].ID
	}
	reactions, err := s.ReactionsForMessages(ctx, roomID, ids, userID)
	if err != nil {
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		return nil
	}
	return reactions
}

// ListAfterWithReactions is a page of history immediately AFTER afterID, oldest-first - for
// resuming downward infinite scroll out of a jumped-to window. Served from the local store (a
// mirrored room's relayed copy), so there's no origin round-trip on every scroll step.
func (s *Service) ListAfterWithReactions(ctx context.Context, userID, roomID, afterID int64, limit int) ([]Message, map[int64][]ReactionSummary, error) {
	if _, _, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory); err != nil {
		return nil, nil, err
	}
	msgs, err := s.repo.ListAfter(ctx, roomID, afterID, limit)
	if err != nil {
		return nil, nil, err
	}
	return msgs, s.reactionsFor(ctx, roomID, msgs, userID), nil
}

// ListAroundWithReactions returns a window of history centred on aroundID - about half the limit
// older, the message itself, then half newer, oldest-first - so the client can jump to a message
// that isn't loaded (a reply target, a search hit) and read outward in both directions. Served
// from the local store like ListAfterWithReactions. A deleted/absent target just yields the
// messages that surround where it was.
func (s *Service) ListAroundWithReactions(ctx context.Context, userID, roomID, aroundID int64, limit int) ([]Message, map[int64][]ReactionSummary, error) {
	if _, _, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory); err != nil {
		return nil, nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	half := limit / 2
	older, err := s.repo.List(ctx, roomID, &aroundID, half) // newest-first, closest to target first
	if err != nil {
		return nil, nil, err
	}
	newer, err := s.repo.ListAfter(ctx, roomID, aroundID, half) // oldest-first
	if err != nil {
		return nil, nil, err
	}
	combined := make([]Message, 0, len(older)+len(newer)+1)
	for i := len(older) - 1; i >= 0; i-- { // reverse the older half into ascending order
		combined = append(combined, older[i])
	}
	if target, terr := s.repo.GetByID(ctx, roomID, aroundID); terr == nil && target != nil && (target.DeletedAt == nil || target.DeletedAt.IsZero()) {
		combined = append(combined, *target)
	}
	combined = append(combined, newer...)
	return combined, s.reactionsFor(ctx, roomID, combined, userID), nil
}

func (s *Service) Edit(ctx context.Context, userID, roomID, msgID int64, in *EditMessageInput) (*Message, error) {
	if err := validateContent(in.Plaintext, in.Ciphertext); err != nil {
		return nil, err
	}
	room, participants, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom)
	if err != nil {
		return nil, err
	}
	if origin := s.remoteOrigin(ctx, room); origin != "" {
		return s.federator.EditRemote(ctx, origin, room, userID, msgID, in)
	}
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil || msg == nil {
		return nil, ErrMessageNotFound
	}
	if msg.SenderID != userID {
		return nil, ErrForbidden
	}
	if msg.DeletedAt != nil && !msg.DeletedAt.IsZero() {
		return nil, ErrMessageNotFound
	}
	// Write to whichever column this room actually uses for content - previously this
	// always wrote `ciphertext` regardless of the room's E2EE setting, so editing a
	// message was effectively broken in any plaintext (non-E2EE) room.
	var ciphertext, plaintext string
	if roomE2EEOff(room) {
		plaintext = in.Plaintext
	} else {
		ciphertext = in.Ciphertext
	}
	// The column this room uses must actually be filled: a client that sent only the
	// other one would otherwise blank the message out.
	if ciphertext == "" && plaintext == "" {
		return nil, ErrInvalidInput
	}
	updated, err := s.repo.Update(ctx, roomID, msgID, ciphertext, plaintext)
	if err != nil {
		return nil, err
	}
	if s.redis != nil && s.cfg != nil && updated != nil {
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_UPDATE", s.messageEventPayloadEnriched(ctx, updated), s.cfg.Stargate.Region)
	}
	if s.federator != nil && updated != nil {
		s.federator.AfterMessageEdited(ctx, roomID, participants, updated)
	}
	return updated, nil
}

func (s *Service) Delete(ctx context.Context, userID, roomID, msgID int64) error {
	room, participants, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom)
	if err != nil {
		return err
	}
	if origin := s.remoteOrigin(ctx, room); origin != "" {
		return s.federator.DeleteRemote(ctx, origin, room, userID, msgID)
	}
	msg, err := s.repo.GetByID(ctx, roomID, msgID)
	if err != nil || msg == nil {
		return ErrMessageNotFound
	}
	if msg.SenderID != userID {
		if room.SpaceID != nil && room.Type == rooms.TypeSpaceText {
			if err := s.checkChannelPerms(ctx, userID, roomID, room, permissions.PermManageMessages); err != nil {
				return err
			}
		} else {
			return ErrForbidden
		}
	}
	if err := s.repo.SoftDelete(ctx, roomID, msgID); err != nil {
		return err
	}
	// Deleting the room's newest message leaves last_message_id pointing at a row that no
	// longer exists. The per-user read cursor can never advance to it (there's nothing left to
	// read up to), so the room shows a permanent unread that only clears while you're actually
	// inside it. Advance last_message_id to the newest surviving message - or clear it if that
	// was the last message. Best-effort: a failure here must not fail the delete itself.
	// `lastChanged` records that this delete moved the room's newest message, so the event
	// below can carry the new last_message_id and clients heal their read cursor without a
	// reload (otherwise their cached last_message_id keeps a phantom unread alive).
	lastChanged := false
	var newLast *int64
	if room.LastMessageID != nil && *room.LastMessageID == msgID {
		if prev, perr := s.repo.List(ctx, roomID, &msgID, 1); perr == nil {
			lastChanged = true
			if len(prev) > 0 {
				newLast = &prev[0].ID
				_ = s.rooms.UpdateLastMessageID(ctx, roomID, participants, prev[0].ID)
			} else {
				_ = s.rooms.ClearLastMessageID(ctx, roomID, participants)
			}
		}
	}
	s.cleanupAttachments(ctx, roomID, msg.Attachments())
	if s.redis != nil && s.cfg != nil {
		region := s.cfg.Stargate.Region
		if region == "" {
			region = "default"
		}
		payload := map[string]interface{}{
			"room_id":    id.Format(roomID),
			"message_id": id.Format(msgID),
		}
		if lastChanged {
			if newLast != nil {
				payload["last_message_id"] = id.Format(*newLast)
			} else {
				payload["last_message_id"] = nil
			}
		}
		stargate.PublishToSpace(ctx, s.redis, roomID, "MESSAGE_DELETE", payload, s.cfg.Stargate.Region)
	}
	if s.federator != nil {
		s.federator.AfterMessageDeleted(ctx, roomID, participants, msgID)
	}
	return nil
}

// UploadsConfigured reports whether Nebula is set up for attachment storage.
func (s *Service) UploadsConfigured() bool {
	return s.cfg != nil && strings.TrimSpace(s.cfg.Nebula.BaseURL) != "" && strings.TrimSpace(s.cfg.Nebula.UploadSecret) != ""
}

// MaxAttachmentBytes is the per-file cap (for client-facing error messages).
func (s *Service) MaxAttachmentBytes() int64 {
	if s.cfg == nil || s.cfg.Nebula.AttachmentMaxBytes <= 0 {
		return 25 * 1024 * 1024
	}
	return int64(s.cfg.Nebula.AttachmentMaxBytes)
}

// UploadAttachment stores one file on Nebula for later use by a message from the same
// user in the same room. Authorization is the same as sending a message. The registry
// row is what Create later validates against, so nobody can attach someone else's
// upload (or one from another room) to their message.
func (s *Service) UploadAttachment(ctx context.Context, userID, roomID int64, in *UploadAttachmentInput) (*Attachment, error) {
	if !s.UploadsConfigured() {
		return nil, ErrUploadsDisabled
	}
	if in == nil || in.Body == nil || in.Size <= 0 {
		return nil, ErrInvalidInput
	}
	if in.Size > s.MaxAttachmentBytes() {
		return nil, ErrAttachmentTooLarge
	}
	if _, _, err := s.authorize(ctx, userID, roomID, permissions.PermSendMessages); err != nil {
		return nil, err
	}

	attID := id.Next()
	row := &AttachmentRow{
		RoomID:     roomID,
		ID:         attID,
		UploaderID: userID,
		Size:       in.Size,
		Encrypted:  in.Encrypted,
		CreatedAt:  time.Now().UTC(),
	}
	// Snowflakes are close to sequential, so a key built from ids alone could be guessed by
	// anyone who knows the room id (a former member, say) and walked to enumerate every
	// file ever posted. The random segment makes the CDN URL unguessable - the only
	// access control Nebula has, since it serves objects to anyone holding the URL.
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	dir := "attachments/" + id.Format(roomID) + "/" + id.Format(attID) + "-" + hex.EncodeToString(nonce[:])
	var key, contentType string
	if in.Encrypted {
		// Opaque blob: the real name/type/dimensions are inside the message ciphertext.
		key = dir + "/blob"
		contentType = "application/octet-stream"
	} else {
		row.Filename = safeFilename(in.Filename)
		row.ContentType = safeContentType(in.ContentType, row.Filename)
		if in.Width > 0 && in.Height > 0 && in.Width <= 20000 && in.Height <= 20000 {
			row.Width, row.Height = in.Width, in.Height
		}
		key = dir + "/" + row.Filename
		contentType = row.ContentType
	}
	if err := nebula.Put(ctx, s.cfg.Nebula.BaseURL, s.cfg.Nebula.UploadSecret, key, in.Body, contentType, in.Size); err != nil {
		logger.Err("messages", err, map[string]any{"room_id": roomID, "key": key})
		return nil, ErrAttachmentBadUpload
	}
	row.URL = nebula.ObjectURL(s.cfg.Nebula.BaseURL, s.cfg.Nebula.PublicURL, key)
	if err := s.repo.CreateAttachment(ctx, row); err != nil {
		_ = nebula.Delete(ctx, s.cfg.Nebula.BaseURL, s.cfg.Nebula.UploadSecret, key)
		return nil, err
	}
	a := row.ToAttachment()
	return &a, nil
}

// claimAttachments validates that every referenced upload belongs to this sender in this
// room and hasn't been used by another message yet, returning their wire forms.
func (s *Service) claimAttachments(ctx context.Context, userID, roomID int64, ids []string) ([]Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxAttachmentsPerMessage {
		return nil, ErrTooManyAttachments
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]Attachment, 0, len(ids))
	for _, raw := range ids {
		aid, err := id.Parse(raw)
		if err != nil {
			return nil, ErrAttachmentNotFound
		}
		if _, dup := seen[aid]; dup {
			continue
		}
		seen[aid] = struct{}{}
		row, err := s.repo.GetAttachment(ctx, roomID, aid)
		if err != nil {
			return nil, err
		}
		if row == nil || row.UploaderID != userID || row.MessageID != nil {
			return nil, ErrAttachmentNotFound
		}
		out = append(out, row.ToAttachment())
	}
	return out, nil
}

// cleanupAttachments removes a deleted message's blobs and registry rows. Best-effort:
// the message is already gone from the room; a leftover blob is a storage leak, not a
// correctness problem, and gets logged.
func (s *Service) cleanupAttachments(ctx context.Context, roomID int64, list []Attachment) {
	if len(list) == 0 {
		return
	}
	for _, a := range list {
		if aid, err := id.Parse(a.ID); err == nil {
			_ = s.repo.DeleteAttachment(ctx, roomID, aid)
		}
		if !s.UploadsConfigured() || !s.ownsAttachment(a.URL) {
			continue
		}
		if key := nebula.KeyFromURL(a.URL); key != "" {
			if err := nebula.Delete(ctx, s.cfg.Nebula.BaseURL, s.cfg.Nebula.UploadSecret, key); err != nil {
				logger.Err("messages", err, map[string]any{"room_id": roomID, "attachment": a.ID})
			}
		}
	}
}

// ownsAttachment reports whether an attachment URL points at this instance's own CDN.
// A member of a space hosted here may have uploaded to their own instance; that blob is
// theirs to clean up, and a key derived from its URL would name nothing of ours.
func (s *Service) ownsAttachment(u string) bool {
	if s.cfg == nil {
		return false
	}
	for _, base := range []string{s.cfg.Nebula.PublicURL, s.cfg.Nebula.BaseURL} {
		base = strings.TrimRight(strings.TrimSpace(base), "/")
		if base != "" && strings.HasPrefix(u, base+"/") {
			return true
		}
	}
	return false
}

func messageEventPayload(m *Message) map[string]interface{} {
	out := map[string]interface{}{
		"room_id":          id.Format(m.RoomID),
		"id":               id.Format(m.ID),
		"sender_id":        id.Format(m.SenderID),
		"sender_device_id": id.Format(m.SenderDeviceID),
		"ciphertext":       m.Ciphertext,
		"created_at":       m.CreatedAt,
		"updated_at":       m.UpdatedAt,
	}
	if m.Plaintext != "" {
		out["plaintext"] = m.Plaintext
	}
	if m.ReplyToID != nil {
		out["reply_to_id"] = id.Format(*m.ReplyToID)
	}
	if len(m.Mentions) > 0 {
		out["mentions"] = formatIDs(m.Mentions)
	}
	if m.MentionEveryone {
		out["mention_everyone"] = true
	}
	if len(m.MentionRoles) > 0 {
		out["mention_roles"] = formatIDs(m.MentionRoles)
	}
	if atts := m.Attachments(); len(atts) > 0 {
		out["attachments"] = atts
	}
	if m.DeletedAt != nil && !m.DeletedAt.IsZero() {
		out["deleted_at"] = m.DeletedAt
	}
	if m.SystemType != "" {
		out["system_type"] = m.SystemType
		out["system_payload"] = m.SystemPayload
	}
	return out
}

func formatIDs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, v := range ids {
		out[i] = id.Format(v)
	}
	return out
}

// parseIDStrings parses snowflake strings, silently dropping any that don't parse -
// matches parseMentions' own leniency for malformed ids in message content.
func parseIDStrings(ss []string) []int64 {
	var out []int64
	for _, s := range ss {
		if v, err := id.Parse(s); err == nil {
			out = append(out, v)
		}
	}
	return out
}
