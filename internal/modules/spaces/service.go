package spaces

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	ErrSpaceNotFound               = errors.New("space not found")
	ErrNotMember                   = errors.New("not a member of this space")
	ErrNotSpaceOwner               = errors.New("only the space owner can do this")
	ErrInviteNotFound              = errors.New("invite not found")
	ErrInsufficientSpacePermission = errors.New("insufficient permissions for this space")
	ErrInvalidSpaceName            = errors.New("space name must be 1-100 characters")
	ErrNothingToPatch              = errors.New("no fields to update")
	ErrBanned                      = errors.New("banned from this space")
	ErrCannotModerateOwner         = errors.New("cannot kick or ban the space owner")
	ErrOwnerCannotLeave            = errors.New("space owner must transfer ownership or delete the space before leaving")
	ErrInvalidDescription          = errors.New("description must be at most 1000 characters")
)

const (
	MaxSpaceNameRunes        = 100
	MaxSpaceDescriptionRunes = 1000
	MaxBanReasonRunes        = 512
)

type Service struct {
	repo     Repository
	roomRepo rooms.Repository
	userRepo auth.UserRepository
	redis    *redis.Client
	cfg      *config.Config
	// systemMessenger is nil until routes wire one in (see SetSystemMessenger).
	systemMessenger SystemMessenger
}

func NewService(repo Repository, roomRepo rooms.Repository, userRepo auth.UserRepository, redis *redis.Client, cfg *config.Config) *Service {
	return &Service{repo: repo, roomRepo: roomRepo, userRepo: userRepo, redis: redis, cfg: cfg}
}

func (s *Service) stargateRegion() string {
	if s.cfg == nil || s.cfg.Stargate.Region == "" {
		return "default"
	}
	return s.cfg.Stargate.Region
}

// nameAcronym returns up to 4 uppercase letters from the first letters of words in name.
func nameAcronym(name string) string {
	words := strings.Fields(strings.TrimSpace(name))
	if len(words) == 0 {
		return "SP"
	}
	var b strings.Builder
	for _, w := range words {
		if b.Len() >= 4 {
			break
		}
		for _, r := range w {
			if unicode.IsLetter(r) {
				b.WriteRune(unicode.ToUpper(r))
				break
			}
		}
	}
	if b.Len() == 0 {
		return "SP"
	}
	return b.String()
}

// CreateSpace creates a new space owned by actorID and publishes SPACE_CREATE to the owner.
func (s *Service) CreateSpace(ctx context.Context, actorID int64, in *CreateSpaceInput) (*Space, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "Unnamed Space"
	}
	if utf8.RuneCountInString(name) > MaxSpaceNameRunes {
		return nil, ErrInvalidSpaceName
	}
	description := strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(description) > MaxSpaceDescriptionRunes {
		return nil, ErrInvalidDescription
	}
	spaceID := id.Next()
	now := time.Now().UTC()
	space := &Space{
		ID:          spaceID,
		Name:        name,
		NameAcronym: nameAcronym(name),
		Description: description,
		// Icons are set through POST /spaces/:id/icon, which stores the image on this
		// instance's CDN. A client-supplied URL would be loaded by every member.
		Icon:                  "",
		OwnerID:               actorID,
		VerificationLevel:     0,
		DefaultMessageNotif:   0,
		ExplicitContentFilter: 0,
		Features:              nil,
		MaxPresences:          0,
		MaxMembers:            0,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := s.repo.Create(ctx, space); err != nil {
		return nil, err
	}
	everyoneRID := id.Next()
	roleNow := time.Now().UTC()
	everyoneRole := &SpaceRole{
		SpaceID:     spaceID,
		ID:          everyoneRID,
		Name:        EveryoneRoleName,
		Permissions: permissions.DefaultEveryone,
		Position:    0,
		CreatedAt:   roleNow,
		UpdatedAt:   roleNow,
	}
	if err := s.repo.InsertSpaceRole(ctx, everyoneRole); err != nil {
		return nil, err
	}
	if err := s.repo.UpdateEveryoneRoleID(ctx, spaceID, everyoneRID); err != nil {
		return nil, err
	}
	if err := s.repo.AddMember(ctx, spaceID, actorID, []int64{everyoneRID}); err != nil {
		return nil, err
	}
	if err := s.repo.AddSpaceToUserSet(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	if err := s.createDefaultSpaceRooms(ctx, spaceID); err != nil {
		return nil, err
	}
	if s.redis != nil {
		payload := spaceToEventPayload(space)
		stargate.PublishToUser(ctx, s.redis, actorID, "SPACE_CREATE", payload, s.stargateRegion())
	}
	return space, nil
}

// ListMembers returns members of a space with basic user info. Caller must be a member.
func (s *Service) ListMembers(ctx context.Context, actorID, spaceID int64) ([]struct {
	Member SpaceMember
	User   *auth.User
}, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	rows, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	userIDs := make([]int64, 0, len(rows))
	for _, m := range rows {
		userIDs = append(userIDs, m.UserID)
	}
	users, err := s.userRepo.GetByIDs(ctx, userIDs)
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		Member SpaceMember
		User   *auth.User
	}, 0, len(rows))
	for i, m := range rows {
		var u *auth.User
		if i < len(users) {
			u = users[i]
		}
		if u == nil {
			continue
		}
		out = append(out, struct {
			Member SpaceMember
			User   *auth.User
		}{
			Member: m,
			User:   u,
		})
	}
	return out, nil
}

