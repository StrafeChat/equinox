package spaces

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// Spaces across instances.
//
// A space is hosted by the instance that created it - the origin - which stays the one
// authority for membership, roles, permissions, channels and moderation. Every other
// instance with a member keeps a *mirror*: a local spaces row, local room rows, the same
// roles (role ids are the origin's; they only need to be unique within the space), the
// same overrides and a member row per member (remote ones as shadow users), so READY,
// permission checks, unread state and the gateway all work unchanged for its users. The
// origin pushes every change to the mirrors; a member on a mirror writes (messages,
// reactions, invites, leaving) by asking the origin, which applies the write with the
// same checks a local member gets and then tells every mirror. Management of a mirrored
// space (roles, channels, kicks, settings) is refused here with ErrRemoteSpace until the
// request-forwarding covers it. internal/federation implements Federator; the mirror
// side of this file is what it calls into.

// SpaceFederation is a mirrored space's global identity: the instance hosting it and the
// id it has there. Absent on spaces this instance hosts.
type SpaceFederation struct {
	OriginDomain string `json:"origin_domain"`
	OriginID     string `json:"origin_id"`
}

// FederationInfo is the read-only side (the gateway's READY builder needs it too).
type FederationInfo interface {
	// SpaceFederation is non-nil only for a space hosted by another instance.
	SpaceFederation(ctx context.Context, spaceID int64) *SpaceFederation
	// RoomFederations returns the global identity of each listed room that has one.
	RoomFederations(ctx context.Context, roomIDs []int64) map[int64]*rooms.Federation
}

// InvitePreview is what an invite code reveals before joining.
type InvitePreview struct {
	Space       *Space
	InviterName string
	MemberCount int
}

// Federator is implemented by internal/federation. The After* hooks run on the origin
// after a local write and relay it to the mirrors; the *Remote* calls are made by a
// mirror on behalf of one of its users and are synchronous (the origin's answer is the
// user's answer, and the mirror is updated from it before the call returns). Every
// hook is best-effort and must not block.
type Federator interface {
	FederationInfo
	AfterSpaceUpdated(ctx context.Context, sp *Space)
	AfterSpaceDeleted(ctx context.Context, sp *Space)
	AfterSpaceMemberAdded(ctx context.Context, spaceID int64, m *SpaceMember)
	AfterSpaceMemberUpdated(ctx context.Context, spaceID int64, m *SpaceMember)
	AfterSpaceMemberRemoved(ctx context.Context, spaceID, userID int64, how string)
	AfterSpaceRoleChanged(ctx context.Context, spaceID int64, role *SpaceRole, deleted bool)
	AfterSpaceRoomChanged(ctx context.Context, spaceID int64, room *rooms.Room, overrides RoomOverrides, deleted bool)
	AfterSpaceRoomsReordered(ctx context.Context, spaceID int64, list []*rooms.Room)
	AfterSpaceEmojiChanged(ctx context.Context, spaceID int64, e *SpaceEmoji, deleted bool)

	PreviewRemoteInvite(ctx context.Context, domain, code string) (*InvitePreview, error)
	JoinRemoteSpace(ctx context.Context, domain, code string, user *auth.User) (*Space, error)
	LeaveRemoteSpace(ctx context.Context, origin string, spaceID int64, user *auth.User) error
	CreateRemoteInvite(ctx context.Context, origin string, spaceID int64, user *auth.User, in *CreateInviteInput) (*SpaceInvite, error)

	// Management of a mirrored space, done at the origin as the member and applied here
	// from the origin's answer. Ids in and out are this instance's.
	RemotePatchSpace(ctx context.Context, origin string, spaceID int64, user *auth.User, in *PatchSpaceInput) (*Space, error)
	RemoteSetSpaceImage(ctx context.Context, origin string, spaceID int64, user *auth.User, kind, url string) (*Space, error)
	RemoteCreateRole(ctx context.Context, origin string, spaceID int64, user *auth.User, in *CreateSpaceRoleInput) (*SpaceRole, error)
	RemoteUpdateRole(ctx context.Context, origin string, spaceID int64, user *auth.User, roleID int64, in *UpdateSpaceRoleInput) (*SpaceRole, error)
	RemoteDeleteRole(ctx context.Context, origin string, spaceID int64, user *auth.User, roleID int64) error
	RemoteSetMemberRoles(ctx context.Context, origin string, spaceID int64, user *auth.User, targetID int64, roleIDs []int64) error
	RemoteModerate(ctx context.Context, origin string, spaceID int64, user *auth.User, action string, targetID int64, reason string) error
	RemoteListBans(ctx context.Context, origin string, spaceID int64, user *auth.User) ([]SpaceBan, map[int64]*auth.User, error)
	RemoteListInvites(ctx context.Context, origin string, spaceID int64, user *auth.User) ([]SpaceInvite, map[int64]*auth.User, error)
	RemoteDeleteInvite(ctx context.Context, origin string, spaceID int64, user *auth.User, code string) error
	RemoteListAuditLog(ctx context.Context, origin string, spaceID int64, user *auth.User, beforeID int64, limit int, action string) ([]SpaceAuditEntry, map[int64]*auth.User, error)
	RemoteCreateRoom(ctx context.Context, origin string, spaceID int64, user *auth.User, in *CreateRoomInput) (*rooms.Room, error)
	RemoteUpdateRoom(ctx context.Context, origin string, spaceID int64, user *auth.User, roomID int64, in *UpdateRoomInput) error
	RemoteDeleteRoom(ctx context.Context, origin string, spaceID int64, user *auth.User, roomID int64) error
	RemoteReorderRooms(ctx context.Context, origin string, spaceID int64, user *auth.User, in *ReorderRoomsInput) error
	RemoteMoveChannel(ctx context.Context, origin string, spaceID int64, user *auth.User, channelID int64, parent, before *int64) error
	RemoteOverride(ctx context.Context, origin string, spaceID int64, user *auth.User, roomID, roleID, targetUserID int64, allow, deny int64, remove bool) error
	RemoteTransferOwnership(ctx context.Context, origin string, spaceID int64, user *auth.User, newOwnerID int64) (*Space, error)
	RemoteDeleteSpace(ctx context.Context, origin string, spaceID int64, user *auth.User, confirmName string) error
	RemoteCreateEmoji(ctx context.Context, origin string, spaceID int64, user *auth.User, emojiID int64, name, url string, animated bool) (*SpaceEmoji, error)
	RemoteRenameEmoji(ctx context.Context, origin string, spaceID int64, user *auth.User, emojiID int64, name string) (*SpaceEmoji, error)
	RemoteDeleteEmoji(ctx context.Context, origin string, spaceID int64, user *auth.User, emojiID int64) (*SpaceEmoji, error)
	// RemoteInstallBot adds a bot of this instance to a space hosted elsewhere, as the
	// member who consented; RemoteAddMember is the spaces.join scope for such a space.
	RemoteInstallBot(ctx context.Context, origin string, spaceID int64, user, bot *auth.User, permissions int64) (granted int64, err error)
	RemoteAddMember(ctx context.Context, origin string, spaceID int64, actor, user *auth.User) (added bool, err error)
	// ResyncRemote reconciles a mirror against a fresh snapshot from its origin.
	ResyncRemote(ctx context.Context, origin string, spaceID int64) error
}

