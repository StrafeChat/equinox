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
	ErrInvalidSpaceName              = errors.New("space name cannot be empty")
	ErrNothingToPatch                = errors.New("no fields to update")
)

type Service struct {
	repo     Repository
	roomRepo rooms.Repository
	userRepo auth.UserRepository
	redis    *redis.Client
	cfg      *config.Config
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
	spaceID := id.Next()
	now := time.Now().UTC()
	space := &Space{
		ID:                  spaceID,
		Name:                name,
		NameAcronym:         nameAcronym(name),
		Description:         strings.TrimSpace(in.Description),
		Icon:                in.Icon,
		OwnerID:             actorID,
		VerificationLevel:   0,
		DefaultMessageNotif: 0,
		ExplicitContentFilter: 0,
		Features:            nil,
		MaxPresences:        0,
		MaxMembers:          0,
		CreatedAt:           now,
		UpdatedAt:           now,
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
		ID:       textSectionID,
		Type:     rooms.TypeRoomSection,
		SpaceID:  &spaceID,
		Name:     "Text Rooms",
		Position: 0,
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

// ListSpaceRooms returns rooms in the space (text and voice). User must be a member.
func (s *Service) ListSpaceRooms(ctx context.Context, userID, spaceID int64) ([]*rooms.Room, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	rows, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })
	out := make([]*rooms.Room, 0, len(rows))
	for _, row := range rows {
		room, err := s.roomRepo.GetByID(ctx, row.RoomID)
		if err != nil || room == nil {
			continue
		}
		out = append(out, room)
	}
	return out, nil
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

// CreateInvite creates a new invite for the given space. Only the space owner can create invites for now.
func (s *Service) CreateInvite(ctx context.Context, actorID, spaceID int64) (*SpaceInvite, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
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
	if space.OwnerID != actorID {
		return nil, ErrNotSpaceOwner
	}
	now := time.Now().UTC()
	code, err := s.generateInviteCode(ctx)
	if err != nil {
		return nil, err
	}
	inv := &SpaceInvite{
		Code:      code,
		SpaceID:   spaceID,
		InviterID: actorID,
		CreatedAt: now,
	}
	if err := s.repo.CreateInvite(ctx, inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// GetInvitePreview returns public space info for an invite code (no auth required). Does not join.
func (s *Service) GetInvitePreview(ctx context.Context, code string) (space *Space, inviterDisplayName string, err error) {
	if code == "" {
		return nil, "", ErrInviteNotFound
	}
	inv, err := s.repo.GetInviteByCode(ctx, code)
	if err != nil {
		return nil, "", err
	}
	if inv == nil {
		return nil, "", ErrInviteNotFound
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
	if code == "" {
		return nil, ErrInviteNotFound
	}
	inv, err := s.repo.GetInviteByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		return nil, ErrInviteNotFound
	}
	spaceID := inv.SpaceID
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		spRow, err := s.repo.GetByID(ctx, spaceID)
		if err != nil || spRow == nil {
			return nil, ErrSpaceNotFound
		}
		eid, err := s.ensureEveryoneRoleID(ctx, spRow)
		if err != nil {
			return nil, err
		}
		if err := s.repo.AddMember(ctx, spaceID, actorID, []int64{eid}); err != nil {
			return nil, err
		}
		if err := s.repo.AddSpaceToUserSet(ctx, actorID, spaceID); err != nil {
			return nil, err
		}
		// Publish SPACE_MEMBER_ADD to the space channel so connected members see the join in real time.
		if s.redis != nil {
			u, _ := s.userRepo.GetByID(ctx, actorID)
			if u != nil {
				payload := map[string]interface{}{
					"space_id":      id.Format(spaceID),
					"id":            id.Format(u.ID),
					"username":      u.Username,
					"discriminator": u.Discriminator,
					"display_name":  u.DisplayName,
					"avatar":        u.Avatar,
					"presence":      auth.ToPublicPresence(u.Presence, true),
				}
				if mem, _ := s.repo.GetMember(ctx, spaceID, actorID); mem != nil {
					payload["role_ids"] = formatRoleIDStrings(mem.RoleIDs)
				}
				stargate.PublishToSpace(ctx, s.redis, spaceID, "SPACE_MEMBER_ADD", payload, s.stargateRegion())
			}
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

func spaceToEventPayload(s *Space) map[string]interface{} {
	m := map[string]interface{}{
		"id":                          id.Format(s.ID),
		"name":                        s.Name,
		"name_acronym":                s.NameAcronym,
		"description":                 s.Description,
		"icon":                        s.Icon,
		"banner":                      s.Banner,
		"owner_id":                    id.Format(s.OwnerID),
		"verification_level":          s.VerificationLevel,
		"default_message_notifications": s.DefaultMessageNotif,
		"explicit_content_filter":     s.ExplicitContentFilter,
		"features":                    s.Features,
		"afk_timeout":                 s.AFKTimeout,
		"system_room_flags":           s.SystemRoomFlags,
		"max_presences":               s.MaxPresences,
		"max_members":                 s.MaxMembers,
		"vanity_url_code":             s.VanityURLCode,
		"preferred_locale":             s.PreferredLocale,
		"max_video_room_users":        s.MaxVideoRoomUsers,
		"created_at":                  s.CreatedAt,
		"updated_at":                  s.UpdatedAt,
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
	return m
}