// createDefaultSpaceRooms creates "Text Rooms" section with General (text), and "Voice Rooms" section with General (voice).
func (s *Service) createDefaultSpaceRooms(ctx context.Context, spaceID int64) error {
	space, _ := s.repo.GetByID(ctx, spaceID)
	ownerID := int64(0)
	if space != nil {
		ownerID = space.OwnerID
	}
	textSectionID := id.Next()
	generalTextID := id.Next()
	voiceSectionID := id.Next()
	generalVoiceID := id.Next()

	textSection := &rooms.Room{
		ID:        textSectionID,
		Type:      rooms.TypeRoomSection,
		SpaceID:   &spaceID,
		Name:      "Text Rooms",
		Position:  0,
		CreatorID: ownerID,
	}
	if err := s.roomRepo.CreateSpaceRoom(ctx, textSection); err != nil {
		return err
	}
	e2eeOff := false // E2EE off by default for space channels (scale; future: space-scale E2EE protocol)
	generalText := &rooms.Room{
		ID:          generalTextID,
		Type:        rooms.TypeSpaceText,
		SpaceID:     &spaceID,
		ParentID:    &textSectionID,
		Name:        "General",
		Position:    1,
		CreatorID:   ownerID,
		E2EEEnabled: &e2eeOff,
	}
	if err := s.roomRepo.CreateSpaceRoom(ctx, generalText); err != nil {
		return err
	}
	voiceSection := &rooms.Room{
		ID:        voiceSectionID,
		Type:      rooms.TypeRoomSection,
		SpaceID:   &spaceID,
		Name:      "Voice Rooms",
		Position:  2,
		CreatorID: ownerID,
	}
	if err := s.roomRepo.CreateSpaceRoom(ctx, voiceSection); err != nil {
		return err
	}
	generalVoice := &rooms.Room{
		ID:          generalVoiceID,
		Type:        rooms.TypeSpaceVoice,
		SpaceID:     &spaceID,
		ParentID:    &voiceSectionID,
		Name:        "General",
		Position:    3,
		CreatorID:   ownerID,
		E2EEEnabled: &e2eeOff,
	}
	return s.roomRepo.CreateSpaceRoom(ctx, generalVoice)
}

// GetSpace returns a space by ID if it exists.
func (s *Service) GetSpace(ctx context.Context, id int64) (*Space, error) {
	return s.repo.GetByID(ctx, id)
}

// GetSpaceForUser returns a space by ID if it exists and the user is a member.
func (s *Service) GetSpaceForUser(ctx context.Context, userID, spaceID int64) (*Space, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	space, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if space == nil {
		return nil, ErrSpaceNotFound
	}
	return space, nil
}