// Resync brings a mirrored space back in line with its origin, for a member who thinks
// it has drifted (a relay missed while this instance was down). A space hosted here has
// nothing to resync from.
func (s *Service) Resync(ctx context.Context, actorID, spaceID int64) error {
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	origin := s.mirrorOrigin(ctx, spaceID)
	if origin == "" || s.fed == nil {
		return ErrNotRemoteSpace
	}
	return s.fed.ResyncRemote(ctx, origin, spaceID)
}

// ErrNotRemoteSpace: the space is hosted here, so there is no origin to sync from.
var ErrNotRemoteSpace = errors.New("this space is hosted on this instance")

// IsUserTargetAction reports whether an audit action's target_id is a user id.
func IsUserTargetAction(action string) bool { return userTargetActions[action] }

// remoteActor loads the local user a forwarded management call is made for.
func (s *Service) remoteActor(ctx context.Context, actorID int64) (*auth.User, error) {
	u, err := s.userRepo.GetByID(ctx, actorID)
	if err != nil {
		return nil, err
	}
	if u == nil || u.IsRemote() {
		return nil, ErrNotMember
	}
	return u, nil
}

// remoteSpace is the (origin, actor) pair for a management call on a mirror, or
// ("", nil) for a space hosted here.
func (s *Service) remoteSpace(ctx context.Context, actorID, spaceID int64) (string, *auth.User, error) {
	origin := s.mirrorOrigin(ctx, spaceID)
	if origin == "" {
		return "", nil, nil
	}
	u, err := s.remoteActor(ctx, actorID)
	if err != nil {
		return "", nil, err
	}
	return origin, u, nil
}

func (s *Service) fedEmojiChanged(ctx context.Context, spaceID int64, e *SpaceEmoji, deleted bool) {
	if s.fed != nil && e != nil {
		s.fed.AfterSpaceEmojiChanged(ctx, spaceID, e, deleted)
	}
}

var (
	// ErrRemoteSpace: the space is a mirror; the origin instance is the only place it can
	// be managed from.
	ErrRemoteSpace = errors.New("this space is hosted on another instance and can only be managed from there")
	// ErrOriginUnavailable: the instance hosting the space did not answer.
	ErrOriginUnavailable = errors.New("the instance hosting this space could not be reached")
	// ErrFederationOff: an invite code names another instance but this one doesn't federate.
	ErrFederationOff = errors.New("this instance does not federate with other instances")
)

// OriginError is the origin instance's refusal of a forwarded request, passed through to
// the user with the origin's status and message.
type OriginError struct {
	Status  int
	Message string
}

func (e *OriginError) Error() string { return e.Message }

// SetFederator wires outbound federation (API process).
func (s *Service) SetFederator(f Federator) {
	s.fed = f
	s.fedInfo = f
}

// SetFederationInfo wires read-only federation metadata (gateway process).
func (s *Service) SetFederationInfo(f FederationInfo) {
	s.fedInfo = f
}

// spaceFederation is the space's global identity when it is a mirror (nil otherwise).
func (s *Service) spaceFederation(ctx context.Context, spaceID int64) *SpaceFederation {
	if s.fedInfo == nil {
		return nil
	}
	return s.fedInfo.SpaceFederation(ctx, spaceID)
}

