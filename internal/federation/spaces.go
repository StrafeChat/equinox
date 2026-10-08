package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// Spaces across instances - the engine side of spaces/federation.go. The origin of a
// space is its one authority; it pushes every change to the instances mirroring it
// (POST /spaces/update, /members, /roles, /rooms, /delete, and the existing room
// endpoints for messages, reactions and typing in its channels), and serves what a
// mirror's member asks of it (GET /spaces/invites/:code, POST /spaces/join, /leave,
// /invites, /messages, /messages/list, /messages/get, /reactions). Ids on the wire are
// the origin's; role ids are kept as they are on a mirror (they only need to be unique
// within the space), rooms and messages are mapped like a PM's, and users are profiles.
// A message in a channel keeps the origin's id on every mirror, so replies, reactions
// and ordering agree everywhere without a lookup.

// ---- wire types -------------------------------------------------------------------------

type SpaceRef struct {
	OriginDomain  string `json:"origin_domain"`
	OriginSpaceID string `json:"origin_space_id"`
}

// SpaceInfo is a space's settings as the origin describes them (room ids are its own).
type SpaceInfo struct {
	Name                  string    `json:"name"`
	NameAcronym           string    `json:"name_acronym"`
	Description           string    `json:"description,omitempty"`
	Icon                  string    `json:"icon,omitempty"`
	Banner                string    `json:"banner,omitempty"`
	Owner                 string    `json:"owner"`
	VerificationLevel     int       `json:"verification_level"`
	DefaultMessageNotif   int       `json:"default_message_notifications"`
	ExplicitContentFilter int       `json:"explicit_content_filter"`
	Features              []string  `json:"features,omitempty"`
	AFKRoom               string    `json:"afk_room,omitempty"`
	AFKTimeout            int       `json:"afk_timeout"`
	SystemRoom            string    `json:"system_room,omitempty"`
	SystemRoomFlags       int       `json:"system_room_flags"`
	RulesRoom             string    `json:"rules_room,omitempty"`
	PublicUpdatesRoom     string    `json:"public_updates_room,omitempty"`
	WidgetRoom            string    `json:"widget_room,omitempty"`
	WidgetEnabled         bool      `json:"widget_enabled"`
	MaxPresences          int       `json:"max_presences"`
	MaxMembers            int       `json:"max_members"`
	MaxVideoRoomUsers     int       `json:"max_video_room_users"`
	VanityURLCode         string    `json:"vanity_url_code,omitempty"`
	PreferredLocale       string    `json:"preferred_locale,omitempty"`
	EveryoneRole          string    `json:"everyone_role"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type SpaceRoleWire struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Permissions int64     `json:"permissions"`
	Position    int       `json:"position"`
	Color       int       `json:"color"`
	Hoist       bool      `json:"hoist"`
	Mentionable bool      `json:"mentionable"`
	Bot         string    `json:"bot,omitempty"` // FID of the bot a managed role belongs to
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SpaceOverrideWire is a room override for a role (Role set) or a user (User set).
type SpaceOverrideWire struct {
	Role      string    `json:"role,omitempty"`
	User      string    `json:"user,omitempty"`
	Allow     int64     `json:"allow"`
	Deny      int64     `json:"deny"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SpaceRoomWire struct {
	ID              string              `json:"id"`
	Type            int                 `json:"type"`
	Parent          string              `json:"parent,omitempty"`
	Name            string              `json:"name"`
	Topic           string              `json:"topic,omitempty"`
	SlowmodeSeconds int                 `json:"slowmode_seconds,omitempty"`
	Position        int                 `json:"position"`
	E2EEEnabled     bool                `json:"e2ee_enabled"`
	UserLimit       int                 `json:"user_limit,omitempty"`
	Bitrate         int                 `json:"bitrate,omitempty"`
	Overrides       []SpaceOverrideWire `json:"overrides,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	// Thread is set for a thread room (type 6): its state as the origin holds it. Thread
	// membership is not mirrored, so a private thread is not visible on a mirror.
	Thread *ThreadWire `json:"thread,omitempty"`
}

// ThreadWire is a thread's state on the wire (see rooms.Room's Thread* fields).
type ThreadWire struct {
	Archived           bool       `json:"archived"`
	ArchivedAt         *time.Time `json:"archived_at,omitempty"`
	AutoArchiveMinutes int        `json:"auto_archive_minutes,omitempty"`
	Locked             bool       `json:"locked"`
	Private            bool       `json:"private"`
	Invitable          bool       `json:"invitable"`
	LastActiveAt       *time.Time `json:"last_active_at,omitempty"`
	Owner              string     `json:"owner,omitempty"` // FID
	StarterMessage     string     `json:"starter_message,omitempty"`
}

type SpaceMemberWire struct {
	User     Profile   `json:"user"`
	Roles    []string  `json:"roles"`
	Nickname string    `json:"nickname,omitempty"`
	JoinedAt time.Time `json:"joined_at"`
	// Presence as others see it, so a mirror's member list starts with real statuses
	// instead of "offline until their next change".
	Presence auth.PublicPresence `json:"presence"`
}

// SpaceSnapshot is everything a mirror is built from: the join reply (and a resync).
// Members is the first page; MembersNext, when set, is the cursor to fetch the rest with
// /spaces/members/list. Peers are the other instances in the space (the origin itself
// excluded), so a mirror's users reach them directly with presence and profile changes.
type SpaceSnapshot struct {
	Space       SpaceRef          `json:"space"`
	Info        SpaceInfo         `json:"info"`
	Roles       []SpaceRoleWire   `json:"roles"`
	Rooms       []SpaceRoomWire   `json:"rooms"`
	Members     []SpaceMemberWire `json:"members"`
	MembersNext string            `json:"members_next,omitempty"`
	Emoji       []SpaceEmojiWire  `json:"emoji,omitempty"`
	MemberCount int               `json:"member_count"`
	Peers       []string          `json:"peers,omitempty"`
}

// SpaceJoinRequest: POST /spaces/join - a user on the requesting instance redeems an
// invite for a space hosted here. Presence is how they appear right now, so the member
// every mirror is told about starts with their real status.
type SpaceJoinRequest struct {
	Code     string               `json:"code"`
	User     Profile              `json:"user"`
	Presence *auth.PublicPresence `json:"presence,omitempty"`
}

// SpaceMembersQuery: POST /spaces/members/list - the page of a hosted space's members
// after a cursor (a snapshot's members_next, or an earlier page's next).
type SpaceMembersQuery struct {
	Space SpaceRef `json:"space"`
	After string   `json:"after,omitempty"`
}

type SpaceMembersReply struct {
	Members []SpaceMemberWire `json:"members"`
	Next    string            `json:"next,omitempty"`
}

// SpacePeerEvent: POST /spaces/peers - an instance started or stopped mirroring the
// space (origin → mirrors).
type SpacePeerEvent struct {
	Space  SpaceRef `json:"space"`
	Action string   `json:"action"` // add | remove
	Domain string   `json:"domain"`
}

// SpaceInvitePreviewReply: GET /spaces/invites/:code.
type SpaceInvitePreviewReply struct {
	Space       SpaceRef `json:"space"`
	Name        string   `json:"name"`
	NameAcronym string   `json:"name_acronym"`
	Description string   `json:"description,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Banner      string   `json:"banner,omitempty"`
	MemberCount int      `json:"member_count"`
	Inviter     string   `json:"inviter,omitempty"`
}

// SpaceUserRequest: POST /spaces/leave.
type SpaceUserRequest struct {
	Space SpaceRef `json:"space"`
	User  string   `json:"user"` // FID, must belong to the requesting instance
}

// SpaceInviteRequest: POST /spaces/invites - a member on the requesting instance wants
// an invite for a space hosted here.
type SpaceInviteRequest struct {
	Space         SpaceRef `json:"space"`
	User          string   `json:"user"`
	MaxAgeSeconds int      `json:"max_age_seconds,omitempty"`
	MaxUses       int      `json:"max_uses,omitempty"`
}

type SpaceInviteReply struct {
	Code        string     `json:"code"`
	MaxUses     int        `json:"max_uses"`
	CurrentUses int        `json:"current_uses"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// SpaceMessageRequest: POST /spaces/messages - a member on the requesting instance sends
// into a channel hosted here. Attachments were uploaded to their own instance.
type SpaceMessageRequest struct {
	Room            RoomRef               `json:"room"`
	User            string                `json:"user"`
	SenderDeviceID  string                `json:"sender_device_id,omitempty"`
	Ciphertext      string                `json:"ciphertext,omitempty"`
	Plaintext       string                `json:"plaintext,omitempty"`
	ReplyTo         *MessageRef           `json:"reply_to,omitempty"`
	Mentions        []string              `json:"mentions,omitempty"`
	MentionRoles    []string              `json:"mention_roles,omitempty"`
	MentionEveryone bool                  `json:"mention_everyone,omitempty"`
	Attachments     []messages.Attachment `json:"attachments,omitempty"`
	// Emojis describes the custom emoji the text uses; mentions in it are <@FID> (content.go).
	Emojis []EmojiRef `json:"emojis,omitempty"`
}

// SpaceMessageEditRequest: PATCH /spaces/messages.
type SpaceMessageEditRequest struct {
	Room       RoomRef    `json:"room"`
	Message    MessageRef `json:"message"`
	User       string     `json:"user"`
	Ciphertext string     `json:"ciphertext,omitempty"`
	Plaintext  string     `json:"plaintext,omitempty"`
	Emojis     []EmojiRef `json:"emojis,omitempty"`
}

// SpaceMessageRequestRef: POST /spaces/messages/delete and /spaces/messages/get.
type SpaceMessageRequestRef struct {
	Room    RoomRef    `json:"room"`
	Message MessageRef `json:"message"`
	User    string     `json:"user"`
}

type SpaceReactionReply struct {
	Reactions []messages.ReactionSummary `json:"reactions"`
}

// SpaceMessagesQuery: POST /spaces/messages/list - a page of a channel's history as the
// asking member may see it.
type SpaceMessagesQuery struct {
	Room RoomRef `json:"room"`
	User string  `json:"user"`
	// At most one of Before / After / Around (origin message ids): the page below a
	// message, the page above one (oldest-first), or the window around one - the same
	// three cursors GET /rooms/:id/messages takes.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Around string `json:"around,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type SpaceMessagesReply struct {
	Messages []MessageEvent `json:"messages"`
}

// SpaceSearchQuery: POST /spaces/messages/search - search a hosted space's text channels
// (or one of them, Room) as the asking member may read them. From and Mentions are FIDs.
type SpaceSearchQuery struct {
	Space    SpaceRef `json:"space"`
	Room     *RoomRef `json:"room,omitempty"`
	User     string   `json:"user"`
	Query    string   `json:"query,omitempty"`
	From     string   `json:"from,omitempty"`
	Mentions string   `json:"mentions,omitempty"`
	Has      string   `json:"has,omitempty"`
	Before   string   `json:"before,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

type SpaceSearchReply struct {
	Messages       []MessageEvent `json:"messages"`
	NextBefore     string         `json:"next_before,omitempty"`
	RoomsSearched  int            `json:"rooms_searched"`
	EncryptedRooms int            `json:"encrypted_rooms"`
}

// Origin → mirrors.
type SpaceUpdateEvent struct {
	Space SpaceRef  `json:"space"`
	Info  SpaceInfo `json:"info"`
}

type SpaceMemberEvent struct {
	Space  SpaceRef        `json:"space"`
	Action string          `json:"action"` // add | update | remove
	Member SpaceMemberWire `json:"member"`
	How    string          `json:"how,omitempty"` // remove: kicked | banned | left
}

type SpaceRoleEvent struct {
	Space  SpaceRef      `json:"space"`
	Action string        `json:"action"` // upsert | delete
	Role   SpaceRoleWire `json:"role"`
}

type SpaceRoomOrder struct {
	ID       string `json:"id"`
	Parent   string `json:"parent,omitempty"`
	Position int    `json:"position"`
}

type SpaceRoomEvent struct {
	Space  SpaceRef         `json:"space"`
	Action string           `json:"action"` // upsert | delete | reorder
	Room   *SpaceRoomWire   `json:"room,omitempty"`
	Order  []SpaceRoomOrder `json:"order,omitempty"`
}

type SpaceDeleteEvent struct {
	Space SpaceRef `json:"space"`
}

// Caps on what a peer may hand us about a space.
const (
	maxMirrorMembers = 100000 // members a mirror holds at most (fetched in pages)
	maxMemberPage    = 1000   // members per page, served to and accepted from a peer
	maxSnapshotRooms = 500
	maxSnapshotRoles = 250
	maxSpacePeers    = 1000
	maxHistoryPage   = 100
)

var (
	errNotSpaceOrigin = errors.New("this instance does not host that space")
	errSpaceNotMirror = errors.New("that space is not mirrored here")
)

// ---- conversions (origin side) ----------------------------------------------------------

func (s *Service) spaceRef(ctx context.Context, spaceID int64) SpaceRef {
	if m, err := s.repo.GetSpaceMapping(ctx, spaceID); err == nil && m != nil {
		return SpaceRef{OriginDomain: m.OriginDomain, OriginSpaceID: id.Format(m.OriginSpaceID)}
	}
	return SpaceRef{OriginDomain: s.fcfg.Domain, OriginSpaceID: id.Format(spaceID)}
}

func fmtOptID(p *int64) string {
	if p == nil {
		return ""
	}
	return id.Format(*p)
}

func (s *Service) spaceInfo(ctx context.Context, sp *spaces.Space) SpaceInfo {
	info := SpaceInfo{
		Name:                  sp.Name,
		NameAcronym:           sp.NameAcronym,
		Description:           sp.Description,
		Icon:                  sp.Icon,
		Banner:                sp.Banner,
		Owner:                 s.LocalFID(sp.OwnerID),
		VerificationLevel:     sp.VerificationLevel,
		DefaultMessageNotif:   sp.DefaultMessageNotif,
		ExplicitContentFilter: sp.ExplicitContentFilter,
		Features:              sp.Features,
		AFKRoom:               fmtOptID(sp.AFKRoomID),
		AFKTimeout:            sp.AFKTimeout,
		SystemRoom:            fmtOptID(sp.SystemRoomID),
		SystemRoomFlags:       sp.SystemRoomFlags,
		RulesRoom:             fmtOptID(sp.RulesRoomID),
		PublicUpdatesRoom:     fmtOptID(sp.PublicUpdatesRoomID),
		WidgetRoom:            fmtOptID(sp.WidgetRoomID),
		WidgetEnabled:         sp.WidgetEnabled,
		MaxPresences:          sp.MaxPresences,
		MaxMembers:            sp.MaxMembers,
		MaxVideoRoomUsers:     sp.MaxVideoRoomUsers,
		VanityURLCode:         sp.VanityURLCode,
		PreferredLocale:       sp.PreferredLocale,
		EveryoneRole:          id.Format(sp.EveryoneRoleID),
		CreatedAt:             sp.CreatedAt,
		UpdatedAt:             sp.UpdatedAt,
	}
	if owner, _ := s.users.GetByID(ctx, sp.OwnerID); owner != nil {
		info.Owner = s.FIDOf(owner)
	}
	return info
}

func (s *Service) roleWire(ctx context.Context, r *spaces.SpaceRole) SpaceRoleWire {
	w := SpaceRoleWire{
		ID: id.Format(r.ID), Name: r.Name, Permissions: r.Permissions, Position: r.Position,
		Color: r.Color, Hoist: r.Hoist, Mentionable: r.Mentionable, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.BotID != 0 {
		if bot, _ := s.users.GetByID(ctx, r.BotID); bot != nil {
			w.Bot = s.FIDOf(bot)
		}
	}
	return w
}

func (s *Service) roomWire(ctx context.Context, room *rooms.Room, ov spaces.RoomOverrides) SpaceRoomWire {
	w := SpaceRoomWire{
		ID: id.Format(room.ID), Type: room.Type, Parent: fmtOptID(room.ParentID), Name: room.Name, Topic: room.Topic,
		SlowmodeSeconds: room.SlowmodeSeconds, Position: room.Position,
		E2EEEnabled: room.E2EEEnabled != nil && *room.E2EEEnabled,
		UserLimit:   room.UserLimit, Bitrate: room.Bitrate, CreatedAt: room.CreatedAt, UpdatedAt: room.UpdatedAt,
	}
	if room.Type == rooms.TypeThread {
		tw := &ThreadWire{
			Archived: room.ThreadIsArchived(), ArchivedAt: room.ThreadArchivedAt, AutoArchiveMinutes: room.ThreadAutoArchiveMin,
			Locked: room.ThreadIsLocked(), Private: room.ThreadIsPrivate(), Invitable: room.ThreadIsInvitable(), LastActiveAt: room.ThreadLastActiveAt,
		}
		if owner, _ := s.users.GetByID(ctx, room.CreatorID); owner != nil {
			tw.Owner = s.FIDOf(owner)
		}
		if room.ThreadStarterMessageID != nil {
			tw.StarterMessage = id.Format(*room.ThreadStarterMessageID)
		}
		w.Thread = tw
	}
	for _, o := range ov.Roles {
		w.Overrides = append(w.Overrides, SpaceOverrideWire{Role: id.Format(o.RoleID), Allow: o.Allow, Deny: o.Deny, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt})
	}
	if len(ov.Users) > 0 {
		ids := make([]int64, 0, len(ov.Users))
		for _, o := range ov.Users {
			ids = append(ids, o.UserID)
		}
		users, _ := s.users.GetByIDs(ctx, ids)
		for i, o := range ov.Users {
			if i < len(users) && users[i] != nil {
				w.Overrides = append(w.Overrides, SpaceOverrideWire{User: s.FIDOf(users[i]), Allow: o.Allow, Deny: o.Deny, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt})
			}
		}
	}
	return w
}

func (s *Service) memberWire(m *spaces.SpaceMember, u *auth.User) SpaceMemberWire {
	w := SpaceMemberWire{
		User: s.ProfileOf(u), Roles: make([]string, 0, len(m.RoleIDs)), Nickname: m.Nickname, JoinedAt: m.JoinedAt,
		Presence: auth.ToPublicPresence(u.Presence, true),
	}
	for _, rid := range m.RoleIDs {
		w.Roles = append(w.Roles, id.Format(rid))
	}
	return w
}

// snapshot describes a space hosted here to a peer.
func (s *Service) snapshot(ctx context.Context, sp *spaces.Space) (*SpaceSnapshot, error) {
	roomList, snap, err := s.spaceSvc.SpaceRoomsWithOverrides(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	out := &SpaceSnapshot{Space: s.spaceRef(ctx, sp.ID), Info: s.spaceInfo(ctx, sp)}
	if snap.EveryoneRoleID != 0 {
		out.Info.EveryoneRole = id.Format(snap.EveryoneRoleID)
	}
	for i := range snap.Roles {
		out.Roles = append(out.Roles, s.roleWire(ctx, &snap.Roles[i]))
	}
	for _, r := range roomList {
		// Register each channel's global identity now: the mirror will name it in its
		// first message before anything was relayed from it.
		if _, err := s.roomRef(ctx, r.ID); err != nil {
			return nil, err
		}
		out.Rooms = append(out.Rooms, s.roomWire(ctx, r, snap.RoomOverridesFor(r.ID)))
	}
	// Members come in pages (the mirror fetches the rest through /spaces/members/list), so
	// a big space neither makes one huge reply nor gets cut off at an arbitrary count.
	count, err := s.spaceSvc.CountMembers(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	out.MemberCount = count
	if out.Members, out.MembersNext, err = s.membersPage(ctx, sp.ID, 0); err != nil {
		return nil, err
	}
	for d := range s.spacePeers(ctx, sp.ID) {
		out.Peers = append(out.Peers, d)
	}
	sort.Strings(out.Peers)
	emoji, err := s.spaceSvc.Emojis(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	for i := range emoji {
		out.Emoji = append(out.Emoji, s.emojiWire(ctx, &emoji[i]))
	}
	return out, nil
}

// ---- conversions (mirror side) ----------------------------------------------------------

// localRoomID maps an origin room id to this instance's room, allocating an id (and the
// mapping) when allocate is set and the room is new here.
func (s *Service) localRoomID(ctx context.Context, origin string, originRoomID int64, allocate bool) (int64, error) {
	m, err := s.repo.GetRoomByOrigin(ctx, origin, originRoomID)
	if err != nil {
		return 0, err
	}
	if m != nil {
		return m.RoomID, nil
	}
	if !allocate {
		return 0, rooms.ErrRoomNotFound
	}
	local := id.Next()
	if err := s.repo.PutRoomMapping(ctx, &RoomMapping{RoomID: local, OriginDomain: origin, OriginRoomID: originRoomID}); err != nil {
		return 0, err
	}
	return local, nil
}

// roomIDResolver maps the origin's optional room ids (settings) to local ones.
func (s *Service) roomIDResolver(ctx context.Context, origin string) func(string) *int64 {
	return func(raw string) *int64 {
		if raw == "" {
			return nil
		}
		oid, err := id.Parse(raw)
		if err != nil {
			return nil
		}
		local, err := s.localRoomID(ctx, origin, oid, false)
		if err != nil {
			return nil
		}
		return &local
	}
}

// clampText keeps a peer-supplied string within a rune budget.
func clampText(v string, max int) string {
	v = strings.TrimSpace(v)
	if r := []rune(v); len(r) > max {
		return string(r[:max])
	}
	return v
}

func httpURLOnly(u string) string {
	u = strings.TrimSpace(u)
	if len(u) > 512 || (!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://")) {
		return ""
	}
	return u
}

// applySpaceInfo overlays the origin's settings onto a (new or stored) space row. The
// owner must already be a known user here (they are always a member).
func (s *Service) applySpaceInfo(ctx context.Context, sp *spaces.Space, origin string, info SpaceInfo) {
	roomID := s.roomIDResolver(ctx, origin)
	sp.Name = clampText(info.Name, spaces.MaxSpaceNameRunes)
	if sp.Name == "" {
		sp.Name = "Unnamed Space"
	}
	sp.NameAcronym = clampText(info.NameAcronym, 4)
	sp.Description = clampText(info.Description, spaces.MaxSpaceDescriptionRunes)
	sp.Icon = httpURLOnly(info.Icon)
	sp.Banner = httpURLOnly(info.Banner)
	if uid, err := s.ResolveLocalID(ctx, info.Owner); err == nil && uid != 0 {
		sp.OwnerID = uid
	}
	sp.VerificationLevel = info.VerificationLevel
	sp.DefaultMessageNotif = info.DefaultMessageNotif
	sp.ExplicitContentFilter = info.ExplicitContentFilter
	sp.Features = info.Features
	sp.AFKRoomID = roomID(info.AFKRoom)
	sp.AFKTimeout = info.AFKTimeout
	sp.SystemRoomID = roomID(info.SystemRoom)
	sp.SystemRoomFlags = info.SystemRoomFlags
	sp.RulesRoomID = roomID(info.RulesRoom)
	sp.PublicUpdatesRoomID = roomID(info.PublicUpdatesRoom)
	sp.WidgetRoomID = roomID(info.WidgetRoom)
	sp.WidgetEnabled = info.WidgetEnabled
	sp.MaxPresences = info.MaxPresences
	sp.MaxMembers = info.MaxMembers
	sp.MaxVideoRoomUsers = info.MaxVideoRoomUsers
	sp.VanityURLCode = clampText(info.VanityURLCode, 64)
	sp.PreferredLocale = clampText(info.PreferredLocale, 16)
	if rid, err := id.Parse(info.EveryoneRole); err == nil {
		sp.EveryoneRoleID = rid
	}
	if !info.CreatedAt.IsZero() {
		sp.CreatedAt = info.CreatedAt
	}
	sp.UpdatedAt = info.UpdatedAt
	if sp.UpdatedAt.IsZero() {
		sp.UpdatedAt = time.Now().UTC()
	}
}

func (s *Service) roleFromWire(ctx context.Context, w SpaceRoleWire) (*spaces.SpaceRole, error) {
	rid, err := id.Parse(w.ID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	r := &spaces.SpaceRole{
		ID: rid, Name: clampText(w.Name, spaces.MaxRoleNameRunes), Permissions: w.Permissions, Position: w.Position,
		Color: w.Color, Hoist: w.Hoist, Mentionable: w.Mentionable, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
	if w.Bot != "" {
		// The role may arrive before the member-add that introduces its bot (an install
		// creates the role first), so fetch the bot from its home if nobody here knows it.
		if u, err := s.shadowByFID(ctx, w.Bot); err == nil && u != nil {
			r.BotID = u.ID
		}
	}
	return r, nil
}

// mirrorRoom turns a room the origin described into local ids.
func (s *Service) mirrorRoom(ctx context.Context, origin string, w SpaceRoomWire, allocate bool) (spaces.MirrorRoom, error) {
	oid, err := id.Parse(w.ID)
	if err != nil {
		return spaces.MirrorRoom{}, ErrInvalidFID
	}
	local, err := s.localRoomID(ctx, origin, oid, allocate)
	if err != nil {
		return spaces.MirrorRoom{}, err
	}
	room := &rooms.Room{
		ID: local, Type: w.Type, Name: clampText(w.Name, spaces.MaxRoomNameRunes), Topic: clampText(w.Topic, spaces.MaxTopicRunes),
		SlowmodeSeconds: w.SlowmodeSeconds, Position: w.Position, UserLimit: w.UserLimit, Bitrate: w.Bitrate,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
	if w.Type == rooms.TypeSpaceText || w.Type == rooms.TypeSpaceVoice || w.Type == rooms.TypeThread {
		e2ee := w.E2EEEnabled
		room.E2EEEnabled = &e2ee
	}
	if w.Type == rooms.TypeThread && w.Thread != nil {
		archived, locked, private, invitable := w.Thread.Archived, w.Thread.Locked, w.Thread.Private, w.Thread.Invitable
		room.ThreadArchived, room.ThreadLocked, room.ThreadPrivate, room.ThreadInvitable = &archived, &locked, &private, &invitable
		room.ThreadArchivedAt, room.ThreadLastActiveAt, room.ThreadAutoArchiveMin = w.Thread.ArchivedAt, w.Thread.LastActiveAt, w.Thread.AutoArchiveMinutes
		if w.Thread.Owner != "" {
			if uid, err := s.ResolveLocalID(ctx, w.Thread.Owner); err == nil {
				room.CreatorID = uid
			}
		}
		if w.Thread.StarterMessage != "" {
			if mid, err := id.Parse(w.Thread.StarterMessage); err == nil {
				room.ThreadStarterMessageID = &mid
			}
		}
	}
	if w.Parent != "" {
		if pid, err := id.Parse(w.Parent); err == nil {
			if parent, err := s.localRoomID(ctx, origin, pid, allocate); err == nil {
				room.ParentID = &parent
			}
		}
	}
	mr := spaces.MirrorRoom{Room: room}
	now := time.Now().UTC()
	for _, o := range w.Overrides {
		created, updated := o.CreatedAt, o.UpdatedAt
		if created.IsZero() {
			created = now
		}
		if updated.IsZero() {
			updated = created
		}
		switch {
		case o.Role != "":
			if rid, err := id.Parse(o.Role); err == nil {
				mr.Overrides.Roles = append(mr.Overrides.Roles, spaces.SpaceRoomRoleOverride{RoomID: local, RoleID: rid, Allow: o.Allow, Deny: o.Deny, CreatedAt: created, UpdatedAt: updated})
			}
		case o.User != "":
			if uid, err := s.ResolveLocalID(ctx, o.User); err == nil {
				mr.Overrides.Users = append(mr.Overrides.Users, spaces.SpaceRoomUserOverride{RoomID: local, UserID: uid, Allow: o.Allow, Deny: o.Deny, CreatedAt: created, UpdatedAt: updated})
			}
		}
	}
	return mr, nil
}

// mirrorMember resolves a member the origin described to a local user id, creating or
// refreshing their shadow.
func (s *Service) mirrorMember(ctx context.Context, origin string, w SpaceMemberWire) (spaces.MirrorMember, error) {
	u, err := s.resolveProfile(ctx, w.User, origin)
	if err != nil {
		return spaces.MirrorMember{}, err
	}
	s.applyRelayedPresence(ctx, u, w.Presence)
	m := spaces.MirrorMember{UserID: u.ID, Nickname: clampText(w.Nickname, 32), JoinedAt: w.JoinedAt}
	for _, raw := range w.Roles {
		if rid, err := id.Parse(raw); err == nil {
			m.RoleIDs = append(m.RoleIDs, rid)
		}
	}
	return m, nil
}

// applyRelayedPresence writes the status a peer reported for one of its (or a third
// instance's) users onto their shadow row, when it differs from what is stored, and tells
// local clients. Local users are never touched: their own gateway is the authority on
// their presence. Reports whether anything changed.
func (s *Service) applyRelayedPresence(ctx context.Context, u *auth.User, p auth.PublicPresence) bool {
	if u == nil || !u.IsRemote() {
		return false
	}
	switch p.Status {
	case "online", "idle", "dnd", "offline":
	default:
		return false
	}
	if utf8.RuneCountInString(p.CustomStatus) > maxRelayedCustomStatus {
		return false
	}
	current := auth.ToPublicPresence(u.Presence, true)
	if current.Status == p.Status && current.CustomStatus == p.CustomStatus {
		return false
	}
	online := p.Status != "offline"
	status := p.Status
	custom := strings.TrimSpace(p.CustomStatus)
	updated, err := s.users.UpdateProfile(ctx, u.ID, &auth.ProfileUpdate{
		Presence: &auth.PresenceUpdate{Online: &online, Status: &status, CustomStatus: &custom},
	})
	if err != nil {
		logger.Err("federation", err, map[string]any{"user_id": u.ID})
		return false
	}
	if updated != nil && s.redis != nil {
		stargate.PublishPresenceUpdate(ctx, s.redis, s.cfg.Stargate.Region, updated)
	}
	return true
}

// mirrorSpec builds a whole mirror from a snapshot (allocating local room ids), for a
// new mirror (spaceID 0) or an existing one being reconciled.
func (s *Service) mirrorSpec(ctx context.Context, origin string, snap *SpaceSnapshot, spaceID int64) (*spaces.MirrorSpec, error) {
	if len(snap.Rooms) > maxSnapshotRooms || len(snap.Roles) > maxSnapshotRoles || len(snap.Members) > maxMirrorMembers {
		return nil, ErrPayloadTooLarge
	}
	if spaceID == 0 {
		spaceID = id.Next()
	}
	spec := &spaces.MirrorSpec{Space: &spaces.Space{ID: spaceID}}
	for _, w := range snap.Emoji {
		e, err := s.emojiFromWire(ctx, w)
		if err != nil {
			continue
		}
		spec.Emoji = append(spec.Emoji, e)
	}
	// Members first: the owner and user overrides refer to them.
	for _, w := range snap.Members {
		m, err := s.mirrorMember(ctx, origin, w)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": origin, "fid": w.User.FID})
			continue
		}
		spec.Members = append(spec.Members, m)
	}
	for _, w := range snap.Roles {
		r, err := s.roleFromWire(ctx, w)
		if err != nil {
			continue
		}
		spec.Roles = append(spec.Roles, *r)
	}
	// Allocate every room before resolving parents and settings that point at rooms.
	for _, w := range snap.Rooms {
		if oid, err := id.Parse(w.ID); err == nil {
			if _, err := s.localRoomID(ctx, origin, oid, true); err != nil {
				return nil, err
			}
		}
	}
	for _, w := range snap.Rooms {
		mr, err := s.mirrorRoom(ctx, origin, w, false)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": origin, "room": w.ID})
			continue
		}
		spec.Rooms = append(spec.Rooms, mr)
	}
	spec.MembersComplete = snap.MembersNext == ""
	s.applySpaceInfo(ctx, spec.Space, origin, snap.Info)
	return spec, nil
}

// ---- outbound (spaces.Federator) -------------------------------------------------------

// ensureHosted registers a space hosted here as federating (its global identity is its
// own id), so relays and refs have a mapping to read.
func (s *Service) ensureHosted(ctx context.Context, spaceID int64) {
	if m, err := s.repo.GetSpaceMapping(ctx, spaceID); err == nil && m != nil {
		return
	}
	if err := s.repo.PutSpaceMapping(ctx, &SpaceMapping{SpaceID: spaceID, OriginDomain: s.fcfg.Domain, OriginSpaceID: spaceID}); err != nil {
		logger.Err("federation", err, map[string]any{"space_id": spaceID})
	}
}

func (s *Service) AfterSpaceUpdated(ctx context.Context, sp *spaces.Space) {
	peers := s.spacePeers(ctx, sp.ID)
	if len(peers) == 0 {
		return
	}
	s.fanOut(ctx, peers, http.MethodPost, "/spaces/update", SpaceUpdateEvent{Space: s.spaceRef(ctx, sp.ID), Info: s.spaceInfo(ctx, sp)})
}

func (s *Service) AfterSpaceDeleted(ctx context.Context, sp *spaces.Space) {
	peers := s.spacePeers(ctx, sp.ID)
	if len(peers) > 0 {
		s.fanOut(ctx, peers, http.MethodPost, "/spaces/delete", SpaceDeleteEvent{Space: s.spaceRef(ctx, sp.ID)})
	}
	s.forgetSpace(ctx, sp.ID)
}

// forgetSpace drops every mapping of a space about to be deleted here.
func (s *Service) forgetSpace(ctx context.Context, spaceID int64) {
	if rows, err := s.roomRepo.ListBySpace(ctx, spaceID); err == nil {
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.RoomID)
		}
		if ms, err := s.repo.GetRoomMappings(ctx, ids); err == nil {
			for _, m := range ms {
				_ = s.repo.DeleteRoomMapping(ctx, m)
			}
		}
	}
	_ = s.repo.DeleteSpacePeers(ctx, spaceID)
	if m, err := s.repo.GetSpaceMapping(ctx, spaceID); err == nil && m != nil {
		_ = s.repo.DeleteSpaceMapping(ctx, m)
	}
}

func (s *Service) AfterSpaceMemberAdded(ctx context.Context, spaceID int64, m *spaces.SpaceMember) {
	u, err := s.users.GetByID(ctx, m.UserID)
	if err != nil || u == nil {
		return
	}
	if u.IsRemote() && s.fcfg.IsAllowedPeer(u.HomeDomain) {
		s.ensureHosted(ctx, spaceID)
		s.addSpacePeer(ctx, spaceID, u.HomeDomain)
	}
	peers := s.spacePeers(ctx, spaceID)
	if len(peers) == 0 {
		return
	}
	s.fanOut(ctx, peers, http.MethodPost, "/spaces/members", SpaceMemberEvent{Space: s.spaceRef(ctx, spaceID), Action: "add", Member: s.memberWire(m, u)})
}

func (s *Service) AfterSpaceMemberUpdated(ctx context.Context, spaceID int64, m *spaces.SpaceMember) {
	peers := s.spacePeers(ctx, spaceID)
	if len(peers) == 0 {
		return
	}
	u, err := s.users.GetByID(ctx, m.UserID)
	if err != nil || u == nil {
		return
	}
	s.fanOut(ctx, peers, http.MethodPost, "/spaces/members", SpaceMemberEvent{Space: s.spaceRef(ctx, spaceID), Action: "update", Member: s.memberWire(m, u)})
}

func (s *Service) AfterSpaceMemberRemoved(ctx context.Context, spaceID, userID int64, how string) {
	u, err := s.users.GetByID(ctx, userID)
	if err != nil || u == nil {
		return
	}
	peers := s.spacePeers(ctx, spaceID)
	if len(peers) > 0 {
		ev := SpaceMemberEvent{Space: s.spaceRef(ctx, spaceID), Action: "remove", Member: SpaceMemberWire{User: s.ProfileOf(u), Roles: []string{}}, How: how}
		s.fanOut(ctx, peers, http.MethodPost, "/spaces/members", ev)
	}
	if !u.IsRemote() {
		return
	}
	// The last member from that instance leaving means it no longer mirrors the space.
	members, err := s.spaceSvc.Members(ctx, spaceID)
	if err != nil {
		return
	}
	for _, m := range members {
		if m.User.IsRemote() && m.User.HomeDomain == u.HomeDomain {
			return
		}
	}
	s.removeSpacePeer(ctx, spaceID, u.HomeDomain)
}

func (s *Service) AfterSpaceRoleChanged(ctx context.Context, spaceID int64, role *spaces.SpaceRole, deleted bool) {
	peers := s.spacePeers(ctx, spaceID)
	if len(peers) == 0 {
		return
	}
	action := "upsert"
	if deleted {
		action = "delete"
	}
	s.fanOut(ctx, peers, http.MethodPost, "/spaces/roles", SpaceRoleEvent{Space: s.spaceRef(ctx, spaceID), Action: action, Role: s.roleWire(ctx, role)})
}

func (s *Service) AfterSpaceRoomChanged(ctx context.Context, spaceID int64, room *rooms.Room, overrides spaces.RoomOverrides, deleted bool) {
	peers := s.spacePeers(ctx, spaceID)
	if len(peers) == 0 {
		return
	}
	ev := SpaceRoomEvent{Space: s.spaceRef(ctx, spaceID), Action: "upsert"}
	if deleted {
		ev.Action = "delete"
		ev.Room = &SpaceRoomWire{ID: id.Format(room.ID)}
	} else {
		// Register the room's identity so later message relays reference it the same way.
		if _, err := s.roomRef(ctx, room.ID); err != nil {
			return
		}
		w := s.roomWire(ctx, room, overrides)
		ev.Room = &w
	}
	s.fanOut(ctx, peers, http.MethodPost, "/spaces/rooms", ev)
}

func (s *Service) AfterSpaceRoomsReordered(ctx context.Context, spaceID int64, list []*rooms.Room) {
	peers := s.spacePeers(ctx, spaceID)
	if len(peers) == 0 {
		return
	}
	ev := SpaceRoomEvent{Space: s.spaceRef(ctx, spaceID), Action: "reorder"}
	for _, r := range list {
		ev.Order = append(ev.Order, SpaceRoomOrder{ID: id.Format(r.ID), Parent: fmtOptID(r.ParentID), Position: r.Position})
	}
	s.fanOut(ctx, peers, http.MethodPost, "/spaces/rooms", ev)
}

// ---- outbound (mirror → origin) --------------------------------------------------------

// spaceOriginError turns a failed call to a space's origin into what the member sees.
func spaceOriginError(err error, origin, path string) error {
	var se *StatusError
	if errors.As(err, &se) {
		return &spaces.OriginError{Status: se.Status, Message: peerErrorMessage(se.Body)}
	}
	if errors.Is(err, ErrPeerNotAllowed) {
		return &spaces.OriginError{Status: http.StatusForbidden, Message: ErrPeerNotAllowed.Error()}
	}
	logger.Err("federation", err, map[string]any{"peer": origin, "path": path})
	return spaces.ErrOriginUnavailable
}

func (s *Service) PreviewRemoteInvite(ctx context.Context, domain, code string) (*spaces.InvitePreview, error) {
	var reply SpaceInvitePreviewReply
	path := "/spaces/invites/" + url.PathEscape(code)
	if _, err := s.client.Do(ctx, domain, http.MethodGet, path, nil, &reply); err != nil {
		return nil, spaceOriginError(err, domain, path)
	}
	originSpaceID, err := id.Parse(reply.Space.OriginSpaceID)
	if err != nil || reply.Space.OriginDomain != domain {
		return nil, spaces.ErrOriginUnavailable
	}
	sp := &spaces.Space{
		ID:          originSpaceID,
		Name:        clampText(reply.Name, spaces.MaxSpaceNameRunes),
		NameAcronym: clampText(reply.NameAcronym, 4),
		Description: clampText(reply.Description, spaces.MaxSpaceDescriptionRunes),
		Icon:        httpURLOnly(reply.Icon),
		Banner:      httpURLOnly(reply.Banner),
		Federation:  &spaces.SpaceFederation{OriginDomain: domain, OriginID: reply.Space.OriginSpaceID},
	}
	// Already mirrored here (someone local is in it): use the local id, so a client can
	// tell it already has the space.
	if m, err := s.repo.GetSpaceByOrigin(ctx, domain, originSpaceID); err == nil && m != nil {
		sp.ID = m.SpaceID
	}
	return &spaces.InvitePreview{Space: sp, InviterName: clampText(reply.Inviter, 32), MemberCount: reply.MemberCount}, nil
}

func (s *Service) JoinRemoteSpace(ctx context.Context, domain, code string, user *auth.User) (*spaces.Space, error) {
	var snap SpaceSnapshot
	presence := auth.ToPublicPresence(user.Presence, true)
	req := SpaceJoinRequest{Code: code, User: s.ProfileOf(user), Presence: &presence}
	if _, err := s.client.Do(ctx, domain, http.MethodPost, "/spaces/join", req, &snap); err != nil {
		return nil, spaceOriginError(err, domain, "/spaces/join")
	}
	originSpaceID, err := id.Parse(snap.Space.OriginSpaceID)
	if err != nil || snap.Space.OriginDomain != domain {
		return nil, spaces.ErrOriginUnavailable
	}
	self := s.LocalFID(user.ID)
	var me *spaces.MirrorMember
	for _, w := range snap.Members {
		if w.User.FID == self {
			m, err := s.mirrorMember(ctx, domain, w)
			if err == nil {
				me = &m
			}
			break
		}
	}
	if me == nil {
		me = &spaces.MirrorMember{UserID: user.ID, JoinedAt: time.Now().UTC()}
		if rid, err := id.Parse(snap.Info.EveryoneRole); err == nil {
			me.RoleIDs = []int64{rid}
		}
	}
	if m, err := s.repo.GetSpaceByOrigin(ctx, domain, originSpaceID); err == nil && m != nil {
		// Someone here already mirrors it: just add this member.
		if err := s.spaceSvc.AddMirrorMember(ctx, m.SpaceID, *me); err != nil {
			return nil, err
		}
		s.storeMirrorPeers(ctx, m.SpaceID, domain, snap.Peers)
		return s.spaceSvc.GetSpace(ctx, m.SpaceID)
	}
	s.completeSnapshot(ctx, domain, &snap)
	spec, err := s.mirrorSpec(ctx, domain, &snap, 0)
	if err != nil {
		return nil, err
	}
	if err := s.spaceSvc.CreateMirror(ctx, spec); err != nil {
		return nil, err
	}
	if err := s.repo.PutSpaceMapping(ctx, &SpaceMapping{SpaceID: spec.Space.ID, OriginDomain: domain, OriginSpaceID: originSpaceID}); err != nil {
		return nil, err
	}
	s.storeMirrorPeers(ctx, spec.Space.ID, domain, snap.Peers)
	s.spaceSvc.AnnounceMirrorMember(ctx, spec.Space.ID, user.ID)
	s.touchPeer(ctx, domain)
	return spec.Space, nil
}

func (s *Service) LeaveRemoteSpace(ctx context.Context, origin string, spaceID int64, user *auth.User) error {
	m, err := s.repo.GetSpaceMapping(ctx, spaceID)
	if err != nil {
		return err
	}
	if m == nil {
		return spaces.ErrSpaceNotFound
	}
	ref := SpaceRef{OriginDomain: m.OriginDomain, OriginSpaceID: id.Format(m.OriginSpaceID)}
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/leave", SpaceUserRequest{Space: ref, User: s.FIDOf(user)}, nil); err != nil {
		var se *StatusError
		// Gone there already (not a member any more): fall through and drop it here too.
		if !errors.As(err, &se) || se.Status != http.StatusNotFound {
			return spaceOriginError(err, origin, "/spaces/leave")
		}
	}
	if err := s.spaceSvc.RemoveMirrorMember(ctx, spaceID, user.ID); err != nil {
		return err
	}
	s.dropMirrorIfEmpty(ctx, m)
	return nil
}

// dropMirrorIfEmpty removes a mirror nobody here is a member of any more.
func (s *Service) dropMirrorIfEmpty(ctx context.Context, m *SpaceMapping) {
	n, err := s.spaceSvc.LocalMemberCount(ctx, m.SpaceID)
	if err != nil || n > 0 {
		return
	}
	s.dropMirror(ctx, m)
}

func (s *Service) dropMirror(ctx context.Context, m *SpaceMapping) {
	// Room mappings first: the cascade deletes the rooms_by_space rows they are found by.
	s.forgetSpace(ctx, m.SpaceID)
	if err := s.spaceSvc.DeleteMirror(ctx, m.SpaceID); err != nil {
		logger.Err("federation", err, map[string]any{"space_id": m.SpaceID})
	}
}

func (s *Service) CreateRemoteInvite(ctx context.Context, origin string, spaceID int64, user *auth.User, in *spaces.CreateInviteInput) (*spaces.SpaceInvite, error) {
	m, err := s.repo.GetSpaceMapping(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, spaces.ErrSpaceNotFound
	}
	req := SpaceInviteRequest{Space: SpaceRef{OriginDomain: m.OriginDomain, OriginSpaceID: id.Format(m.OriginSpaceID)}, User: s.FIDOf(user)}
	if in != nil {
		req.MaxAgeSeconds, req.MaxUses = in.MaxAgeSeconds, in.MaxUses
	}
	var reply SpaceInviteReply
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/invites", req, &reply); err != nil {
		return nil, spaceOriginError(err, origin, "/spaces/invites")
	}
	return &spaces.SpaceInvite{
		Code: spaces.FormatInviteCode(reply.Code, origin), SpaceID: spaceID, InviterID: user.ID,
		MaxUses: reply.MaxUses, CurrentUses: reply.CurrentUses, ExpiresAt: reply.ExpiresAt, CreatedAt: reply.CreatedAt,
	}, nil
}

// ---- messages in channels hosted elsewhere (messages.Federator) ------------------------

func (s *Service) RemoteOrigin(ctx context.Context, room *rooms.Room) string {
	if room == nil || room.SpaceID == nil {
		return ""
	}
	m, err := s.repo.GetSpaceMapping(ctx, *room.SpaceID)
	if err != nil || m == nil || s.IsLocalServer(m.OriginDomain) {
		return ""
	}
	return m.OriginDomain
}

func messageOriginError(err error, origin, path string) error {
	var se *StatusError
	if errors.As(err, &se) {
		return &messages.OriginError{Status: se.Status, Message: peerErrorMessage(se.Body)}
	}
	logger.Err("federation", err, map[string]any{"peer": origin, "path": path})
	return messages.ErrOriginUnavailable
}

// mirrorScope is the roomScope of a mirrored channel for applying the origin's answers.
func (s *Service) mirrorScope(ctx context.Context, room *rooms.Room) (*roomScope, RoomRef, error) {
	m, err := s.repo.GetRoomMapping(ctx, room.ID)
	if err != nil {
		return nil, RoomRef{}, err
	}
	if m == nil {
		return nil, RoomRef{}, rooms.ErrRoomNotFound
	}
	sc, err := s.roomScope(ctx, m)
	if err != nil {
		return nil, RoomRef{}, err
	}
	return sc, RoomRef{OriginDomain: m.OriginDomain, OriginRoomID: id.Format(m.OriginRoomID)}, nil
}

func (s *Service) CreateRemote(ctx context.Context, origin string, room *rooms.Room, userID int64, in *messages.CreateMessageInput, attachments []messages.Attachment) (*messages.Message, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return nil, messages.ErrNotParticipant
	}
	sc, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return nil, err
	}
	req := SpaceMessageRequest{
		Room: ref, User: s.FIDOf(user), Ciphertext: in.Ciphertext, Plaintext: s.textToWire(ctx, in.Plaintext),
		Emojis:       s.emojiRefs(ctx, in.Plaintext),
		MentionRoles: in.MentionRoles, MentionEveryone: in.MentionEveryone, Attachments: attachments,
	}
	if in.SenderDeviceID != 0 {
		req.SenderDeviceID = id.Format(int64(in.SenderDeviceID))
	}
	if in.ReplyToID != nil {
		r := s.messageRef(ctx, room.ID, int64(*in.ReplyToID))
		req.ReplyTo = &r
	}
	for _, raw := range in.Mentions {
		if uid, err := id.Parse(raw); err == nil {
			if u, _ := s.users.GetByID(ctx, uid); u != nil {
				req.Mentions = append(req.Mentions, s.FIDOf(u))
			}
		}
	}
	var reply MessageEvent
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/messages", req, &reply); err != nil {
		return nil, messageOriginError(err, origin, "/spaces/messages")
	}
	return s.applyMessageEvent(ctx, origin, sc, reply)
}

func (s *Service) EditRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64, in *messages.EditMessageInput) (*messages.Message, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return nil, messages.ErrNotParticipant
	}
	_, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return nil, err
	}
	req := SpaceMessageEditRequest{
		Room: ref, Message: s.messageRef(ctx, room.ID, msgID), User: s.FIDOf(user),
		Ciphertext: in.Ciphertext, Plaintext: s.textToWire(ctx, in.Plaintext), Emojis: s.emojiRefs(ctx, in.Plaintext),
	}
	var reply MessageEvent
	if _, err := s.client.Do(ctx, origin, http.MethodPatch, "/spaces/messages", req, &reply); err != nil {
		return nil, messageOriginError(err, origin, "/spaces/messages")
	}
	s.recordEmojiRefs(ctx, reply.Emojis)
	updated, err := s.msgSvc.EditFederated(ctx, room.ID, msgID, reply.Ciphertext, s.textFromWire(ctx, reply.Plaintext))
	if errors.Is(err, messages.ErrMessageNotFound) {
		// Edited something this instance never stored (pre-join history): hand back the
		// origin's copy as-is.
		sc, _, serr := s.mirrorScope(ctx, room)
		if serr != nil {
			return nil, serr
		}
		return s.messageFromEvent(ctx, origin, sc, reply)
	}
	return updated, err
}

func (s *Service) DeleteRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64) error {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return messages.ErrNotParticipant
	}
	_, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return err
	}
	req := SpaceMessageRequestRef{Room: ref, Message: s.messageRef(ctx, room.ID, msgID), User: s.FIDOf(user)}
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/messages/delete", req, nil); err != nil {
		return messageOriginError(err, origin, "/spaces/messages/delete")
	}
	return s.msgSvc.DeleteFederated(ctx, room.ID, msgID)
}

func (s *Service) ReactRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64, emoji string, remove bool) ([]messages.ReactionSummary, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return nil, messages.ErrNotParticipant
	}
	_, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return nil, err
	}
	path := "/spaces/reactions"
	if remove {
		path = "/spaces/reactions/delete"
	}
	req := ReactionEvent{Room: ref, Message: s.messageRef(ctx, room.ID, msgID), User: s.FIDOf(user), Emoji: emoji, EmojiRef: s.emojiRefFor(ctx, emoji)}
	var reply SpaceReactionReply
	if _, err := s.client.Do(ctx, origin, http.MethodPost, path, req, &reply); err != nil {
		return nil, messageOriginError(err, origin, path)
	}
	// Keep the local copy in step (the origin tells everyone else); a message not stored
	// here just has nothing to update.
	if remove {
		_ = s.msgSvc.RemoveReactionFederated(ctx, room.ID, msgID, userID, emoji)
	} else {
		_ = s.msgSvc.AddReactionFederated(ctx, room.ID, msgID, userID, emoji)
	}
	if reply.Reactions == nil {
		reply.Reactions = []messages.ReactionSummary{}
	}
	return reply.Reactions, nil
}

// PinRemote asks a channel's origin to pin or unpin as the local member: the origin checks the
// permission, posts the notice and tells every other mirror; this instance applies the
// origin's answer to its own copy.
func (s *Service) PinRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64, remove bool) error {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return messages.ErrNotParticipant
	}
	_, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return err
	}
	req := PinEvent{Room: ref, Message: s.messageRef(ctx, room.ID, msgID), User: s.FIDOf(user), PinnedAt: time.Now().UTC(), Remove: remove}
	var reply PinEvent
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/pins", req, &reply); err != nil {
		return messageOriginError(err, origin, "/spaces/pins")
	}
	at := reply.PinnedAt
	if at.IsZero() {
		at = req.PinnedAt
	}
	return s.msgSvc.PinFederated(ctx, room.ID, msgID, userID, at, remove)
}

// ListPinsRemote reads a hosted channel's pin list from its origin as the local member, and
// keeps a copy of the messages like history does.
func (s *Service) ListPinsRemote(ctx context.Context, origin string, room *rooms.Room, userID int64) ([]messages.PinnedMessage, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return nil, messages.ErrNotParticipant
	}
	sc, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return nil, err
	}
	var reply PinsReply
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/pins/list", PinsQuery{Room: ref, User: s.FIDOf(user)}, &reply); err != nil {
		return nil, messageOriginError(err, origin, "/spaces/pins/list")
	}
	out := make([]messages.PinnedMessage, 0, len(reply.Items))
	for _, it := range reply.Items {
		m, err := s.messageFromEvent(ctx, origin, sc, it.Message)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": origin, "message": it.Message.Message.OriginMessageID})
			continue
		}
		s.storeHistory(ctx, sc, m)
		var pinnedBy int64
		if it.PinnedBy != "" {
			if uid, err := s.ResolveLocalID(ctx, it.PinnedBy); err == nil {
				pinnedBy = uid
			}
		}
		out = append(out, messages.PinnedMessage{
			Pin:       messages.Pin{RoomID: room.ID, MessageID: m.ID, PinnedBy: pinnedBy, PinnedAt: it.PinnedAt},
			Message:   *m,
			Reactions: it.Message.Reactions,
		})
	}
	return out, nil
}

func (s *Service) ListRemote(ctx context.Context, origin string, room *rooms.Room, userID int64, beforeID *int64, limit int) ([]messages.Message, map[int64][]messages.ReactionSummary, error) {
	q := SpaceMessagesQuery{Limit: limit}
	if beforeID != nil {
		q.Before = id.Format(*beforeID)
	}
	return s.historyRemote(ctx, origin, room, userID, q)
}

// ListAfterRemote / ListAroundRemote: the other two history cursors (jump-to-message and
// reading downward out of a jumped-to window), served by the origin like ListRemote.
func (s *Service) ListAfterRemote(ctx context.Context, origin string, room *rooms.Room, userID, afterID int64, limit int) ([]messages.Message, map[int64][]messages.ReactionSummary, error) {
	return s.historyRemote(ctx, origin, room, userID, SpaceMessagesQuery{After: id.Format(afterID), Limit: limit})
}

func (s *Service) ListAroundRemote(ctx context.Context, origin string, room *rooms.Room, userID, aroundID int64, limit int) ([]messages.Message, map[int64][]messages.ReactionSummary, error) {
	return s.historyRemote(ctx, origin, room, userID, SpaceMessagesQuery{Around: id.Format(aroundID), Limit: limit})
}

// historyRemote asks the origin for a page of a hosted channel as the local member, and
// keeps a copy of what came back.
func (s *Service) historyRemote(ctx context.Context, origin string, room *rooms.Room, userID int64, req SpaceMessagesQuery) ([]messages.Message, map[int64][]messages.ReactionSummary, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return nil, nil, messages.ErrNotParticipant
	}
	sc, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return nil, nil, err
	}
	req.Room = ref
	req.User = s.FIDOf(user)
	var reply SpaceMessagesReply
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/messages/list", req, &reply); err != nil {
		return nil, nil, messageOriginError(err, origin, "/spaces/messages/list")
	}
	out := make([]messages.Message, 0, len(reply.Messages))
	reactions := make(map[int64][]messages.ReactionSummary, len(reply.Messages))
	for _, ev := range reply.Messages {
		m, err := s.messageFromEvent(ctx, origin, sc, ev)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": origin, "message": ev.Message.OriginMessageID})
			continue
		}
		// Keep a copy: search, and reading while the origin is down, work from it.
		s.storeHistory(ctx, sc, m)
		out = append(out, *m)
		if len(ev.Reactions) > 0 {
			reactions[m.ID] = ev.Reactions
		}
	}
	return out, reactions, nil
}

func (s *Service) GetRemote(ctx context.Context, origin string, room *rooms.Room, userID, msgID int64) (*messages.Message, []messages.ReactionSummary, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return nil, nil, messages.ErrNotParticipant
	}
	sc, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return nil, nil, err
	}
	req := SpaceMessageRequestRef{Room: ref, Message: s.messageRef(ctx, room.ID, msgID), User: s.FIDOf(user)}
	var reply MessageEvent
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/messages/get", req, &reply); err != nil {
		return nil, nil, messageOriginError(err, origin, "/spaces/messages/get")
	}
	m, err := s.messageFromEvent(ctx, origin, sc, reply)
	if err != nil {
		return nil, nil, err
	}
	s.storeHistory(ctx, sc, m)
	return m, reply.Reactions, nil
}

// storeHistory keeps a message read from the origin, without the bookkeeping a new
// message gets (it is history, not news).
func (s *Service) storeHistory(ctx context.Context, sc *roomScope, m *messages.Message) {
	if existing, err := s.msgRepo.GetByID(ctx, sc.room.ID, m.ID); err == nil && existing != nil {
		return
	}
	if err := s.msgRepo.Insert(ctx, m); err != nil {
		logger.Err("federation", err, map[string]any{"room_id": sc.room.ID, "message_id": m.ID})
		return
	}
	_ = s.repo.PutMessageMapping(ctx, &MessageMapping{RoomID: sc.room.ID, MessageID: m.ID, OriginDomain: sc.mapping.OriginDomain, OriginMessageID: m.ID})
}

// ---- inbound: a mirror's member asks the origin ------------------------------------------

// hostedSpace resolves a space ref to a space hosted here that the requesting instance
// mirrors.
func (h *Handler) hostedSpace(c fiber.Ctx, ref SpaceRef) (*spaces.Space, error) {
	if !h.svc.IsLocalServer(ref.OriginDomain) {
		return nil, errNotSpaceOrigin
	}
	spaceID, err := id.Parse(ref.OriginSpaceID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	ok, err := h.svc.repo.IsSpacePeer(c.Context(), spaceID, RequesterDomain(c))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrPeerNotAllowed
	}
	sp, err := h.svc.spaceSvc.GetSpace(c.Context(), spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, spaces.ErrSpaceNotFound
	}
	return sp, nil
}

// spaceActor resolves the requesting instance's user named by fid (a shadow here).
func (h *Handler) spaceActor(c fiber.Ctx, fid string) (*auth.User, error) {
	if _, domain, err := ParseFID(fid); err != nil || domain != RequesterDomain(c) {
		return nil, errForeignUser
	}
	uid, err := h.svc.ResolveLocalID(c.Context(), fid)
	if err != nil {
		return nil, err
	}
	u, err := h.svc.users.GetByID(c.Context(), uid)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrRemoteUserNotFound
	}
	return u, nil
}

func spaceFail(c fiber.Ctx, err error, fields map[string]any) error {
	// A member elsewhere gets the same status and reason a local member would.
	if status, msg, ok := spaces.HTTPError(err); ok {
		return c.Status(status).JSON(fiber.Map{"error": msg})
	}
	switch {
	case errors.Is(err, errNotSpaceOrigin), errors.Is(err, errSpaceNotMirror):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, errForeignUser), errors.Is(err, errNotInRoom):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, messages.ErrNotParticipant):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a member of that space"})
	case errors.Is(err, messages.ErrForbidden):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission in that channel"})
	case errors.Is(err, messages.ErrMessageNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	case errors.Is(err, messages.ErrInvalidInput):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "plaintext required when E2EE is off; ciphertext required when E2EE is on"})
	case errors.Is(err, messages.ErrContentTooLong), errors.Is(err, messages.ErrTooManyMentions), errors.Is(err, messages.ErrTooManyAttachments),
		errors.Is(err, messages.ErrInvalidReaction), errors.Is(err, messages.ErrTooManyReactions), errors.Is(err, messages.ErrTooManyPins):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	var slow *messages.SlowmodeError
	if errors.As(err, &slow) {
		secs := int(slow.RetryAfter.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		c.Set("Retry-After", strconv.Itoa(secs))
		return c.Status(http.StatusTooManyRequests).JSON(fiber.Map{"error": "slowmode is on in this channel", "retry_after": secs})
	}
	return fail(c, err, fields)
}

// SpaceInvitePreview GET /spaces/invites/:code - what a local invite code leads to.
func (h *Handler) SpaceInvitePreview(c fiber.Ctx) error {
	code, err := url.PathUnescape(c.Params("code"))
	if err != nil || strings.ContainsRune(code, '@') {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": spaces.ErrInviteNotFound.Error()})
	}
	preview, err := h.svc.spaceSvc.GetInvitePreview(c.Context(), code)
	if err != nil {
		return spaceFail(c, err, map[string]any{"code": code})
	}
	sp := preview.Space
	return c.JSON(SpaceInvitePreviewReply{
		Space: h.svc.spaceRef(c.Context(), sp.ID), Name: sp.Name, NameAcronym: sp.NameAcronym, Description: sp.Description,
		Icon: sp.Icon, Banner: sp.Banner, MemberCount: preview.MemberCount, Inviter: preview.InviterName,
	})
}

// SpaceJoin POST /spaces/join - a user on the requesting instance redeems an invite.
// The answer is the whole space, so the requester can build its mirror in one step.
func (h *Handler) SpaceJoin(c fiber.Ctx) error {
	var body SpaceJoinRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	if _, domain, err := ParseFID(body.User.FID); err != nil || domain != requester {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": errForeignUser.Error()})
	}
	if strings.ContainsRune(body.Code, '@') {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": spaces.ErrInviteNotFound.Error()})
	}
	ctx := WithExcludedPeer(c.Context(), requester)
	user, err := h.svc.EnsureShadow(ctx, requester, body.User)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	if body.Presence != nil {
		// Their status before the member is announced, so every mirror starts right.
		h.svc.applyRelayedPresence(ctx, user, *body.Presence)
	}
	sp, err := h.svc.spaceSvc.JoinByInvite(ctx, user.ID, body.Code)
	if err != nil {
		return spaceFail(c, err, map[string]any{"peer": requester, "code": body.Code})
	}
	// The requester is a peer of this space from now on, whether or not the hook that
	// normally records it ran (an already-member rejoin skips it).
	h.svc.ensureHosted(ctx, sp.ID)
	h.svc.addSpacePeer(ctx, sp.ID, requester)
	snap, err := h.svc.snapshot(ctx, sp)
	if err != nil {
		return fail(c, err, map[string]any{"space_id": sp.ID})
	}
	return c.JSON(snap)
}

// SpaceLeave POST /spaces/leave.
func (h *Handler) SpaceLeave(c fiber.Ctx) error {
	var body SpaceUserRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sp, err := h.hostedSpace(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	actor, err := h.spaceActor(c, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	ctx := WithExcludedPeer(c.Context(), RequesterDomain(c))
	if err := h.svc.spaceSvc.LeaveSpace(ctx, actor.ID, sp.ID); err != nil {
		if errors.Is(err, spaces.ErrNotMember) {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
		}
		return spaceFail(c, err, map[string]any{"space_id": sp.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// SpaceInviteCreate POST /spaces/invites.
func (h *Handler) SpaceInviteCreate(c fiber.Ctx) error {
	var body SpaceInviteRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sp, err := h.hostedSpace(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	actor, err := h.spaceActor(c, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	inv, err := h.svc.spaceSvc.CreateInvite(c.Context(), actor.ID, sp.ID, &spaces.CreateInviteInput{MaxAgeSeconds: body.MaxAgeSeconds, MaxUses: body.MaxUses})
	if err != nil {
		return spaceFail(c, err, map[string]any{"space_id": sp.ID})
	}
	return c.Status(http.StatusCreated).JSON(SpaceInviteReply{Code: inv.Code, MaxUses: inv.MaxUses, CurrentUses: inv.CurrentUses, ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt})
}

// hostedChannel resolves a room ref to a channel of a space hosted here that the
// requesting instance mirrors, plus the acting member.
func (h *Handler) hostedChannel(c fiber.Ctx, ref RoomRef, fid string) (*roomScope, *auth.User, error) {
	sc, err := h.roomFor(c, ref)
	if err != nil {
		return nil, nil, err
	}
	if sc.spaceID == 0 || !sc.hostedHere {
		return nil, nil, errNotSpaceOrigin
	}
	actor, err := h.spaceActor(c, fid)
	if err != nil {
		return nil, nil, err
	}
	return sc, actor, nil
}

// SpaceMessageCreate POST /spaces/messages.
func (h *Handler) SpaceMessageCreate(c fiber.Ctx) error {
	var body SpaceMessageRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, actor, err := h.hostedChannel(c, body.Room, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	ctx := WithExcludedPeer(c.Context(), RequesterDomain(c))
	h.svc.recordEmojiRefs(ctx, body.Emojis)
	in := &messages.CreateMessageInput{
		Ciphertext: body.Ciphertext, Plaintext: h.svc.textFromWire(ctx, body.Plaintext), MentionEveryone: body.MentionEveryone,
	}
	if body.SenderDeviceID != "" {
		if did, err := id.Parse(body.SenderDeviceID); err == nil {
			in.SenderDeviceID = messages.SnowflakeID(did)
		}
	}
	if body.ReplyTo != nil {
		if rid := h.localMessageID(c, sc.room.ID, *body.ReplyTo); rid != 0 {
			r := messages.SnowflakeID(rid)
			in.ReplyToID = &r
		}
	}
	for _, fid := range body.Mentions {
		if len(in.Mentions) >= messages.MaxMentionsPerMessage {
			break
		}
		if uid, err := h.svc.ResolveLocalID(ctx, fid); err == nil {
			in.Mentions = append(in.Mentions, id.Format(uid))
		}
	}
	for _, raw := range body.MentionRoles {
		if len(in.MentionRoles) >= messages.MaxMentionsPerMessage {
			break
		}
		if rid, err := id.Parse(raw); err == nil {
			in.MentionRoles = append(in.MentionRoles, id.Format(rid))
		}
	}
	m, err := h.svc.msgSvc.CreateFromRemote(ctx, actor.ID, sc.room.ID, in, sanitizeAttachments(body.Attachments))
	if err != nil {
		return spaceFail(c, err, map[string]any{"room_id": sc.room.ID})
	}
	return c.Status(http.StatusCreated).JSON(h.svc.messageEvent(ctx, body.Room, m, nil))
}

// SpaceMessageEdit PATCH /spaces/messages.
func (h *Handler) SpaceMessageEdit(c fiber.Ctx) error {
	var body SpaceMessageEditRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, actor, err := h.hostedChannel(c, body.Room, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	msgID := h.localMessageID(c, sc.room.ID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	ctx := WithExcludedPeer(c.Context(), RequesterDomain(c))
	h.svc.recordEmojiRefs(ctx, body.Emojis)
	updated, err := h.svc.msgSvc.Edit(ctx, actor.ID, sc.room.ID, msgID, &messages.EditMessageInput{Ciphertext: body.Ciphertext, Plaintext: h.svc.textFromWire(ctx, body.Plaintext)})
	if err != nil {
		if errors.Is(err, messages.ErrForbidden) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "cannot edit this message"})
		}
		return spaceFail(c, err, map[string]any{"room_id": sc.room.ID, "message_id": msgID})
	}
	return c.JSON(h.svc.messageEvent(ctx, body.Room, updated, nil))
}

// SpaceMessageDelete POST /spaces/messages/delete.
func (h *Handler) SpaceMessageDelete(c fiber.Ctx) error {
	var body SpaceMessageRequestRef
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, actor, err := h.hostedChannel(c, body.Room, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	msgID := h.localMessageID(c, sc.room.ID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	ctx := WithExcludedPeer(c.Context(), RequesterDomain(c))
	if err := h.svc.msgSvc.Delete(ctx, actor.ID, sc.room.ID, msgID); err != nil {
		if errors.Is(err, messages.ErrForbidden) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "cannot delete this message"})
		}
		return spaceFail(c, err, map[string]any{"room_id": sc.room.ID, "message_id": msgID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// spaceReaction serves POST /spaces/reactions and /spaces/reactions/delete.
func (h *Handler) spaceReaction(c fiber.Ctx, remove bool) error {
	var body ReactionEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, actor, err := h.hostedChannel(c, body.Room, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	msgID := h.localMessageID(c, sc.room.ID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	ctx := WithExcludedPeer(c.Context(), RequesterDomain(c))
	var summary []messages.ReactionSummary
	if remove {
		summary, err = h.svc.msgSvc.RemoveReaction(ctx, actor.ID, sc.room.ID, msgID, body.Emoji)
	} else {
		if body.EmojiRef != nil {
			h.svc.recordEmojiRefs(ctx, []EmojiRef{*body.EmojiRef})
		}
		summary, err = h.svc.msgSvc.AddReaction(ctx, actor.ID, sc.room.ID, msgID, body.Emoji)
	}
	if err != nil {
		return spaceFail(c, err, map[string]any{"room_id": sc.room.ID, "message_id": msgID})
	}
	if summary == nil {
		summary = []messages.ReactionSummary{}
	}
	return c.JSON(SpaceReactionReply{Reactions: summary})
}

func (h *Handler) SpaceReactionAdd(c fiber.Ctx) error    { return h.spaceReaction(c, false) }
func (h *Handler) SpaceReactionRemove(c fiber.Ctx) error { return h.spaceReaction(c, true) }

// SpacePin POST /spaces/pins: a mirror's member pins or unpins in a channel hosted here. The
// origin checks Manage Messages for them, posts the notice and relays to every other mirror;
// the reply is the event as applied, so the asking mirror records the same pinned_at.
func (h *Handler) SpacePin(c fiber.Ctx) error {
	var body PinEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, actor, err := h.hostedChannel(c, body.Room, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	msgID := h.localMessageID(c, sc.room.ID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	ctx := WithExcludedPeer(c.Context(), RequesterDomain(c))
	reply := PinEvent{Room: body.Room, Message: body.Message, User: body.User, Remove: body.Remove}
	if body.Remove {
		err = h.svc.msgSvc.Unpin(ctx, actor.ID, sc.room.ID, msgID)
	} else {
		reply.PinnedAt, err = h.svc.msgSvc.Pin(ctx, actor.ID, sc.room.ID, msgID)
	}
	if err != nil {
		if errors.Is(err, messages.ErrForbidden) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to pin messages in this channel"})
		}
		return spaceFail(c, err, map[string]any{"room_id": sc.room.ID, "message_id": msgID})
	}
	return c.JSON(reply)
}

// SpacePinsList POST /spaces/pins/list - a hosted channel's pin list as the member sees it.
func (h *Handler) SpacePinsList(c fiber.Ctx) error {
	var body PinsQuery
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, actor, err := h.hostedChannel(c, body.Room, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	ctx := c.Context()
	pins, err := h.svc.msgSvc.ListPins(ctx, actor.ID, sc.room.ID)
	if err != nil {
		return spaceFail(c, err, map[string]any{"room_id": sc.room.ID})
	}
	reply := PinsReply{Items: make([]PinItem, 0, len(pins))}
	for i := range pins {
		p := &pins[i]
		item := PinItem{PinnedAt: p.PinnedAt, Message: h.svc.messageEvent(ctx, body.Room, &p.Message, p.Reactions)}
		if u, _ := h.svc.users.GetByID(ctx, p.PinnedBy); u != nil {
			item.PinnedBy = h.svc.FIDOf(u)
		}
		reply.Items = append(reply.Items, item)
	}
	return c.JSON(reply)
}

// SpaceMessagesList POST /spaces/messages/list - a page of history as the member sees it.
func (h *Handler) SpaceMessagesList(c fiber.Ctx) error {
	var body SpaceMessagesQuery
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, actor, err := h.hostedChannel(c, body.Room, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	limit := body.Limit
	if limit <= 0 || limit > maxHistoryPage {
		limit = 50
	}
	ctx := c.Context()
	// The cursor ids are the origin's own (channels keep them everywhere), so they are
	// local ids here.
	cursor := func(raw string) *int64 {
		if raw == "" {
			return nil
		}
		v, err := id.Parse(raw)
		if err != nil {
			return nil
		}
		return &v
	}
	var (
		msgs      []messages.Message
		reactions map[int64][]messages.ReactionSummary
	)
	switch {
	case body.Around != "":
		if around := cursor(body.Around); around != nil {
			msgs, reactions, err = h.svc.msgSvc.ListAroundWithReactions(ctx, actor.ID, sc.room.ID, *around, limit)
		} else {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid around cursor"})
		}
	case body.After != "":
		if after := cursor(body.After); after != nil {
			msgs, reactions, err = h.svc.msgSvc.ListAfterWithReactions(ctx, actor.ID, sc.room.ID, *after, limit)
		} else {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid after cursor"})
		}
	default:
		msgs, reactions, err = h.svc.msgSvc.ListWithReactions(ctx, actor.ID, sc.room.ID, cursor(body.Before), limit)
	}
	if err != nil {
		return spaceFail(c, err, map[string]any{"room_id": sc.room.ID})
	}
	reply := SpaceMessagesReply{Messages: make([]MessageEvent, 0, len(msgs))}
	for i := range msgs {
		reply.Messages = append(reply.Messages, h.svc.messageEvent(ctx, body.Room, &msgs[i], reactions[msgs[i].ID]))
	}
	return c.JSON(reply)
}

// SpaceMessageGet POST /spaces/messages/get.
func (h *Handler) SpaceMessageGet(c fiber.Ctx) error {
	var body SpaceMessageRequestRef
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sc, actor, err := h.hostedChannel(c, body.Room, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	msgID := h.localMessageID(c, sc.room.ID, body.Message)
	if msgID == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	m, reactions, err := h.svc.msgSvc.Get(c.Context(), actor.ID, sc.room.ID, msgID)
	if err != nil {
		return spaceFail(c, err, map[string]any{"room_id": sc.room.ID, "message_id": msgID})
	}
	return c.JSON(h.svc.messageEvent(c.Context(), body.Room, m, reactions))
}

// ---- inbound: the origin tells a mirror ---------------------------------------------------

// mirrorFor resolves a space ref to the mirror of a space the requesting instance hosts.
func (h *Handler) mirrorFor(c fiber.Ctx, ref SpaceRef) (*SpaceMapping, error) {
	if ref.OriginDomain != RequesterDomain(c) {
		return nil, ErrPeerNotAllowed
	}
	originSpaceID, err := id.Parse(ref.OriginSpaceID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	m, err := h.svc.repo.GetSpaceByOrigin(c.Context(), ref.OriginDomain, originSpaceID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errSpaceNotMirror
	}
	return m, nil
}

// SpaceUpdate POST /spaces/update.
func (h *Handler) SpaceUpdate(c fiber.Ctx) error {
	var body SpaceUpdateEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, err := h.mirrorFor(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	if utf8.RuneCountInString(body.Info.Name) > spaces.MaxSpaceNameRunes*2 {
		return fail(c, ErrPayloadTooLarge, nil)
	}
	return h.applied(c, m, h.svc.applySpaceUpdate(c.Context(), m, body))
}

// applied answers a relay from the origin once applied.
func (h *Handler) applied(c fiber.Ctx, m *SpaceMapping, err error) error {
	if err != nil {
		if errors.Is(err, errUnknownAction) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		return spaceFail(c, err, map[string]any{"space_id": m.SpaceID, "peer": m.OriginDomain})
	}
	return c.SendStatus(http.StatusNoContent)
}

// SpaceMembers POST /spaces/members.
func (h *Handler) SpaceMembers(c fiber.Ctx) error {
	var body SpaceMemberEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, err := h.mirrorFor(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	return h.applied(c, m, h.svc.applySpaceMembers(c.Context(), m, body))
}

// SpaceRoles POST /spaces/roles.
func (h *Handler) SpaceRoles(c fiber.Ctx) error {
	var body SpaceRoleEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, err := h.mirrorFor(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	return h.applied(c, m, h.svc.applySpaceRoles(c.Context(), m, body))
}

// SpaceRooms POST /spaces/rooms.
func (h *Handler) SpaceRooms(c fiber.Ctx) error {
	var body SpaceRoomEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, err := h.mirrorFor(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	return h.applied(c, m, h.svc.applySpaceRooms(c.Context(), m, body))
}

// SpaceEmoji POST /spaces/emoji.
func (h *Handler) SpaceEmoji(c fiber.Ctx) error {
	var body SpaceEmojiEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, err := h.mirrorFor(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	return h.applied(c, m, h.svc.applySpaceEmoji(c.Context(), m, body))
}

// SpaceDelete POST /spaces/delete - the origin deleted the space.
func (h *Handler) SpaceDelete(c fiber.Ctx) error {
	var body SpaceDeleteEvent
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	m, err := h.mirrorFor(c, body.Space)
	if err != nil {
		if errors.Is(err, errSpaceNotMirror) {
			return c.SendStatus(http.StatusNoContent)
		}
		return spaceFail(c, err, nil)
	}
	h.svc.dropMirror(c.Context(), m)
	return c.SendStatus(http.StatusNoContent)
}