// ListSpacesForUser returns all spaces the user is a member of (via spaces_by_user).
func (s *Service) ListSpacesForUser(ctx context.Context, userID int64) ([]SpaceMemberRow, error) {
	return s.repo.ListSpacesByUser(ctx, userID)
}

// ListSpaceMemberUserIDs returns every member's user ID in a space (for rooms_by_user fan-out on new messages).
func (s *Service) ListSpaceMemberUserIDs(ctx context.Context, spaceID int64) ([]int64, error) {
	ms, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(ms))
	for i := range ms {
		out[i] = ms[i].UserID
	}
	return out, nil
}

// ListSpaceMemberUserIDsByRoles returns every member holding at least one of the given
// role IDs, deduped - for expanding a role mention (<@&roleId>) into a mention-count
// notify set. Two roles are deliberately excluded even if present in roleIDs:
//   - the space's own @everyone role, since every member always holds it - expanding it
//     here would silently bypass the same PermMentionEveryone gate the literal
//     "@everyone"/"@here" text path already enforces;
//   - any role with Mentionable == false, so an admin's "don't let this role be pinged"
//     setting can't be worked around by hand-typing (or an old message containing) the
//     raw <@&roleId> syntax.
//
// A message that mentions an excluded role still renders it (Mentions/MentionRoles are
// stored as parsed either way) - it just isn't treated as a notify-worthy mention.
func (s *Service) ListSpaceMemberUserIDsByRoles(ctx context.Context, spaceID int64, roleIDs []int64) ([]int64, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	snap, err := s.permissionSnapshot(ctx, spaceID, 0)
	if err != nil {
		return nil, err
	}
	mentionable := make(map[int64]struct{}, len(snap.Roles))
	for _, r := range snap.Roles {
		if r.Mentionable {
			mentionable[r.ID] = struct{}{}
		}
	}
	want := make(map[int64]struct{}, len(roleIDs))
	for _, rid := range roleIDs {
		if rid == 0 || rid == snap.EveryoneRoleID {
			continue
		}
		// Respects each role's own "mentionable" setting - a role an admin deliberately
		// marked as not mentionable shouldn't start notifying its holders just because
		// someone hand-typed (or an old message contains) its raw <@&roleId> syntax.
		if _, ok := mentionable[rid]; ok {
			want[rid] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	members, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]struct{}, len(members))
	out := make([]int64, 0, len(members))
	for _, m := range members {
		for _, rid := range m.RoleIDs {
			if _, ok := want[rid]; !ok {
				continue
			}
			if _, dup := seen[m.UserID]; !dup {
				seen[m.UserID] = struct{}{}
				out = append(out, m.UserID)
			}
			break
		}
	}
	return out, nil
}

// ListSpaceRooms returns rooms in the space (text and voice). User must be a member.
func (s *Service) ListSpaceRooms(ctx context.Context, userID, spaceID int64) ([]*rooms.Room, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	return s.listRooms(ctx, spaceID)
}

// listRooms loads every room in a space in position order: the rooms_by_space index and
// then one batched read of the room rows, instead of one read per room.
func (s *Service) listRooms(ctx context.Context, spaceID int64) ([]*rooms.Room, error) {
	rows, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RoomID)
	}
	list, err := s.roomRepo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*rooms.Room, 0, len(list))
	for _, room := range list {
		if room != nil && room.SpaceID != nil && *room.SpaceID == spaceID {
			out = append(out, room)
		}
	}
	return out, nil
}

// ListSpaceRoomsWithOverrides is ListSpaceRooms plus every room's permission overrides,
// so a client can compute channel permissions locally (the way a Discord client does
// from the permission_overwrites on each channel) instead of fetching them per visit.
func (s *Service) ListSpaceRoomsWithOverrides(ctx context.Context, userID, spaceID int64) ([]*rooms.Room, *Snapshot, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, userID)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, ErrNotMember
	}
	return s.SpaceRoomsWithOverrides(ctx, spaceID)
}