// mirrorOrigin is the hosting instance of a mirrored space, "" for a space hosted here.
func (s *Service) mirrorOrigin(ctx context.Context, spaceID int64) string {
	if f := s.spaceFederation(ctx, spaceID); f != nil {
		return f.OriginDomain
	}
	return ""
}

// assertLocal refuses management of a mirrored space.
func (s *Service) assertLocal(ctx context.Context, spaceID int64) error {
	if s.mirrorOrigin(ctx, spaceID) != "" {
		return ErrRemoteSpace
	}
	return nil
}

// attachFederation fills Space.Federation for mirrors so every payload carries it.
func (s *Service) attachFederation(ctx context.Context, list ...*Space) {
	if s.fedInfo == nil {
		return
	}
	for _, sp := range list {
		if sp != nil && sp.Federation == nil {
			sp.Federation = s.fedInfo.SpaceFederation(ctx, sp.ID)
		}
	}
}

// RoomFederations is FederationInfo.RoomFederations, nil-safe.
func (s *Service) RoomFederations(ctx context.Context, roomIDs []int64) map[int64]*rooms.Federation {
	if s.fedInfo == nil || len(roomIDs) == 0 {
		return nil
	}
	return s.fedInfo.RoomFederations(ctx, roomIDs)
}

// AttachFederation adds a room's global identity to its wire map.
func AttachFederation(m map[string]interface{}, f *rooms.Federation) map[string]interface{} {
	if f != nil {
		m["federation"] = f
	}
	return m
}

// ---- invite codes ---------------------------------------------------------------------

var (
	inviteCodeRe   = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	inviteDomainRe = regexp.MustCompile(`^[A-Za-z0-9.\-]+(?::\d+)?$`)
)

// ParseInviteCode splits "code@domain" (domain optional) as people type it. The domain
// is the hosting instance; "" means this one.
func ParseInviteCode(raw string) (code, domain string, err error) {
	raw = strings.TrimSpace(raw)
	if i := strings.LastIndexByte(raw, '@'); i >= 0 {
		code, domain = raw[:i], strings.ToLower(raw[i+1:])
		if !inviteDomainRe.MatchString(domain) {
			return "", "", ErrInviteNotFound
		}
	} else {
		code = raw
	}
	if !inviteCodeRe.MatchString(code) {
		return "", "", ErrInviteNotFound
	}
	return code, domain, nil
}

// FormatInviteCode is the inverse of ParseInviteCode.
func FormatInviteCode(code, domain string) string {
	if domain == "" {
		return code
	}
	return code + "@" + domain
}

// localDomain is this instance's federation domain ("" when federation is off).
func (s *Service) localDomain() string {
	if s.cfg == nil {
		return ""
	}
	return strings.ToLower(s.cfg.Federation.Domain)
}

// remoteInviteDomain returns the hosting instance named by an invite code when it is not
// this one ("" for a local code), checking federation is available for it.
func (s *Service) remoteInviteDomain(raw string) (code, domain string, err error) {
	code, domain, err = ParseInviteCode(raw)
	if err != nil {
		return "", "", err
	}
	if domain == "" || domain == s.localDomain() {
		return code, "", nil
	}
	if s.fed == nil {
		return "", "", ErrFederationOff
	}
	return code, domain, nil
}

// ---- origin-side hooks ----------------------------------------------------------------

func (s *Service) fedSpaceUpdated(ctx context.Context, sp *Space) {
	if s.fed != nil && sp != nil {
		s.fed.AfterSpaceUpdated(ctx, sp)
	}
}

func (s *Service) fedMemberAdded(ctx context.Context, spaceID, userID int64) {
	if s.fed == nil {
		return
	}
	m, err := s.repo.GetMember(ctx, spaceID, userID)
	if err != nil || m == nil {
		return
	}
	s.fed.AfterSpaceMemberAdded(ctx, spaceID, m)
}

func (s *Service) fedMemberUpdated(ctx context.Context, spaceID, userID int64) {
	if s.fed == nil {
		return
	}
	m, err := s.repo.GetMember(ctx, spaceID, userID)
	if err != nil || m == nil {
		return
	}
	s.fed.AfterSpaceMemberUpdated(ctx, spaceID, m)
}

func (s *Service) fedMemberRemoved(ctx context.Context, spaceID, userID int64, how string) {
	if s.fed != nil {
		s.fed.AfterSpaceMemberRemoved(ctx, spaceID, userID, how)
	}
}

func (s *Service) fedRoleChanged(ctx context.Context, spaceID int64, role *SpaceRole, deleted bool) {
	if s.fed != nil && role != nil {
		s.fed.AfterSpaceRoleChanged(ctx, spaceID, role, deleted)
	}
}

// fedRoomChanged relays a room with its current overrides (or its deletion).
func (s *Service) fedRoomChanged(ctx context.Context, spaceID, roomID int64, deleted bool) {
	if s.fed == nil {
		return
	}
	if deleted {
		s.fed.AfterSpaceRoomChanged(ctx, spaceID, &rooms.Room{ID: roomID, SpaceID: &spaceID}, RoomOverrides{}, true)
		return
	}
	room, err := s.roomRepo.GetByID(ctx, roomID)
	if err != nil || room == nil {
		return
	}
	ro, _ := s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
	uo, _ := s.repo.ListRoomUserOverrides(ctx, spaceID, roomID)
	s.fed.AfterSpaceRoomChanged(ctx, spaceID, room, RoomOverrides{Roles: ro, Users: uo}, false)
}