// SpaceRoomsWithOverrides is the unchecked form for callers that already know the user
// is a member (the gateway's READY builder walks spaces_by_user).
func (s *Service) SpaceRoomsWithOverrides(ctx context.Context, spaceID int64) ([]*rooms.Room, *Snapshot, error) {
	list, err := s.listRooms(ctx, spaceID)
	if err != nil {
		return nil, nil, err
	}
	snap, err := s.Snapshot(ctx, spaceID)
	if err != nil {
		return nil, nil, err
	}
	return list, snap, nil
}

// GetSpaces batch-loads spaces (nil entries for ids that do not exist).
func (s *Service) GetSpaces(ctx context.Context, ids []int64) ([]*Space, error) {
	return s.repo.GetByIDs(ctx, ids)
}

// markPreJoinHistoryRead marks a newly-joined member's read cursor caught up to each
// room's current last message, so pre-join history isn't shown as unread (matches
// Discord: joining a server doesn't dump its whole history into your unread badge).
// Best-effort: a failure here just leaves one room's unread state wrong until the
// member's next visit or the room's next message - not worth failing the join over.
func (s *Service) markPreJoinHistoryRead(ctx context.Context, spaceID, userID int64) {
	list, err := s.listRooms(ctx, spaceID)
	if err != nil {
		return
	}
	for _, room := range list {
		if room.LastMessageID == nil {
			continue
		}
		if room.Type != rooms.TypeSpaceText && room.Type != rooms.TypeSpaceVoice {
			continue
		}
		_ = s.roomRepo.UpdateReadState(ctx, userID, room.ID, *room.LastMessageID)
		// Also snapshot the mention baseline (mirrors AckAll): without this, a member who
		// left while holding an unread mention and then rejoins (or was kicked and
		// re-invited) keeps that stale positive count forever - room_mention_counts is
		// never decremented (see SetMentionCountBaseline's own doc comment), so nothing
		// else would ever clear it for them.
		if total, err := s.roomRepo.GetMentionCount(ctx, userID, room.ID); err == nil {
			_ = s.roomRepo.SetMentionCountBaseline(ctx, userID, room.ID, int64(total))
		}
	}
}

// GetUserRoomRow exposes the rooms_by_user row for a room, for callers (GetRooms) that
// need to merge per-user read-state into an otherwise room-only response.
func (s *Service) GetUserRoomRow(ctx context.Context, userID, roomID int64) (*rooms.RoomRow, error) {
	return s.roomRepo.GetRoomRow(ctx, userID, roomID)
}

// UserRoomRows returns every rooms_by_user row of a user keyed by room id - one partition
// read that replaces a GetUserRoomRow per room when building a room list.
func (s *Service) UserRoomRows(ctx context.Context, userID int64) (map[int64]*rooms.RoomRow, error) {
	rows, err := s.roomRepo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*rooms.RoomRow, len(rows))
	for i := range rows {
		out[rows[i].RoomID] = &rows[i]
	}
	return out, nil
}

// GetMentionCounts returns every room's mention count for a user (one query).
func (s *Service) GetMentionCounts(ctx context.Context, userID int64) (map[int64]int, error) {
	return s.roomRepo.GetMentionCounts(ctx, userID)
}

// AckAll marks every text/voice room in a space read up to its current last message, for
// the caller - Discord's per-server "Mark As Read". Best-effort per room: one room
// failing to ack doesn't roll back the others.
func (s *Service) AckAll(ctx context.Context, actorID, spaceID int64) error {
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	list, err := s.listRooms(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, room := range list {
		if room.LastMessageID == nil {
			continue
		}
		if room.Type != rooms.TypeSpaceText && room.Type != rooms.TypeSpaceVoice {
			continue
		}
		_ = s.roomRepo.UpdateReadState(ctx, actorID, room.ID, *room.LastMessageID)
		if total, err := s.roomRepo.GetMentionCount(ctx, actorID, room.ID); err == nil {
			_ = s.roomRepo.SetMentionCountBaseline(ctx, actorID, room.ID, int64(total))
		}
		if s.redis != nil && s.cfg != nil {
			payload := map[string]interface{}{
				"room_id":              id.Format(room.ID),
				"message_id":           id.Format(*room.LastMessageID),
				"last_read_message_id": id.Format(*room.LastMessageID),
			}
			stargate.PublishToUser(ctx, s.redis, actorID, "MESSAGE_ACK", payload, s.stargateRegion())
		}
	}
	return nil
}

// generateInviteCode returns a short Discord-style invite code (letters + digits).
func (s *Service) generateInviteCode(ctx context.Context) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const length = 8
	for attempts := 0; attempts < 5; attempts++ {
		var b strings.Builder
		for i := 0; i < length; i++ {
			nBig, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return "", err
			}
			b.WriteByte(alphabet[nBig.Int64()])
		}
		code := b.String()
		// Best-effort uniqueness check; collisions are extremely unlikely.
		inv, err := s.repo.GetInviteByCode(ctx, code)
		if err != nil {
			return "", err
		}
		if inv == nil {
			return code, nil
		}
	}
	// Fallback to snowflake-based string if we somehow had repeated collisions.
	return id.Format(id.Next()), nil
}

// GetInvitePreview returns public space info for an invite code (no auth required). Does not join.
func (s *Service) GetInvitePreview(ctx context.Context, code string) (space *Space, inviterDisplayName string, err error) {
	inv, err := s.consumeInvite(ctx, code, false)
	if err != nil {
		return nil, "", err
	}
	space, err = s.repo.GetByID(ctx, inv.SpaceID)
	if err != nil || space == nil {
		return nil, "", ErrSpaceNotFound
	}
	inviter, _ := s.userRepo.GetByID(ctx, inv.InviterID)
	if inviter != nil {
		inviterDisplayName = inviter.DisplayName
		if inviterDisplayName == "" {
			inviterDisplayName = inviter.Username
		}
	}
	return space, inviterDisplayName, nil
}

// JoinByInvite adds the user to the space referenced by the invite code (idempotent).
func (s *Service) JoinByInvite(ctx context.Context, actorID int64, code string) (*Space, error) {
	inv, err := s.consumeInvite(ctx, code, false)
	if err != nil {
		return nil, err
	}
	spaceID := inv.SpaceID
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		if ban, err := s.repo.GetBan(ctx, spaceID, actorID); err != nil {
			return nil, err
		} else if ban != nil {
			return nil, ErrBanned
		}
		spRow, err := s.repo.GetByID(ctx, spaceID)
		if err != nil || spRow == nil {
			return nil, ErrSpaceNotFound
		}
		eid, err := s.ensureEveryoneRoleID(ctx, spRow)
		if err != nil {
			return nil, err
		}
		if err := s.addMember(ctx, spRow, actorID, []int64{eid}); err != nil {
			return nil, err
		}
		// Only a join that actually added someone counts against the invite.
		if _, err := s.consumeInvite(ctx, code, true); err != nil && !errors.Is(err, ErrInviteNotFound) {
			return nil, err
		}
	}
	space, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if space == nil {
		return nil, ErrSpaceNotFound
	}
	return space, nil
}

// canModerateMember checks actorID may kick/ban targetUserID: actor has `need` (or
// Administrator, or is the owner), the target isn't the owner, and - Discord-style
// hierarchy - the target's highest role doesn't outrank the actor's own (the owner is
// exempt from the hierarchy check, same as everywhere else).
func (s *Service) canModerateMember(ctx context.Context, spaceID, actorID, targetUserID int64, need int64) error {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return ErrSpaceNotFound
	}
	if targetUserID == sp.OwnerID {
		return ErrCannotModerateOwner
	}
	if sp.OwnerID == actorID {
		return nil
	}
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return err
	}
	if !permissions.Has(base, need) && !permissions.Has(base, permissions.PermAdministrator) {
		return ErrMissingPerm
	}
	everyoneID, byID, actorHighest, err := s.memberRoleContext(ctx, spaceID, actorID)
	if err != nil {
		return err
	}
	targetMem, err := s.repo.GetMember(ctx, spaceID, targetUserID)
	if err != nil {
		return err
	}
	if targetMem == nil {
		return ErrNotMember
	}
	if highestRolePosition(targetMem.RoleIDs, everyoneID, byID) >= actorHighest {
		return ErrRoleHierarchy
	}
	return nil
}