func (s *Service) fedRoomsReordered(ctx context.Context, spaceID int64) {
	if s.fed == nil {
		return
	}
	list, err := s.listRooms(ctx, spaceID)
	if err != nil {
		return
	}
	s.fed.AfterSpaceRoomsReordered(ctx, spaceID, list)
}

// ---- members, unchecked ---------------------------------------------------------------

// MemberWithUser is a member row with its profile.
type MemberWithUser struct {
	Member SpaceMember
	User   *auth.User
}

// Members lists a space's members with profiles, no caller check (the origin builds a
// snapshot for a peer from this; ListMembers is the member-facing form).
func (s *Service) Members(ctx context.Context, spaceID int64) ([]MemberWithUser, error) {
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
	out := make([]MemberWithUser, 0, len(rows))
	for i, m := range rows {
		if i < len(users) && users[i] != nil {
			out = append(out, MemberWithUser{Member: m, User: users[i]})
		}
	}
	return out, nil
}

// MembersPage is Members one page at a time: up to limit members whose user id is above
// after, in id order. next is the cursor for the page after this one, 0 when this was the
// last (a page that comes back full may still be the last; the next read is then empty).
func (s *Service) MembersPage(ctx context.Context, spaceID, after int64, limit int) (members []MemberWithUser, next int64, err error) {
	rows, err := s.repo.ListMembersPage(ctx, spaceID, after, limit)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 {
		return nil, 0, nil
	}
	if len(rows) == limit {
		next = rows[len(rows)-1].UserID
	}
	userIDs := make([]int64, 0, len(rows))
	for _, m := range rows {
		userIDs = append(userIDs, m.UserID)
	}
	users, err := s.userRepo.GetByIDs(ctx, userIDs)
	if err != nil {
		return nil, 0, err
	}
	members = make([]MemberWithUser, 0, len(rows))
	for i, m := range rows {
		if i < len(users) && users[i] != nil {
			members = append(members, MemberWithUser{Member: m, User: users[i]})
		}
	}
	return members, next, nil
}

// CountMembers counts a space's members, no caller check.
func (s *Service) CountMembers(ctx context.Context, spaceID int64) (int, error) {
	return s.repo.CountMembers(ctx, spaceID)
}

// Emojis lists a space's custom emoji, no caller check (for the origin's snapshot).
func (s *Service) Emojis(ctx context.Context, spaceID int64) ([]SpaceEmoji, error) {
	return s.repo.ListEmojis(ctx, spaceID)
}

// LocalMemberCount counts the members of a space who live on this instance.
func (s *Service) LocalMemberCount(ctx context.Context, spaceID int64) (int, error) {
	list, err := s.Members(ctx, spaceID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range list {
		if !m.User.IsRemote() {
			n++
		}
	}
	return n, nil
}

// ---- mirrors --------------------------------------------------------------------------

// MirrorRoom is a room as the origin describes it, already in local ids.
type MirrorRoom struct {
	Room      *rooms.Room
	Overrides RoomOverrides
}

// MirrorMember is a member as the origin describes them, already a local user id.
type MirrorMember struct {
	UserID   int64
	RoleIDs  []int64
	Nickname string
	JoinedAt time.Time
}

// RoomOrder is one room's place in the channel list.
type RoomOrder struct {
	RoomID   int64
	ParentID *int64
	Position int
}

// MirrorEmoji is a custom emoji as the origin describes it (its id is kept).
type MirrorEmoji struct {
	ID        int64
	Name      string
	URL       string
	Animated  bool
	CreatorID int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MirrorSpec is everything a mirror is built from.
type MirrorSpec struct {
	Space   *Space
	Roles   []SpaceRole
	Rooms   []MirrorRoom
	Members []MirrorMember
	Emoji   []MirrorEmoji
	// MembersComplete: Members is the whole list. When a page could not be fetched it is
	// a prefix, and reconciling must not take the missing members for gone ones.
	MembersComplete bool
}

func (e MirrorEmoji) row(spaceID int64) *SpaceEmoji {
	return &SpaceEmoji{SpaceID: spaceID, ID: e.ID, Name: e.Name, URL: e.URL, Animated: e.Animated, CreatorID: e.CreatorID, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
}

// ApplyMirrorEmoji stores, renames or deletes a custom emoji the origin reported.
func (s *Service) ApplyMirrorEmoji(ctx context.Context, spaceID int64, e MirrorEmoji, deleted bool) error {
	if deleted {
		if err := s.repo.DeleteEmoji(ctx, spaceID, e.ID); err != nil {
			return err
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_EMOJI_DELETE", map[string]interface{}{"emoji_id": id.Format(e.ID)})
		return nil
	}
	existing, err := s.repo.GetEmoji(ctx, spaceID, e.ID)
	if err != nil {
		return err
	}
	row := e.row(spaceID)
	if err := s.repo.CreateEmoji(ctx, row); err != nil {
		return err
	}
	event := "SPACE_EMOJI_UPDATE"
	if existing == nil {
		event = "SPACE_EMOJI_CREATE"
	}
	s.publishSpaceEvent(ctx, spaceID, event, spaceEmojiEventData(row))
	return nil
}

// ReconcileMirror brings an existing mirror in line with a fresh description from the
// origin: anything the origin no longer has goes, anything new or changed is applied,
// with the usual gateway events. Returns the ids of rooms removed here so the caller
// can drop their mappings.
func (s *Service) ReconcileMirror(ctx context.Context, spec *MirrorSpec) (removedRooms []int64, err error) {
	if spec == nil || spec.Space == nil {
		return nil, ErrSpaceNotFound
	}
	spaceID := spec.Space.ID
	if err := s.ApplyMirrorSpace(ctx, spec.Space); err != nil {
		return nil, err
	}
	// Roles.
	haveRoles, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	wantRoles := map[int64]struct{}{}
	for i := range spec.Roles {
		r := spec.Roles[i]
		wantRoles[r.ID] = struct{}{}
		if err := s.ApplyMirrorRole(ctx, spaceID, &r, false); err != nil {
			return nil, err
		}
	}
	for i := range haveRoles {
		if _, ok := wantRoles[haveRoles[i].ID]; !ok {
			if err := s.ApplyMirrorRole(ctx, spaceID, &haveRoles[i], true); err != nil {
				return nil, err
			}
		}
	}
	// Rooms.
	haveRooms, err := s.roomRepo.ListBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	wantRooms := map[int64]struct{}{}
	for _, mr := range spec.Rooms {
		if mr.Room == nil {
			continue
		}
		wantRooms[mr.Room.ID] = struct{}{}
		if err := s.ApplyMirrorRoom(ctx, spaceID, mr, false); err != nil {
			return nil, err
		}
	}
	for _, row := range haveRooms {
		if _, ok := wantRooms[row.RoomID]; !ok {
			if err := s.ApplyMirrorRoom(ctx, spaceID, MirrorRoom{Room: &rooms.Room{ID: row.RoomID}}, true); err != nil {
				return nil, err
			}
			removedRooms = append(removedRooms, row.RoomID)
		}
	}
	// Members: the origin's list is the truth, local members included.
	haveMembers, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	wantMembers := map[int64]struct{}{}
	haveByID := make(map[int64]SpaceMember, len(haveMembers))
	for _, m := range haveMembers {
		haveByID[m.UserID] = m
	}
	for _, m := range spec.Members {
		wantMembers[m.UserID] = struct{}{}
		if existing, ok := haveByID[m.UserID]; !ok {
			if err := s.AddMirrorMember(ctx, spaceID, m); err != nil {
				return nil, err
			}
		} else if !multisetEqualInt64(existing.RoleIDs, m.RoleIDs) {
			if err := s.UpdateMirrorMember(ctx, spaceID, m); err != nil {
				return nil, err
			}
		}
	}
	if spec.MembersComplete {
		for _, m := range haveMembers {
			if _, ok := wantMembers[m.UserID]; !ok {
				if err := s.RemoveMirrorMember(ctx, spaceID, m.UserID); err != nil {
					return nil, err
				}
			}
		}
	}
	// Emoji.
	haveEmoji, err := s.repo.ListEmojis(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	wantEmoji := map[int64]struct{}{}
	for _, e := range spec.Emoji {
		wantEmoji[e.ID] = struct{}{}
		if err := s.ApplyMirrorEmoji(ctx, spaceID, e, false); err != nil {
			return nil, err
		}
	}
	for _, e := range haveEmoji {
		if _, ok := wantEmoji[e.ID]; !ok {
			if err := s.ApplyMirrorEmoji(ctx, spaceID, MirrorEmoji{ID: e.ID}, true); err != nil {
				return nil, err
			}
		}
	}
	s.invalidateSnapshot(ctx, spaceID)
	return removedRooms, nil
}

// CreateMirror stores a space another instance hosts. Publishes nothing: the caller
// announces the member whose join produced it (AnnounceMirrorMember).
func (s *Service) CreateMirror(ctx context.Context, spec *MirrorSpec) error {
	if spec == nil || spec.Space == nil {
		return ErrSpaceNotFound
	}
	sp := spec.Space
	if sp.CreatedAt.IsZero() {
		sp.CreatedAt = time.Now().UTC()
	}
	if sp.UpdatedAt.IsZero() {
		sp.UpdatedAt = sp.CreatedAt
	}
	if err := s.repo.Create(ctx, sp); err != nil {
		return err
	}
	for i := range spec.Roles {
		r := spec.Roles[i]
		r.SpaceID = sp.ID
		if err := s.repo.InsertSpaceRole(ctx, &r); err != nil {
			return err
		}
	}
	for _, mr := range spec.Rooms {
		if err := s.putMirrorRoom(ctx, sp.ID, mr, false); err != nil {
			return err
		}
	}
	for _, m := range spec.Members {
		if err := s.putMirrorMember(ctx, sp.ID, m); err != nil {
			return err
		}
	}
	for _, e := range spec.Emoji {
		if err := s.repo.CreateEmoji(ctx, e.row(sp.ID)); err != nil {
			return err
		}
	}
	s.invalidateSnapshot(ctx, sp.ID)
	return nil
}

func (s *Service) putMirrorMember(ctx context.Context, spaceID int64, m MirrorMember) error {
	if err := s.repo.AddMember(ctx, spaceID, m.UserID, m.RoleIDs); err != nil {
		return err
	}
	// Shadows get the space in their users.spaces set too: that set is what presence
	// fan-out (PublishPresenceUpdate) walks, so a remote member's status reaches the
	// local members of the space.
	return s.repo.AddSpaceToUserSet(ctx, m.UserID, spaceID)
}

// AddMirrorMember records a member the origin reported (or a local user whose join just
// succeeded there) and announces them like a local join, minus the system-room notice,
// which the origin posts and relays as a message.
func (s *Service) AddMirrorMember(ctx context.Context, spaceID int64, m MirrorMember) error {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return ErrSpaceNotFound
	}
	if err := s.putMirrorMember(ctx, spaceID, m); err != nil {
		return err
	}
	if u, _ := s.userRepo.GetByID(ctx, m.UserID); u != nil && !u.IsRemote() {
		s.markPreJoinHistoryRead(ctx, spaceID, m.UserID)
	}
	s.publishMemberAdd(ctx, sp, m.UserID)
	return nil
}

// AnnounceMirrorMember publishes SPACE_MEMBER_ADD / SPACE_CREATE for a member already
// stored (the joining user after CreateMirror).
func (s *Service) AnnounceMirrorMember(ctx context.Context, spaceID, userID int64) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return
	}
	s.markPreJoinHistoryRead(ctx, spaceID, userID)
	s.publishMemberAdd(ctx, sp, userID)
}

// UpdateMirrorMember applies a member change the origin reported (roles).
func (s *Service) UpdateMirrorMember(ctx context.Context, spaceID int64, m MirrorMember) error {
	existing, err := s.repo.GetMember(ctx, spaceID, m.UserID)
	if err != nil {
		return err
	}
	if existing == nil {
		return s.AddMirrorMember(ctx, spaceID, m)
	}
	if err := s.repo.SetMemberRoleIDs(ctx, spaceID, m.UserID, m.RoleIDs); err != nil {
		return err
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_MEMBER_UPDATE", map[string]interface{}{
		"user_id":  id.Format(m.UserID),
		"role_ids": formatRoleIDStrings(m.RoleIDs),
	})
	return nil
}

// RemoveMirrorMember applies a removal the origin reported (kick, ban, leave) and tells
// the member's sessions and the rest of the space, like a local removal.
func (s *Service) RemoveMirrorMember(ctx context.Context, spaceID, userID int64) error {
	ok, err := s.repo.IsMember(ctx, spaceID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := s.repo.RemoveMember(ctx, spaceID, userID); err != nil {
		return err
	}
	s.publishMemberRemoved(ctx, spaceID, userID)
	return nil
}

// ApplyMirrorSpace overwrites a mirror's settings with what the origin reported.
func (s *Service) ApplyMirrorSpace(ctx context.Context, sp *Space) error {
	if sp == nil {
		return ErrSpaceNotFound
	}
	existing, err := s.repo.GetByID(ctx, sp.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrSpaceNotFound
	}
	set := map[string]interface{}{
		"name":                          sp.Name,
		"name_acronym":                  sp.NameAcronym,
		"description":                   sp.Description,
		"icon":                          sp.Icon,
		"banner":                        sp.Banner,
		"owner_id":                      sp.OwnerID,
		"verification_level":            sp.VerificationLevel,
		"default_message_notifications": sp.DefaultMessageNotif,
		"explicit_content_filter":       sp.ExplicitContentFilter,
		"features":                      sp.Features,
		"afk_room_id":                   nullableID(sp.AFKRoomID),
		"afk_timeout":                   sp.AFKTimeout,
		"system_room_id":                nullableID(sp.SystemRoomID),
		"system_room_flags":             sp.SystemRoomFlags,
		"rules_room_id":                 nullableID(sp.RulesRoomID),
		"max_presences":                 sp.MaxPresences,
		"max_members":                   sp.MaxMembers,
		"vanity_url_code":               sp.VanityURLCode,
		"preferred_locale":              sp.PreferredLocale,
		"public_updates_room_id":        nullableID(sp.PublicUpdatesRoomID),
		"max_video_room_users":          sp.MaxVideoRoomUsers,
		"everyone_role_id":              sp.EveryoneRoleID,
		"widget_enabled":                sp.WidgetEnabled,
		"widget_room_id":                nullableID(sp.WidgetRoomID),
		"updated_at":                    sp.UpdatedAt,
	}
	if err := s.repo.UpdateSpaceFields(ctx, sp.ID, set); err != nil {
		return err
	}
	// The owner may have changed, and every permission answer depends on who that is.
	s.invalidateSnapshot(ctx, sp.ID)
	updated, err := s.repo.GetByID(ctx, sp.ID)
	if err != nil || updated == nil {
		return ErrSpaceNotFound
	}
	s.attachFederation(ctx, updated)
	s.publishSpaceEvent(ctx, sp.ID, "SPACE_UPDATE", spaceToEventPayload(updated))
	return nil
}

// ApplyMirrorRole stores or deletes a role the origin reported.
func (s *Service) ApplyMirrorRole(ctx context.Context, spaceID int64, role *SpaceRole, deleted bool) error {
	if role == nil {
		return ErrRoleNotFound
	}
	if deleted {
		if err := s.repo.DeleteSpaceRole(ctx, spaceID, role.ID); err != nil {
			return err
		}
		s.invalidateSnapshot(ctx, spaceID)
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROLE_DELETE", map[string]interface{}{"role_id": id.Format(role.ID)})
		return nil
	}
	existing, err := s.repo.GetSpaceRole(ctx, spaceID, role.ID)
	if err != nil {
		return err
	}
	role.SpaceID = spaceID
	if err := s.repo.InsertSpaceRole(ctx, role); err != nil {
		return err
	}
	s.invalidateSnapshot(ctx, spaceID)
	event := "SPACE_ROLE_UPDATE"
	if existing == nil {
		event = "SPACE_ROLE_CREATE"
	}
	s.publishSpaceEvent(ctx, spaceID, event, spaceRoleEventData(role))
	return nil
}

// putMirrorRoom writes a room and its overrides; with publish set it emits the same
// gateway events the origin's own clients saw.
func (s *Service) putMirrorRoom(ctx context.Context, spaceID int64, mr MirrorRoom, publish bool) error {
	if mr.Room == nil {
		return ErrInvalidRoom
	}
	room := mr.Room
	room.SpaceID = &spaceID
	existing, err := s.roomRepo.GetByID(ctx, room.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		if err := s.roomRepo.CreateSpaceRoom(ctx, room); err != nil {
			return err
		}
	} else {
		if existing.Name != room.Name || existing.Topic != room.Topic || existing.SlowmodeSeconds != room.SlowmodeSeconds {
			if err := s.roomRepo.UpdateSpaceRoom(ctx, room.ID, room.Name, room.Topic, room.SlowmodeSeconds); err != nil {
				return err
			}
		}
		if room.E2EEEnabled != nil && (existing.E2EEEnabled == nil || *existing.E2EEEnabled != *room.E2EEEnabled) {
			if err := s.roomRepo.UpdateRoomE2EEEnabled(ctx, room.ID, *room.E2EEEnabled); err != nil {
				return err
			}
		}
		if existing.UserLimit != room.UserLimit || existing.Bitrate != room.Bitrate {
			if err := s.roomRepo.UpdateVoiceSettings(ctx, room.ID, room.UserLimit, room.Bitrate); err != nil {
				return err
			}
		}
		if !int64PtrEqual(existing.ParentID, room.ParentID) || existing.Position != room.Position {
			if err := s.roomRepo.UpdateSpaceRoomParentAndPosition(ctx, spaceID, room.ID, room.ParentID, room.Position); err != nil {
				return err
			}
		}
	}
	// Overrides: make the stored set equal to what the origin reported.
	haveRoles, _ := s.repo.ListRoomRoleOverrides(ctx, spaceID, room.ID)
	haveUsers, _ := s.repo.ListRoomUserOverrides(ctx, spaceID, room.ID)
	wantRoles := map[int64]struct{}{}
	for i := range mr.Overrides.Roles {
		o := mr.Overrides.Roles[i]
		o.SpaceID, o.RoomID = spaceID, room.ID
		wantRoles[o.RoleID] = struct{}{}
		if err := s.repo.UpsertRoomRoleOverride(ctx, &o); err != nil {
			return err
		}
		if publish {
			s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_OVERRIDE_UPDATE", roomOverrideEventData(&o))
		}
	}
	for _, o := range haveRoles {
		if _, ok := wantRoles[o.RoleID]; ok {
			continue
		}
		if err := s.repo.DeleteRoomRoleOverride(ctx, spaceID, room.ID, o.RoleID); err != nil {
			return err
		}
		if publish {
			s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_OVERRIDE_DELETE", map[string]interface{}{
				"room_id": id.Format(room.ID), "role_id": id.Format(o.RoleID),
			})
		}
	}
	wantUsers := map[int64]struct{}{}
	for i := range mr.Overrides.Users {
		o := mr.Overrides.Users[i]
		o.SpaceID, o.RoomID = spaceID, room.ID
		wantUsers[o.UserID] = struct{}{}
		if err := s.repo.UpsertRoomUserOverride(ctx, &o); err != nil {
			return err
		}
		if publish {
			s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_UPDATE", roomUserOverrideEventData(&o))
		}
	}
	for _, o := range haveUsers {
		if _, ok := wantUsers[o.UserID]; ok {
			continue
		}
		if err := s.repo.DeleteRoomUserOverride(ctx, spaceID, room.ID, o.UserID); err != nil {
			return err
		}
		if publish {
			s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_USER_OVERRIDE_DELETE", map[string]interface{}{
				"room_id": id.Format(room.ID), "user_id": id.Format(o.UserID),
			})
		}
	}
	if !publish {
		return nil
	}
	updated, _ := s.roomRepo.GetByID(ctx, room.ID)
	if updated == nil {
		return nil
	}
	if existing == nil {
		payload := AttachOverrides(RoomMap(updated), mr.Overrides)
		payload["room_id"] = id.Format(updated.ID)
		if feds := s.RoomFederations(ctx, []int64{updated.ID}); feds != nil {
			AttachFederation(payload, feds[updated.ID])
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_CREATE", payload)
		return nil
	}
	payload := map[string]interface{}{
		"room_id":          id.Format(updated.ID),
		"name":             updated.Name,
		"topic":            updated.Topic,
		"slowmode_seconds": updated.SlowmodeSeconds,
		"e2ee_enabled":     updated.E2EEEnabled != nil && *updated.E2EEEnabled,
		"user_limit":       updated.UserLimit,
		"bitrate":          updated.Bitrate,
		"position":         updated.Position,
		"updated_at":       updated.UpdatedAt,
	}
	if updated.ParentID != nil {
		payload["parent_id"] = id.Format(*updated.ParentID)
	} else {
		payload["parent_id"] = nil
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_UPDATE", payload)
	return nil
}

// ApplyMirrorRoom stores, updates or deletes a room the origin reported.
func (s *Service) ApplyMirrorRoom(ctx context.Context, spaceID int64, mr MirrorRoom, deleted bool) error {
	if mr.Room == nil {
		return ErrInvalidRoom
	}
	if deleted {
		if err := s.roomRepo.DeleteSpaceRoom(ctx, spaceID, mr.Room.ID); err != nil {
			return err
		}
		s.invalidateSnapshot(ctx, spaceID)
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_DELETE", map[string]interface{}{"room_id": id.Format(mr.Room.ID)})
		return nil
	}
	if err := s.putMirrorRoom(ctx, spaceID, mr, true); err != nil {
		return err
	}
	s.invalidateSnapshot(ctx, spaceID)
	return nil
}

// ApplyMirrorReorder moves rooms to the parents and positions the origin reported.
func (s *Service) ApplyMirrorReorder(ctx context.Context, spaceID int64, order []RoomOrder) error {
	for _, o := range order {
		room, err := s.roomRepo.GetByID(ctx, o.RoomID)
		if err != nil || room == nil || room.SpaceID == nil || *room.SpaceID != spaceID {
			continue
		}
		if int64PtrEqual(room.ParentID, o.ParentID) && room.Position == o.Position {
			continue
		}
		if err := s.roomRepo.UpdateSpaceRoomParentAndPosition(ctx, spaceID, o.RoomID, o.ParentID, o.Position); err != nil {
			return err
		}
		payload := map[string]interface{}{
			"room_id":    id.Format(o.RoomID),
			"position":   o.Position,
			"updated_at": time.Now().UTC(),
		}
		if o.ParentID != nil {
			payload["parent_id"] = id.Format(*o.ParentID)
		} else {
			payload["parent_id"] = nil
		}
		s.publishSpaceEvent(ctx, spaceID, "SPACE_ROOM_UPDATE", payload)
	}
	return nil
}

// DeleteMirror drops a mirror: the origin deleted the space, or the last local member
// left it. Members hear SPACE_DELETE / SPACE_LEAVE like a local deletion.
func (s *Service) DeleteMirror(ctx context.Context, spaceID int64) error {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return nil
	}
	return s.deleteSpaceCascade(ctx, 0, sp)
}