// publishMemberRemoved notifies the space (member list updates for everyone else) and the
// removed user directly (so their other sessions drop the space locally), for kick, ban,
// and voluntary leave alike.
func (s *Service) publishMemberRemoved(ctx context.Context, spaceID, userID int64) {
	if s.redis == nil {
		return
	}
	region := s.stargateRegion()
	stargate.PublishToSpace(ctx, s.redis, spaceID, "SPACE_MEMBER_REMOVE", map[string]interface{}{
		"space_id": id.Format(spaceID),
		"user_id":  id.Format(userID),
	}, region)
	stargate.PublishToUser(ctx, s.redis, userID, "SPACE_LEAVE", map[string]interface{}{
		"space_id": id.Format(spaceID),
	}, region)
	// Same reasoning as rooms.Service.RemoveParticipant's SESSION_ROTATE: a Megolm
	// session isn't cryptographically revoked by removing someone from the room list, so
	// this tells remaining members' clients a membership change happened in this space -
	// each client already knows which of its own space channels have E2EE on and decides
	// there whether to rotate its outbound session, rather than the server enumerating
	// every E2EE channel in the space here.
	stargate.PublishToSpace(ctx, s.redis, spaceID, "SESSION_ROTATE", map[string]interface{}{
		"space_id": id.Format(spaceID),
	}, region)
}

// KickMember removes a member from the space. They can rejoin with a new invite.
func (s *Service) KickMember(ctx context.Context, actorID, spaceID, targetUserID int64) error {
	if err := s.canModerateMember(ctx, spaceID, actorID, targetUserID, permissions.PermKickMembers); err != nil {
		return err
	}
	if err := s.repo.RemoveMember(ctx, spaceID, targetUserID); err != nil {
		return err
	}
	s.publishMemberRemoved(ctx, spaceID, targetUserID)
	s.audit(ctx, spaceID, actorID, AuditMemberKick, id.Format(targetUserID), nil, "")
	s.announceLeave(ctx, spaceID, targetUserID, "kicked")
	return nil
}

// BanMember removes a member (if present) and blocks them from rejoining via invite until unbanned.
func (s *Service) BanMember(ctx context.Context, actorID, spaceID, targetUserID int64, reason string) error {
	if err := s.canModerateMember(ctx, spaceID, actorID, targetUserID, permissions.PermBanMembers); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if r := []rune(reason); len(r) > MaxBanReasonRunes {
		reason = string(r[:MaxBanReasonRunes])
	}
	ban := &SpaceBan{
		SpaceID:   spaceID,
		UserID:    targetUserID,
		Reason:    reason,
		BannedBy:  actorID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.repo.CreateBan(ctx, ban); err != nil {
		return err
	}
	if err := s.repo.RemoveMember(ctx, spaceID, targetUserID); err != nil {
		return err
	}
	s.publishMemberRemoved(ctx, spaceID, targetUserID)
	s.audit(ctx, spaceID, actorID, AuditMemberBanAdd, id.Format(targetUserID), nil, reason)
	s.announceLeave(ctx, spaceID, targetUserID, "banned")
	return nil
}

// UnbanMember lifts a ban, allowing the user to rejoin via invite again. Requires Ban Members
// (or Administrator, or owner) - same gate as banning, no hierarchy check needed since the
// target isn't necessarily a current member.
func (s *Service) UnbanMember(ctx context.Context, actorID, spaceID, targetUserID int64) error {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return ErrSpaceNotFound
	}
	if sp.OwnerID != actorID {
		base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
		if err != nil {
			return err
		}
		if !permissions.Has(base, permissions.PermBanMembers) && !permissions.Has(base, permissions.PermAdministrator) {
			return ErrMissingPerm
		}
	}
	if err := s.repo.DeleteBan(ctx, spaceID, targetUserID); err != nil {
		return err
	}
	s.audit(ctx, spaceID, actorID, AuditMemberBanRemove, id.Format(targetUserID), nil, "")
	return nil
}

// ListBans returns the space's ban list plus the profiles of the banned users and the
// moderators who banned them. Requires Ban Members (or Administrator, or owner).
func (s *Service) ListBans(ctx context.Context, actorID, spaceID int64) ([]SpaceBan, map[int64]*auth.User, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, nil, err
	}
	if sp == nil {
		return nil, nil, ErrSpaceNotFound
	}
	if sp.OwnerID != actorID {
		base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
		if err != nil {
			return nil, nil, err
		}
		if !permissions.Has(base, permissions.PermBanMembers) && !permissions.Has(base, permissions.PermAdministrator) {
			return nil, nil, ErrMissingPerm
		}
	}
	bans, err := s.repo.ListBans(ctx, spaceID)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(bans)*2)
	seen := map[int64]struct{}{}
	for _, b := range bans {
		for _, uid := range []int64{b.UserID, b.BannedBy} {
			if uid == 0 {
				continue
			}
			if _, ok := seen[uid]; !ok {
				seen[uid] = struct{}{}
				ids = append(ids, uid)
			}
		}
	}
	users := make(map[int64]*auth.User, len(ids))
	if len(ids) > 0 {
		if list, err := s.userRepo.GetByIDs(ctx, ids); err == nil {
			for _, u := range list {
				if u != nil {
					users[u.ID] = u
				}
			}
		}
	}
	return bans, users, nil
}

// LeaveSpace removes actorID from the space. The owner must transfer ownership or delete
// the space instead - mirrors Discord, which refuses to let an owner just leave.
func (s *Service) LeaveSpace(ctx context.Context, actorID, spaceID int64) error {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return ErrSpaceNotFound
	}
	if sp.OwnerID == actorID {
		return ErrOwnerCannotLeave
	}
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	if err := s.repo.RemoveMember(ctx, spaceID, actorID); err != nil {
		return err
	}
	s.publishMemberRemoved(ctx, spaceID, actorID)
	s.announceLeave(ctx, spaceID, actorID, "left")
	return nil
}

func spaceToEventPayload(s *Space) map[string]interface{} {
	m := map[string]interface{}{
		"id":                            id.Format(s.ID),
		"name":                          s.Name,
		"name_acronym":                  s.NameAcronym,
		"description":                   s.Description,
		"icon":                          s.Icon,
		"banner":                        s.Banner,
		"owner_id":                      id.Format(s.OwnerID),
		"verification_level":            s.VerificationLevel,
		"default_message_notifications": s.DefaultMessageNotif,
		"explicit_content_filter":       s.ExplicitContentFilter,
		"features":                      s.Features,
		"afk_timeout":                   s.AFKTimeout,
		"system_room_flags":             s.SystemRoomFlags,
		"max_presences":                 s.MaxPresences,
		"max_members":                   s.MaxMembers,
		"vanity_url_code":               s.VanityURLCode,
		"preferred_locale":              s.PreferredLocale,
		"max_video_room_users":          s.MaxVideoRoomUsers,
		"widget_enabled":                s.WidgetEnabled,
		"created_at":                    s.CreatedAt,
		"updated_at":                    s.UpdatedAt,
	}
	if s.AFKRoomID != nil {
		m["afk_room_id"] = id.Format(*s.AFKRoomID)
	}
	if s.SystemRoomID != nil {
		m["system_room_id"] = id.Format(*s.SystemRoomID)
	}
	if s.RulesRoomID != nil {
		m["rules_room_id"] = id.Format(*s.RulesRoomID)
	}
	if s.PublicUpdatesRoomID != nil {
		m["public_updates_room_id"] = id.Format(*s.PublicUpdatesRoomID)
	}
	if s.WidgetRoomID != nil {
		m["widget_room_id"] = id.Format(*s.WidgetRoomID)
	}
	return m
}