// ---- forwarded member actions ---------------------------------------------------------

// joinRemote joins a space another instance hosts through that instance.
func (s *Service) joinRemote(ctx context.Context, actorID int64, domain, code string) (*Space, error) {
	user, err := s.userRepo.GetByID(ctx, actorID)
	if err != nil {
		return nil, err
	}
	if user == nil || user.IsRemote() {
		return nil, ErrNotMember
	}
	sp, err := s.fed.JoinRemoteSpace(ctx, domain, code, user)
	if err != nil {
		return nil, err
	}
	s.attachFederation(ctx, sp)
	return sp, nil
}

// leaveRemote leaves a mirrored space through its origin; the origin's answer is applied
// locally by the federation engine.
func (s *Service) leaveRemote(ctx context.Context, actorID, spaceID int64, origin string) error {
	user, err := s.userRepo.GetByID(ctx, actorID)
	if err != nil {
		return err
	}
	if user == nil {
		return ErrNotMember
	}
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	return s.fed.LeaveRemoteSpace(ctx, origin, spaceID, user)
}

// createRemoteInvite asks a mirrored space's origin for an invite on the member's behalf.
func (s *Service) createRemoteInvite(ctx context.Context, actorID, spaceID int64, origin string, in *CreateInviteInput) (*SpaceInvite, error) {
	ok, err := s.repo.IsMember(ctx, spaceID, actorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotMember
	}
	user, err := s.userRepo.GetByID(ctx, actorID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, ErrNotMember
	}
	return s.fed.CreateRemoteInvite(ctx, origin, spaceID, user, in)
}

// memberUserFields is the public profile part of a member payload; with federation on it
// carries the user's home instance so clients can show name#0001@domain and build the
// E2EE identity.
func (s *Service) memberUserFields(u *auth.User) map[string]interface{} {
	m := map[string]interface{}{
		"id":            id.Format(u.ID),
		"username":      u.Username,
		"discriminator": u.Discriminator,
		"display_name":  u.DisplayName,
		"avatar":        u.Avatar,
		"banner":        u.Banner,
		"bio":           u.Bio,
		"about_me":      u.AboutMe,
		"pronouns":      u.Pronouns,
		"bot":           u.Bot,
		"public_flags":  auth.PublicFlags(u),
		"presence":      auth.ToPublicPresence(u.Presence, true),
	}
	if local := s.localDomain(); local != "" {
		m["home_domain"] = local
		if u.IsRemote() {
			m["home_domain"] = u.HomeDomain
		}
		m["origin_id"] = id.Format(u.OriginID())
	}
	return m
}

// logMirror notes a mirror-side failure that must not fail the peer's request.
func logMirror(err error, spaceID int64, what string) {
	if err != nil {
		logger.Err("spaces", fmt.Errorf("%s: %w", what, err), map[string]any{"space_id": spaceID})
	}
}
