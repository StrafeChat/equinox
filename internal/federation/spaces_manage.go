package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/safego"
)

// Managing a space from a mirror, and keeping a mirror in step.
//
// A member on a mirror who may manage the space (by the mirrored roles, which are the
// origin's) does so through one call: POST /spaces/manage {op, params}. The origin runs
// the operation as that member with every check a local member gets, and answers with
// an op-specific result plus the relays the operation produced *for the asking
// instance* - captured instead of sent (WithRelayCapture). The mirror applies those
// exactly as it would have applied them arriving on their own, so its state is what
// every other mirror has, and the REST reply can be served from it. Ids on the wire are
// the origin's; users are FIDs.
//
// POST /spaces/sync hands a mirror a fresh snapshot to reconcile against; the API runs
// that for every mirror now and then (StartMaintenance) in case a relay was missed.

// ---- custom emoji -------------------------------------------------------------------------

type SpaceEmojiWire struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Animated  bool      `json:"animated"`
	Creator   string    `json:"creator,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SpaceEmojiEvent: POST /spaces/emoji - the origin reports a custom emoji added, renamed
// or removed. Emoji keep the origin's ids (messages reference them by id).
type SpaceEmojiEvent struct {
	Space  SpaceRef       `json:"space"`
	Action string         `json:"action"` // upsert | delete
	Emoji  SpaceEmojiWire `json:"emoji"`
}

func (s *Service) emojiWire(ctx context.Context, e *spaces.SpaceEmoji) SpaceEmojiWire {
	w := SpaceEmojiWire{ID: id.Format(e.ID), Name: e.Name, URL: e.URL, Animated: e.Animated, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
	if e.CreatorID != 0 {
		if u, _ := s.users.GetByID(ctx, e.CreatorID); u != nil {
			w.Creator = s.FIDOf(u)
		}
	}
	return w
}

func (s *Service) emojiFromWire(ctx context.Context, w SpaceEmojiWire) (spaces.MirrorEmoji, error) {
	eid, err := id.Parse(w.ID)
	if err != nil {
		return spaces.MirrorEmoji{}, ErrInvalidFID
	}
	name, err := spaces.ValidateEmojiName(w.Name)
	if err != nil {
		return spaces.MirrorEmoji{}, err
	}
	e := spaces.MirrorEmoji{ID: eid, Name: name, URL: httpURLOnly(w.URL), Animated: w.Animated, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
	if w.Creator != "" {
		if uid, err := s.ResolveLocalID(ctx, w.Creator); err == nil {
			e.CreatorID = uid
		}
	}
	return e, nil
}

func emojiRow(spaceID int64, e spaces.MirrorEmoji) *spaces.SpaceEmoji {
	return &spaces.SpaceEmoji{SpaceID: spaceID, ID: e.ID, Name: e.Name, URL: e.URL, Animated: e.Animated, CreatorID: e.CreatorID, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
}

func (s *Service) AfterSpaceEmojiChanged(ctx context.Context, spaceID int64, e *spaces.SpaceEmoji, deleted bool) {
	peers := s.spacePeers(ctx, spaceID)
	if len(peers) == 0 {
		return
	}
	action := "upsert"
	if deleted {
		action = "delete"
	}
	s.fanOut(ctx, peers, http.MethodPost, "/spaces/emoji", SpaceEmojiEvent{Space: s.spaceRef(ctx, spaceID), Action: action, Emoji: s.emojiWire(ctx, e)})
}

// ---- applying what the origin says (mirror side) ------------------------------------------

func (s *Service) applySpaceUpdate(ctx context.Context, m *SpaceMapping, body SpaceUpdateEvent) error {
	sp, err := s.spaceSvc.GetSpace(ctx, m.SpaceID)
	if err != nil {
		return err
	}
	if sp == nil {
		return spaces.ErrSpaceNotFound
	}
	s.applySpaceInfo(ctx, sp, m.OriginDomain, body.Info)
	return s.spaceSvc.ApplyMirrorSpace(ctx, sp)
}

func (s *Service) applySpaceMembers(ctx context.Context, m *SpaceMapping, body SpaceMemberEvent) error {
	switch body.Action {
	case "add", "update":
		member, err := s.mirrorMember(ctx, m.OriginDomain, body.Member)
		if err != nil {
			return err
		}
		if body.Action == "add" {
			return s.spaceSvc.AddMirrorMember(ctx, m.SpaceID, member)
		}
		return s.spaceSvc.UpdateMirrorMember(ctx, m.SpaceID, member)
	case "remove":
		uid, err := s.ResolveLocalID(ctx, body.Member.User.FID)
		if err != nil {
			return nil // never known here: nothing to remove
		}
		if err := s.spaceSvc.RemoveMirrorMember(ctx, m.SpaceID, uid); err != nil {
			return err
		}
		s.dropMirrorIfEmpty(ctx, m)
		return nil
	}
	return errUnknownAction
}

func (s *Service) applySpaceRoles(ctx context.Context, m *SpaceMapping, body SpaceRoleEvent) error {
	role, err := s.roleFromWire(ctx, body.Role)
	if err != nil {
		return err
	}
	switch body.Action {
	case "upsert", "delete":
		return s.spaceSvc.ApplyMirrorRole(ctx, m.SpaceID, role, body.Action == "delete")
	}
	return errUnknownAction
}

func (s *Service) applySpaceRooms(ctx context.Context, m *SpaceMapping, body SpaceRoomEvent) error {
	switch body.Action {
	case "upsert":
		if body.Room == nil {
			return errUnknownAction
		}
		mr, err := s.mirrorRoom(ctx, m.OriginDomain, *body.Room, true)
		if err != nil {
			return err
		}
		return s.spaceSvc.ApplyMirrorRoom(ctx, m.SpaceID, mr, false)
	case "delete":
		if body.Room == nil {
			return errUnknownAction
		}
		oid, err := id.Parse(body.Room.ID)
		if err != nil {
			return ErrInvalidFID
		}
		rm, err := s.repo.GetRoomByOrigin(ctx, m.OriginDomain, oid)
		if err != nil || rm == nil {
			return nil
		}
		if err := s.spaceSvc.ApplyMirrorRoom(ctx, m.SpaceID, spaces.MirrorRoom{Room: &rooms.Room{ID: rm.RoomID}}, true); err != nil {
			return err
		}
		_ = s.repo.DeleteRoomMapping(ctx, rm)
		return nil
	case "reorder":
		order := make([]spaces.RoomOrder, 0, len(body.Order))
		for _, o := range body.Order {
			oid, err := id.Parse(o.ID)
			if err != nil {
				continue
			}
			local, err := s.localRoomID(ctx, m.OriginDomain, oid, false)
			if err != nil {
				continue
			}
			ro := spaces.RoomOrder{RoomID: local, Position: o.Position}
			if o.Parent != "" {
				if pid, err := id.Parse(o.Parent); err == nil {
					if parent, err := s.localRoomID(ctx, m.OriginDomain, pid, false); err == nil {
						ro.ParentID = &parent
					}
				}
			}
			order = append(order, ro)
		}
		return s.spaceSvc.ApplyMirrorReorder(ctx, m.SpaceID, order)
	}
	return errUnknownAction
}

func (s *Service) applySpaceEmoji(ctx context.Context, m *SpaceMapping, body SpaceEmojiEvent) error {
	switch body.Action {
	case "upsert":
		e, err := s.emojiFromWire(ctx, body.Emoji)
		if err != nil {
			return err
		}
		return s.spaceSvc.ApplyMirrorEmoji(ctx, m.SpaceID, e, false)
	case "delete":
		eid, err := id.Parse(body.Emoji.ID)
		if err != nil {
			return ErrInvalidFID
		}
		return s.spaceSvc.ApplyMirrorEmoji(ctx, m.SpaceID, spaces.MirrorEmoji{ID: eid}, true)
	}
	return errUnknownAction
}

var errUnknownAction = errors.New("unknown action")

// mirrorOf resolves a space ref in something the origin sent to the local mirror.
func (s *Service) mirrorOf(ctx context.Context, origin string, ref SpaceRef) (*SpaceMapping, error) {
	if ref.OriginDomain != origin {
		return nil, ErrPeerNotAllowed
	}
	originSpaceID, err := id.Parse(ref.OriginSpaceID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	m, err := s.repo.GetSpaceByOrigin(ctx, origin, originSpaceID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errSpaceNotMirror
	}
	return m, nil
}

// applyRelays applies, in order, the relays an origin handed back in a reply - the same
// requests it would otherwise have sent here.
func (s *Service) applyRelays(ctx context.Context, origin string, events []RelayEvent) {
	for _, ev := range events {
		if err := s.applyRelay(ctx, origin, ev); err != nil {
			logger.Err("federation", err, map[string]any{"peer": origin, "path": ev.Path})
		}
	}
}

func (s *Service) applyRelay(ctx context.Context, origin string, ev RelayEvent) error {
	switch ev.Path {
	case "/spaces/peers":
		var b SpacePeerEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		m, err := s.mirrorOf(ctx, origin, b.Space)
		if err != nil {
			return err
		}
		return s.applySpacePeers(ctx, m, b)
	case "/spaces/update":
		var b SpaceUpdateEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		m, err := s.mirrorOf(ctx, origin, b.Space)
		if err != nil {
			return err
		}
		return s.applySpaceUpdate(ctx, m, b)
	case "/spaces/members":
		var b SpaceMemberEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		m, err := s.mirrorOf(ctx, origin, b.Space)
		if err != nil {
			return err
		}
		return s.applySpaceMembers(ctx, m, b)
	case "/spaces/roles":
		var b SpaceRoleEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		m, err := s.mirrorOf(ctx, origin, b.Space)
		if err != nil {
			return err
		}
		return s.applySpaceRoles(ctx, m, b)
	case "/spaces/rooms":
		var b SpaceRoomEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		m, err := s.mirrorOf(ctx, origin, b.Space)
		if err != nil {
			return err
		}
		return s.applySpaceRooms(ctx, m, b)
	case "/spaces/emoji":
		var b SpaceEmojiEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		m, err := s.mirrorOf(ctx, origin, b.Space)
		if err != nil {
			return err
		}
		return s.applySpaceEmoji(ctx, m, b)
	case "/spaces/delete":
		var b SpaceDeleteEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		m, err := s.mirrorOf(ctx, origin, b.Space)
		if err != nil {
			if errors.Is(err, errSpaceNotMirror) {
				return nil
			}
			return err
		}
		s.dropMirror(ctx, m)
		return nil
	case "/rooms/messages":
		var b MessageEvent
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		sc, err := s.scopeByRef(ctx, origin, b.Room)
		if err != nil {
			return err
		}
		_, err = s.applyMessageEvent(ctx, origin, sc, b)
		return err
	case "/rooms/messages/delete":
		var b MessageDelete
		if err := json.Unmarshal(ev.Body, &b); err != nil {
			return err
		}
		sc, err := s.scopeByRef(ctx, origin, b.Room)
		if err != nil {
			return err
		}
		if msgID := s.localMessageID(ctx, sc.room.ID, b.Message); msgID != 0 {
			return s.msgSvc.DeleteFederated(ctx, sc.room.ID, msgID)
		}
		return nil
	}
	// Typing, presence, voice and anything else: nothing a management reply needs.
	return nil
}

// scopeByRef resolves a room ref the origin used to the local mirror of that room.
func (s *Service) scopeByRef(ctx context.Context, origin string, ref RoomRef) (*roomScope, error) {
	if ref.OriginDomain != origin {
		return nil, ErrPeerNotAllowed
	}
	oid, err := id.Parse(ref.OriginRoomID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	m, err := s.repo.GetRoomByOrigin(ctx, origin, oid)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, rooms.ErrRoomNotFound
	}
	sc, err := s.roomScope(ctx, m)
	if err != nil {
		return nil, err
	}
	sc.fromOrigin = true
	return sc, nil
}

// ---- manage: wire -------------------------------------------------------------------------

// SpaceManageRequest: POST /spaces/manage.
type SpaceManageRequest struct {
	Space  SpaceRef        `json:"space"`
	User   string          `json:"user"` // FID, must belong to the requesting instance
	Op     string          `json:"op"`
	Params json.RawMessage `json:"params,omitempty"`
}

// SpaceManageReply: the op's result (shape per op) and the relays captured for the
// asking instance.
type SpaceManageReply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Events []RelayEvent    `json:"events"`
}

type manageSpacePatch struct {
	Name                *string `json:"name,omitempty"`
	Description         *string `json:"description,omitempty"`
	SystemRoom          *string `json:"system_room,omitempty"`
	SystemRoomFlags     *int    `json:"system_room_flags,omitempty"`
	DefaultMessageNotif *int    `json:"default_message_notifications,omitempty"`
	AFKRoom             *string `json:"afk_room,omitempty"`
	AFKTimeout          *int    `json:"afk_timeout,omitempty"`
	WidgetEnabled       *bool   `json:"widget_enabled,omitempty"`
	WidgetRoom          *string `json:"widget_room,omitempty"`
}

type manageImage struct {
	Kind string `json:"kind"` // icon | banner
	URL  string `json:"url"`
}

type manageRoleUpdate struct {
	Role   string                      `json:"role"`
	Update spaces.UpdateSpaceRoleInput `json:"update"`
}

type manageRole struct {
	Role string `json:"role"`
}

type manageMemberRoles struct {
	User  string   `json:"user"`
	Roles []string `json:"roles"`
}

type manageMember struct {
	User   string `json:"user"`
	Reason string `json:"reason,omitempty"`
}

type manageInvite struct {
	Code string `json:"code"`
}

type manageAudit struct {
	Before string `json:"before,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Action string `json:"action,omitempty"`
}

type manageRoomCreate struct {
	Name        string `json:"name"`
	Type        int    `json:"type"`
	Parent      string `json:"parent,omitempty"`
	E2EEEnabled *bool  `json:"e2ee_enabled,omitempty"`
	UserLimit   *int   `json:"user_limit,omitempty"`
	Bitrate     *int   `json:"bitrate,omitempty"`
}

type manageRoomUpdate struct {
	Room   string                 `json:"room"`
	Update spaces.UpdateRoomInput `json:"update"`
}

type manageRoom struct {
	Room string `json:"room"`
}

type manageReorder struct {
	Scope  string   `json:"scope"`
	Parent string   `json:"parent,omitempty"`
	Rooms  []string `json:"rooms"`
}

type manageMove struct {
	Channel string `json:"channel"`
	Parent  string `json:"parent,omitempty"`
	Before  string `json:"before,omitempty"`
}

type manageOverride struct {
	Room  string `json:"room"`
	Role  string `json:"role,omitempty"`
	User  string `json:"user,omitempty"`
	Allow int64  `json:"allow"`
	Deny  int64  `json:"deny"`
}

type manageTransfer struct {
	User string `json:"user"`
}

type manageDelete struct {
	Name string `json:"name"`
}

type manageEmojiCreate struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Animated bool   `json:"animated"`
}

type manageEmojiRename struct {
	Emoji string `json:"emoji"`
	Name  string `json:"name"`
}

type manageEmoji struct {
	Emoji string `json:"emoji"`
}

// bot.install: a bot of the asking instance, added as the member who consented there.
type manageBotInstall struct {
	Bot         Profile `json:"bot"`
	Permissions int64   `json:"permissions"`
}

type manageGranted struct {
	Granted int64 `json:"granted"`
}

// member.add: the spaces.join scope - the actor (a bot of the asking instance, typically)
// adds a user of that instance who consented.
type manageMemberAdd struct {
	User Profile `json:"user"`
}

type manageAdded struct {
	Added bool `json:"added"`
}

type manageBan struct {
	User      Profile   `json:"user"`
	Reason    string    `json:"reason,omitempty"`
	BannedBy  *Profile  `json:"banned_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type manageBans struct {
	Bans []manageBan `json:"bans"`
}

type manageInviteRow struct {
	Code        string     `json:"code"`
	Inviter     *Profile   `json:"inviter,omitempty"`
	MaxUses     int        `json:"max_uses"`
	CurrentUses int        `json:"current_uses"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type manageInvites struct {
	Invites []manageInviteRow `json:"invites"`
}

type manageAuditEntry struct {
	ID         string          `json:"id"`
	ActionType string          `json:"action_type"`
	User       string          `json:"user"` // FID of the actor
	TargetID   string          `json:"target_id"`
	Changes    json.RawMessage `json:"changes,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

type manageAuditResult struct {
	Entries []manageAuditEntry `json:"entries"`
	Users   []Profile          `json:"users"`
}

var errBadParams = errors.New("invalid parameters")

func decodeParams(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return errBadParams
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return errBadParams
	}
	return nil
}

func optRoomID(raw *string) (*int64, bool) {
	if raw == nil {
		return nil, true
	}
	if strings.TrimSpace(*raw) == "" {
		return nil, true
	}
	v, err := id.Parse(strings.TrimSpace(*raw))
	if err != nil {
		return nil, false
	}
	return &v, true
}

// ---- manage: origin side ------------------------------------------------------------------

// SpaceManage POST /spaces/manage - run a management operation as a member elsewhere.
func (h *Handler) SpaceManage(c fiber.Ctx) error {
	var body SpaceManageRequest
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
	ctx, capture := WithRelayCapture(c.Context(), RequesterDomain(c))
	result, err := h.svc.manageAt(ctx, sp, actor, body.Op, body.Params)
	if err != nil {
		if errors.Is(err, errBadParams) || errors.Is(err, errUnknownAction) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		return spaceFail(c, err, map[string]any{"space_id": sp.ID, "op": body.Op})
	}
	reply := SpaceManageReply{Events: capture.take()}
	if result != nil {
		if reply.Result, err = json.Marshal(result); err != nil {
			return fail(c, err, map[string]any{"op": body.Op})
		}
	}
	return c.JSON(reply)
}

// manageAt dispatches one management operation to the spaces service as actor.
func (s *Service) manageAt(ctx context.Context, sp *spaces.Space, actor *auth.User, op string, params json.RawMessage) (any, error) {
	svc := s.spaceSvc
	spaceID := sp.ID
	switch op {
	case "space.patch":
		var p manageSpacePatch
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		in := &spaces.PatchSpaceInput{
			Name: p.Name, Description: p.Description, SystemRoomID: p.SystemRoom, SystemRoomFlags: p.SystemRoomFlags,
			DefaultMessageNotif: p.DefaultMessageNotif, AFKRoomID: p.AFKRoom, AFKTimeout: p.AFKTimeout,
			WidgetEnabled: p.WidgetEnabled, WidgetRoomID: p.WidgetRoom,
		}
		updated, err := svc.PatchSpace(ctx, actor.ID, spaceID, in)
		if err != nil {
			return nil, err
		}
		return s.spaceInfo(ctx, updated), nil
	case "space.image":
		var p manageImage
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		url := httpURLOnly(p.URL)
		if url == "" {
			return nil, errBadParams
		}
		var updated *spaces.Space
		var err error
		switch p.Kind {
		case "icon":
			updated, err = svc.UpdateSpaceIcon(ctx, actor.ID, spaceID, url)
		case "banner":
			updated, err = svc.UpdateSpaceBanner(ctx, actor.ID, spaceID, url)
		default:
			return nil, errBadParams
		}
		if err != nil {
			return nil, err
		}
		return s.spaceInfo(ctx, updated), nil
	case "role.create":
		var p spaces.CreateSpaceRoleInput
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		role, err := svc.CreateSpaceRole(ctx, actor.ID, spaceID, &p)
		if err != nil {
			return nil, err
		}
		return s.roleWire(ctx, role), nil
	case "role.update":
		var p manageRoleUpdate
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		rid, err := id.Parse(p.Role)
		if err != nil {
			return nil, errBadParams
		}
		role, err := svc.UpdateSpaceRole(ctx, actor.ID, spaceID, rid, &p.Update)
		if err != nil {
			return nil, err
		}
		return s.roleWire(ctx, role), nil
	case "role.delete":
		var p manageRole
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		rid, err := id.Parse(p.Role)
		if err != nil {
			return nil, errBadParams
		}
		return nil, svc.DeleteSpaceRole(ctx, actor.ID, spaceID, rid)
	case "member.roles":
		var p manageMemberRoles
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		target, err := s.ResolveLocalID(ctx, p.User)
		if err != nil {
			return nil, err
		}
		roleIDs := make([]int64, 0, len(p.Roles))
		for _, raw := range p.Roles {
			rid, err := id.Parse(raw)
			if err != nil {
				return nil, errBadParams
			}
			roleIDs = append(roleIDs, rid)
		}
		return nil, svc.SetMemberRoles(ctx, actor.ID, spaceID, target, roleIDs)
	case "member.kick", "member.ban", "member.unban":
		var p manageMember
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		target, err := s.ResolveLocalID(ctx, p.User)
		if err != nil {
			return nil, err
		}
		switch op {
		case "member.kick":
			return nil, svc.KickMember(ctx, actor.ID, spaceID, target)
		case "member.ban":
			return nil, svc.BanMember(ctx, actor.ID, spaceID, target, p.Reason)
		default:
			return nil, svc.UnbanMember(ctx, actor.ID, spaceID, target)
		}
	case "bot.install", "member.add":
		// The subject must be the asking instance's own user, like the actor: no instance
		// installs another's bot or speaks for another's users.
		var subject Profile
		var perms int64
		if op == "bot.install" {
			var p manageBotInstall
			if err := decodeParams(params, &p); err != nil {
				return nil, err
			}
			subject, perms = p.Bot, p.Permissions
			if !subject.Bot {
				return nil, spaces.ErrInvalidBot
			}
		} else {
			var p manageMemberAdd
			if err := decodeParams(params, &p); err != nil {
				return nil, err
			}
			subject = p.User
		}
		if _, domain, err := ParseFID(subject.FID); err != nil || domain != actor.HomeDomain {
			return nil, errForeignUser
		}
		u, err := s.EnsureShadow(ctx, actor.HomeDomain, subject)
		if err != nil {
			return nil, err
		}
		if op == "bot.install" {
			granted, err := svc.InstallBot(ctx, actor.ID, spaceID, u.ID, perms)
			if err != nil {
				return nil, err
			}
			return manageGranted{Granted: granted}, nil
		}
		added, err := svc.AddMemberViaOAuth(ctx, actor.ID, spaceID, u.ID)
		if err != nil {
			return nil, err
		}
		return manageAdded{Added: added}, nil
	case "bans.list":
		bans, users, err := svc.ListBans(ctx, actor.ID, spaceID)
		if err != nil {
			return nil, err
		}
		out := manageBans{Bans: make([]manageBan, 0, len(bans))}
		for _, b := range bans {
			u := users[b.UserID]
			if u == nil {
				continue
			}
			row := manageBan{User: s.ProfileOf(u), Reason: b.Reason, CreatedAt: b.CreatedAt}
			if by := users[b.BannedBy]; by != nil {
				p := s.ProfileOf(by)
				row.BannedBy = &p
			}
			out.Bans = append(out.Bans, row)
		}
		return out, nil
	case "invites.list":
		invites, users, err := svc.ListInvites(ctx, actor.ID, spaceID)
		if err != nil {
			return nil, err
		}
		out := manageInvites{Invites: make([]manageInviteRow, 0, len(invites))}
		for _, inv := range invites {
			row := manageInviteRow{Code: inv.Code, MaxUses: inv.MaxUses, CurrentUses: inv.CurrentUses, ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt}
			if u := users[inv.InviterID]; u != nil {
				p := s.ProfileOf(u)
				row.Inviter = &p
			}
			out.Invites = append(out.Invites, row)
		}
		return out, nil
	case "invite.delete":
		var p manageInvite
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		return nil, svc.DeleteInvite(ctx, actor.ID, spaceID, p.Code)
	case "audit.list":
		var p manageAudit
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		var before int64
		if p.Before != "" {
			v, err := id.Parse(p.Before)
			if err != nil {
				return nil, errBadParams
			}
			before = v
		}
		entries, users, err := svc.ListAuditLog(ctx, actor.ID, spaceID, before, p.Limit, p.Action)
		if err != nil {
			return nil, err
		}
		return s.auditResult(ctx, entries, users), nil
	case "room.create":
		var p manageRoomCreate
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		in := &spaces.CreateRoomInput{Name: p.Name, Type: p.Type, E2EEEnabled: p.E2EEEnabled, UserLimit: p.UserLimit, Bitrate: p.Bitrate}
		if p.Parent != "" {
			pid, err := id.Parse(p.Parent)
			if err != nil {
				return nil, errBadParams
			}
			in.ParentID = &pid
		}
		room, err := svc.CreateRoom(ctx, actor.ID, spaceID, in)
		if err != nil {
			return nil, err
		}
		return s.roomWire(ctx, room, spaces.RoomOverrides{}), nil
	case "room.update":
		var p manageRoomUpdate
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		rid, err := id.Parse(p.Room)
		if err != nil {
			return nil, errBadParams
		}
		if err := svc.UpdateRoom(ctx, actor.ID, spaceID, rid, &p.Update); err != nil {
			return nil, err
		}
		return nil, nil
	case "room.delete":
		var p manageRoom
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		rid, err := id.Parse(p.Room)
		if err != nil {
			return nil, errBadParams
		}
		return nil, svc.DeleteRoom(ctx, actor.ID, spaceID, rid)
	case "rooms.reorder":
		var p manageReorder
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		in := &spaces.ReorderRoomsInput{Scope: p.Scope, RoomIDs: p.Rooms}
		if p.Parent != "" {
			parent := p.Parent
			in.ParentSectionID = &parent
		}
		return nil, svc.ReorderRooms(ctx, actor.ID, spaceID, in)
	case "room.move":
		var p manageMove
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		channel, err := id.Parse(p.Channel)
		if err != nil {
			return nil, errBadParams
		}
		parent, ok := optRoomID(&p.Parent)
		if !ok {
			return nil, errBadParams
		}
		before, ok := optRoomID(&p.Before)
		if !ok {
			return nil, errBadParams
		}
		return nil, svc.MoveSpaceChannel(ctx, actor.ID, spaceID, channel, parent, before)
	case "override.put", "override.delete":
		var p manageOverride
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		roomID, err := id.Parse(p.Room)
		if err != nil {
			return nil, errBadParams
		}
		remove := op == "override.delete"
		switch {
		case p.Role != "":
			rid, err := id.Parse(p.Role)
			if err != nil {
				return nil, errBadParams
			}
			if remove {
				return nil, svc.DeleteRoomRoleOverride(ctx, actor.ID, spaceID, roomID, rid)
			}
			return nil, svc.PutRoomRoleOverride(ctx, actor.ID, spaceID, roomID, rid, &spaces.PutRoomRoleOverrideInput{Allow: p.Allow, Deny: p.Deny})
		case p.User != "":
			uid, err := s.ResolveLocalID(ctx, p.User)
			if err != nil {
				return nil, err
			}
			if remove {
				return nil, svc.DeleteRoomUserOverride(ctx, actor.ID, spaceID, roomID, uid)
			}
			return nil, svc.PutRoomUserOverride(ctx, actor.ID, spaceID, roomID, uid, &spaces.PutRoomUserOverrideInput{Allow: p.Allow, Deny: p.Deny})
		}
		return nil, errBadParams
	case "space.transfer":
		var p manageTransfer
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		target, err := s.ResolveLocalID(ctx, p.User)
		if err != nil {
			return nil, err
		}
		updated, err := svc.TransferOwnership(ctx, actor.ID, spaceID, target)
		if err != nil {
			return nil, err
		}
		return s.spaceInfo(ctx, updated), nil
	case "space.delete":
		var p manageDelete
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		return nil, svc.DeleteSpace(ctx, actor.ID, spaceID, p.Name)
	case "emoji.create":
		var p manageEmojiCreate
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		url := httpURLOnly(p.URL)
		if url == "" {
			return nil, errBadParams
		}
		eid := id.Next()
		if p.ID != "" {
			if v, err := id.Parse(p.ID); err == nil {
				eid = v
			}
		}
		e, err := svc.CreateEmoji(ctx, actor.ID, spaceID, eid, p.Name, url, p.Animated)
		if err != nil {
			return nil, err
		}
		return s.emojiWire(ctx, e), nil
	case "emoji.rename":
		var p manageEmojiRename
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		eid, err := id.Parse(p.Emoji)
		if err != nil {
			return nil, errBadParams
		}
		e, err := svc.RenameEmoji(ctx, actor.ID, spaceID, eid, p.Name)
		if err != nil {
			return nil, err
		}
		return s.emojiWire(ctx, e), nil
	case "emoji.delete":
		var p manageEmoji
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		eid, err := id.Parse(p.Emoji)
		if err != nil {
			return nil, errBadParams
		}
		e, err := svc.DeleteEmoji(ctx, actor.ID, spaceID, eid)
		if err != nil {
			return nil, err
		}
		return s.emojiWire(ctx, e), nil
	}
	return nil, errUnknownAction
}

// auditResult puts an audit page on the wire: actors and user targets as FIDs, room
// targets as the origin's room ids (what the mirror maps), everything else verbatim.
func (s *Service) auditResult(ctx context.Context, entries []spaces.SpaceAuditEntry, users map[int64]*auth.User) manageAuditResult {
	out := manageAuditResult{Entries: make([]manageAuditEntry, 0, len(entries)), Users: []Profile{}}
	seen := map[int64]struct{}{}
	fidOf := func(uid int64) string {
		u := users[uid]
		if u == nil {
			u, _ = s.users.GetByID(ctx, uid)
		}
		if u == nil {
			return ""
		}
		if _, ok := seen[u.ID]; !ok {
			seen[u.ID] = struct{}{}
			out.Users = append(out.Users, s.ProfileOf(u))
		}
		return s.FIDOf(u)
	}
	for _, e := range entries {
		row := manageAuditEntry{ID: id.Format(e.ID), ActionType: e.ActionType, User: fidOf(e.UserID), TargetID: e.TargetID, Reason: e.Reason, CreatedAt: e.CreatedAt}
		if e.Changes != "" && json.Valid([]byte(e.Changes)) {
			row.Changes = json.RawMessage(e.Changes)
		}
		switch {
		case spaces.IsUserTargetAction(e.ActionType):
			if uid, err := id.Parse(e.TargetID); err == nil {
				if fid := fidOf(uid); fid != "" {
					row.TargetID = fid
				}
			}
		case strings.HasPrefix(e.ActionType, "override_"):
			// "<room>:user:<id>" names a user; "<room>:role:<id>" needs no translation.
			if parts := strings.SplitN(e.TargetID, ":", 3); len(parts) == 3 && parts[1] == "user" {
				if uid, err := id.Parse(parts[2]); err == nil {
					if fid := fidOf(uid); fid != "" {
						row.TargetID = parts[0] + ":user:" + fid
					}
				}
			}
		}
		out.Entries = append(out.Entries, row)
	}
	return out
}

// ---- manage: mirror side ------------------------------------------------------------------

// manage sends one management operation to the origin and applies what comes back.
func (s *Service) manage(ctx context.Context, origin string, spaceID int64, user *auth.User, op string, params any, result any) error {
	m, err := s.repo.GetSpaceMapping(ctx, spaceID)
	if err != nil {
		return err
	}
	if m == nil {
		return spaces.ErrSpaceNotFound
	}
	req := SpaceManageRequest{Space: SpaceRef{OriginDomain: m.OriginDomain, OriginSpaceID: id.Format(m.OriginSpaceID)}, User: s.FIDOf(user), Op: op}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}
	var reply SpaceManageReply
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/manage", req, &reply); err != nil {
		return spaceOriginError(err, origin, "/spaces/manage "+op)
	}
	s.applyRelays(ctx, origin, reply.Events)
	if result != nil && len(reply.Result) > 0 {
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return spaces.ErrOriginUnavailable
		}
	}
	return nil
}

// originRoomID is the origin's id for a local room of a mirror.
func (s *Service) originRoomID(ctx context.Context, origin string, roomID int64) (string, error) {
	m, err := s.repo.GetRoomMapping(ctx, roomID)
	if err != nil {
		return "", err
	}
	if m == nil || m.OriginDomain != origin {
		return "", spaces.ErrInvalidRoom
	}
	return id.Format(m.OriginRoomID), nil
}

func (s *Service) optOriginRoomID(ctx context.Context, origin string, raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	v := strings.TrimSpace(*raw)
	if v == "" {
		return &v, nil
	}
	local, err := id.Parse(v)
	if err != nil {
		return nil, spaces.ErrInvalidRoom
	}
	oid, err := s.originRoomID(ctx, origin, local)
	if err != nil {
		return nil, err
	}
	return &oid, nil
}

// fidOfLocal is the federated id of any user known here (local or shadow).
func (s *Service) fidOfLocal(ctx context.Context, uid int64) (string, error) {
	u, err := s.users.GetByID(ctx, uid)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", spaces.ErrNotMember
	}
	return s.FIDOf(u), nil
}

func (s *Service) RemotePatchSpace(ctx context.Context, origin string, spaceID int64, user *auth.User, in *spaces.PatchSpaceInput) (*spaces.Space, error) {
	p := manageSpacePatch{
		Name: in.Name, Description: in.Description, SystemRoomFlags: in.SystemRoomFlags, DefaultMessageNotif: in.DefaultMessageNotif,
		AFKTimeout: in.AFKTimeout, WidgetEnabled: in.WidgetEnabled,
	}
	var err error
	if p.SystemRoom, err = s.optOriginRoomID(ctx, origin, in.SystemRoomID); err != nil {
		return nil, spaces.ErrInvalidSystemRoom
	}
	if p.AFKRoom, err = s.optOriginRoomID(ctx, origin, in.AFKRoomID); err != nil {
		return nil, spaces.ErrInvalidAFKRoom
	}
	if p.WidgetRoom, err = s.optOriginRoomID(ctx, origin, in.WidgetRoomID); err != nil {
		return nil, spaces.ErrInvalidWidgetRoom
	}
	if err := s.manage(ctx, origin, spaceID, user, "space.patch", p, nil); err != nil {
		return nil, err
	}
	return s.spaceSvc.GetSpace(ctx, spaceID)
}

func (s *Service) RemoteSetSpaceImage(ctx context.Context, origin string, spaceID int64, user *auth.User, kind, url string) (*spaces.Space, error) {
	if err := s.manage(ctx, origin, spaceID, user, "space.image", manageImage{Kind: kind, URL: url}, nil); err != nil {
		return nil, err
	}
	return s.spaceSvc.GetSpace(ctx, spaceID)
}

func (s *Service) RemoteCreateRole(ctx context.Context, origin string, spaceID int64, user *auth.User, in *spaces.CreateSpaceRoleInput) (*spaces.SpaceRole, error) {
	var w SpaceRoleWire
	if err := s.manage(ctx, origin, spaceID, user, "role.create", in, &w); err != nil {
		return nil, err
	}
	role, err := s.roleFromWire(ctx, w)
	if err != nil {
		return nil, err
	}
	role.SpaceID = spaceID
	return role, nil
}

func (s *Service) RemoteUpdateRole(ctx context.Context, origin string, spaceID int64, user *auth.User, roleID int64, in *spaces.UpdateSpaceRoleInput) (*spaces.SpaceRole, error) {
	var w SpaceRoleWire
	if err := s.manage(ctx, origin, spaceID, user, "role.update", manageRoleUpdate{Role: id.Format(roleID), Update: *in}, &w); err != nil {
		return nil, err
	}
	role, err := s.roleFromWire(ctx, w)
	if err != nil {
		return nil, err
	}
	role.SpaceID = spaceID
	return role, nil
}

func (s *Service) RemoteDeleteRole(ctx context.Context, origin string, spaceID int64, user *auth.User, roleID int64) error {
	return s.manage(ctx, origin, spaceID, user, "role.delete", manageRole{Role: id.Format(roleID)}, nil)
}

func (s *Service) RemoteSetMemberRoles(ctx context.Context, origin string, spaceID int64, user *auth.User, targetID int64, roleIDs []int64) error {
	fid, err := s.fidOfLocal(ctx, targetID)
	if err != nil {
		return err
	}
	p := manageMemberRoles{User: fid, Roles: make([]string, 0, len(roleIDs))}
	for _, rid := range roleIDs {
		p.Roles = append(p.Roles, id.Format(rid))
	}
	return s.manage(ctx, origin, spaceID, user, "member.roles", p, nil)
}

func (s *Service) RemoteModerate(ctx context.Context, origin string, spaceID int64, user *auth.User, action string, targetID int64, reason string) error {
	fid, err := s.fidOfLocal(ctx, targetID)
	if err != nil {
		return err
	}
	return s.manage(ctx, origin, spaceID, user, "member."+action, manageMember{User: fid, Reason: reason}, nil)
}

func (s *Service) RemoteListBans(ctx context.Context, origin string, spaceID int64, user *auth.User) ([]spaces.SpaceBan, map[int64]*auth.User, error) {
	var res manageBans
	if err := s.manage(ctx, origin, spaceID, user, "bans.list", nil, &res); err != nil {
		return nil, nil, err
	}
	bans := make([]spaces.SpaceBan, 0, len(res.Bans))
	users := map[int64]*auth.User{}
	for _, b := range res.Bans {
		u, err := s.resolveProfile(ctx, b.User, origin)
		if err != nil {
			continue
		}
		users[u.ID] = u
		row := spaces.SpaceBan{SpaceID: spaceID, UserID: u.ID, Reason: b.Reason, CreatedAt: b.CreatedAt}
		if b.BannedBy != nil {
			if by, err := s.resolveProfile(ctx, *b.BannedBy, origin); err == nil {
				users[by.ID] = by
				row.BannedBy = by.ID
			}
		}
		bans = append(bans, row)
	}
	return bans, users, nil
}

func (s *Service) RemoteListInvites(ctx context.Context, origin string, spaceID int64, user *auth.User) ([]spaces.SpaceInvite, map[int64]*auth.User, error) {
	var res manageInvites
	if err := s.manage(ctx, origin, spaceID, user, "invites.list", nil, &res); err != nil {
		return nil, nil, err
	}
	invites := make([]spaces.SpaceInvite, 0, len(res.Invites))
	users := map[int64]*auth.User{}
	for _, inv := range res.Invites {
		row := spaces.SpaceInvite{Code: spaces.FormatInviteCode(inv.Code, origin), SpaceID: spaceID, MaxUses: inv.MaxUses, CurrentUses: inv.CurrentUses, ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt}
		if inv.Inviter != nil {
			if u, err := s.resolveProfile(ctx, *inv.Inviter, origin); err == nil {
				users[u.ID] = u
				row.InviterID = u.ID
			}
		}
		invites = append(invites, row)
	}
	return invites, users, nil
}

func (s *Service) RemoteDeleteInvite(ctx context.Context, origin string, spaceID int64, user *auth.User, code string) error {
	return s.manage(ctx, origin, spaceID, user, "invite.delete", manageInvite{Code: code}, nil)
}

func (s *Service) RemoteListAuditLog(ctx context.Context, origin string, spaceID int64, user *auth.User, beforeID int64, limit int, action string) ([]spaces.SpaceAuditEntry, map[int64]*auth.User, error) {
	p := manageAudit{Limit: limit, Action: action}
	if beforeID > 0 {
		p.Before = id.Format(beforeID)
	}
	var res manageAuditResult
	if err := s.manage(ctx, origin, spaceID, user, "audit.list", p, &res); err != nil {
		return nil, nil, err
	}
	users := map[int64]*auth.User{}
	byFID := map[string]*auth.User{}
	for _, prof := range res.Users {
		if u, err := s.resolveProfile(ctx, prof, origin); err == nil {
			users[u.ID] = u
			byFID[prof.FID] = u
		}
	}
	localRoom := func(raw string) string {
		oid, err := id.Parse(raw)
		if err != nil {
			return raw
		}
		local, err := s.localRoomID(ctx, origin, oid, false)
		if err != nil {
			return raw
		}
		return id.Format(local)
	}
	entries := make([]spaces.SpaceAuditEntry, 0, len(res.Entries))
	for _, e := range res.Entries {
		eid, err := id.Parse(e.ID)
		if err != nil {
			continue
		}
		row := spaces.SpaceAuditEntry{SpaceID: spaceID, ID: eid, ActionType: e.ActionType, TargetID: e.TargetID, Changes: string(e.Changes), Reason: e.Reason, CreatedAt: e.CreatedAt}
		if u := byFID[e.User]; u != nil {
			row.UserID = u.ID
		}
		switch {
		case spaces.IsUserTargetAction(e.ActionType):
			if u := byFID[e.TargetID]; u != nil {
				row.TargetID = id.Format(u.ID)
			}
		case strings.HasPrefix(e.ActionType, "room_"):
			row.TargetID = localRoom(e.TargetID)
		case strings.HasPrefix(e.ActionType, "override_"):
			if parts := strings.SplitN(e.TargetID, ":", 3); len(parts) == 3 {
				subject := parts[2]
				if parts[1] == "user" {
					if u := byFID[subject]; u != nil {
						subject = id.Format(u.ID)
					}
				}
				row.TargetID = localRoom(parts[0]) + ":" + parts[1] + ":" + subject
			}
		}
		entries = append(entries, row)
	}
	return entries, users, nil
}

func (s *Service) RemoteCreateRoom(ctx context.Context, origin string, spaceID int64, user *auth.User, in *spaces.CreateRoomInput) (*rooms.Room, error) {
	p := manageRoomCreate{Name: in.Name, Type: in.Type, E2EEEnabled: in.E2EEEnabled, UserLimit: in.UserLimit, Bitrate: in.Bitrate}
	if in.ParentID != nil {
		oid, err := s.originRoomID(ctx, origin, *in.ParentID)
		if err != nil {
			return nil, err
		}
		p.Parent = oid
	}
	var w SpaceRoomWire
	if err := s.manage(ctx, origin, spaceID, user, "room.create", p, &w); err != nil {
		return nil, err
	}
	oid, err := id.Parse(w.ID)
	if err != nil {
		return nil, spaces.ErrOriginUnavailable
	}
	local, err := s.localRoomID(ctx, origin, oid, false)
	if err != nil {
		// The relay that would have stored it was not in the reply: store it from the
		// result instead.
		mr, err := s.mirrorRoom(ctx, origin, w, true)
		if err != nil {
			return nil, err
		}
		if err := s.spaceSvc.ApplyMirrorRoom(ctx, spaceID, mr, false); err != nil {
			return nil, err
		}
		local = mr.Room.ID
	}
	room, err := s.roomRepo.GetByID(ctx, local)
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, spaces.ErrInvalidRoom
	}
	return room, nil
}

func (s *Service) RemoteUpdateRoom(ctx context.Context, origin string, spaceID int64, user *auth.User, roomID int64, in *spaces.UpdateRoomInput) error {
	oid, err := s.originRoomID(ctx, origin, roomID)
	if err != nil {
		return err
	}
	return s.manage(ctx, origin, spaceID, user, "room.update", manageRoomUpdate{Room: oid, Update: *in}, nil)
}

func (s *Service) RemoteDeleteRoom(ctx context.Context, origin string, spaceID int64, user *auth.User, roomID int64) error {
	oid, err := s.originRoomID(ctx, origin, roomID)
	if err != nil {
		return err
	}
	return s.manage(ctx, origin, spaceID, user, "room.delete", manageRoom{Room: oid}, nil)
}

func (s *Service) RemoteReorderRooms(ctx context.Context, origin string, spaceID int64, user *auth.User, in *spaces.ReorderRoomsInput) error {
	p := manageReorder{Scope: in.Scope, Rooms: make([]string, 0, len(in.RoomIDs))}
	if in.ParentSectionID != nil && strings.TrimSpace(*in.ParentSectionID) != "" {
		local, err := id.Parse(strings.TrimSpace(*in.ParentSectionID))
		if err != nil {
			return spaces.ErrInvalidReorder
		}
		oid, err := s.originRoomID(ctx, origin, local)
		if err != nil {
			return spaces.ErrInvalidReorder
		}
		p.Parent = oid
	}
	for _, raw := range in.RoomIDs {
		local, err := id.Parse(strings.TrimSpace(raw))
		if err != nil {
			return spaces.ErrInvalidReorder
		}
		oid, err := s.originRoomID(ctx, origin, local)
		if err != nil {
			return spaces.ErrInvalidReorder
		}
		p.Rooms = append(p.Rooms, oid)
	}
	return s.manage(ctx, origin, spaceID, user, "rooms.reorder", p, nil)
}

func (s *Service) RemoteMoveChannel(ctx context.Context, origin string, spaceID int64, user *auth.User, channelID int64, parent, before *int64) error {
	oid, err := s.originRoomID(ctx, origin, channelID)
	if err != nil {
		return err
	}
	p := manageMove{Channel: oid}
	if parent != nil {
		if p.Parent, err = s.originRoomID(ctx, origin, *parent); err != nil {
			return spaces.ErrInvalidReorder
		}
	}
	if before != nil {
		if p.Before, err = s.originRoomID(ctx, origin, *before); err != nil {
			return spaces.ErrInvalidReorder
		}
	}
	return s.manage(ctx, origin, spaceID, user, "room.move", p, nil)
}

func (s *Service) RemoteOverride(ctx context.Context, origin string, spaceID int64, user *auth.User, roomID, roleID, targetUserID int64, allow, deny int64, remove bool) error {
	oid, err := s.originRoomID(ctx, origin, roomID)
	if err != nil {
		return err
	}
	p := manageOverride{Room: oid, Allow: allow, Deny: deny}
	switch {
	case roleID != 0:
		p.Role = id.Format(roleID)
	case targetUserID != 0:
		if p.User, err = s.fidOfLocal(ctx, targetUserID); err != nil {
			return err
		}
	default:
		return spaces.ErrRoleNotFound
	}
	op := "override.put"
	if remove {
		op = "override.delete"
	}
	return s.manage(ctx, origin, spaceID, user, op, p, nil)
}

func (s *Service) RemoteTransferOwnership(ctx context.Context, origin string, spaceID int64, user *auth.User, newOwnerID int64) (*spaces.Space, error) {
	fid, err := s.fidOfLocal(ctx, newOwnerID)
	if err != nil {
		return nil, err
	}
	if err := s.manage(ctx, origin, spaceID, user, "space.transfer", manageTransfer{User: fid}, nil); err != nil {
		return nil, err
	}
	return s.spaceSvc.GetSpace(ctx, spaceID)
}

func (s *Service) RemoteDeleteSpace(ctx context.Context, origin string, spaceID int64, user *auth.User, confirmName string) error {
	return s.manage(ctx, origin, spaceID, user, "space.delete", manageDelete{Name: confirmName}, nil)
}

func (s *Service) remoteEmoji(ctx context.Context, origin string, spaceID int64, user *auth.User, op string, params any) (*spaces.SpaceEmoji, error) {
	var w SpaceEmojiWire
	if err := s.manage(ctx, origin, spaceID, user, op, params, &w); err != nil {
		return nil, err
	}
	e, err := s.emojiFromWire(ctx, w)
	if err != nil {
		return nil, err
	}
	return emojiRow(spaceID, e), nil
}

func (s *Service) RemoteCreateEmoji(ctx context.Context, origin string, spaceID int64, user *auth.User, emojiID int64, name, url string, animated bool) (*spaces.SpaceEmoji, error) {
	return s.remoteEmoji(ctx, origin, spaceID, user, "emoji.create", manageEmojiCreate{ID: id.Format(emojiID), Name: name, URL: url, Animated: animated})
}

func (s *Service) RemoteRenameEmoji(ctx context.Context, origin string, spaceID int64, user *auth.User, emojiID int64, name string) (*spaces.SpaceEmoji, error) {
	return s.remoteEmoji(ctx, origin, spaceID, user, "emoji.rename", manageEmojiRename{Emoji: id.Format(emojiID), Name: name})
}

func (s *Service) RemoteDeleteEmoji(ctx context.Context, origin string, spaceID int64, user *auth.User, emojiID int64) (*spaces.SpaceEmoji, error) {
	return s.remoteEmoji(ctx, origin, spaceID, user, "emoji.delete", manageEmoji{Emoji: id.Format(emojiID)})
}

// ---- sync ---------------------------------------------------------------------------------

func (s *Service) RemoteInstallBot(ctx context.Context, origin string, spaceID int64, user, bot *auth.User, permissions int64) (int64, error) {
	var out manageGranted
	if err := s.manage(ctx, origin, spaceID, user, "bot.install", manageBotInstall{Bot: s.ProfileOf(bot), Permissions: permissions}, &out); err != nil {
		return 0, err
	}
	return out.Granted, nil
}

func (s *Service) RemoteAddMember(ctx context.Context, origin string, spaceID int64, actor, user *auth.User) (bool, error) {
	var out manageAdded
	if err := s.manage(ctx, origin, spaceID, actor, "member.add", manageMemberAdd{User: s.ProfileOf(user)}, &out); err != nil {
		return false, err
	}
	return out.Added, nil
}

// SpaceSyncRequest: POST /spaces/sync - a mirror asks for a fresh snapshot.
type SpaceSyncRequest struct {
	Space SpaceRef `json:"space"`
}

// SpaceSync POST /spaces/sync.
func (h *Handler) SpaceSync(c fiber.Ctx) error {
	var body SpaceSyncRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sp, err := h.hostedSpace(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	snap, err := h.svc.snapshot(c.Context(), sp)
	if err != nil {
		return fail(c, err, map[string]any{"space_id": sp.ID})
	}
	return c.JSON(snap)
}

// ResyncMirror reconciles one mirror against its origin's current snapshot. A space the
// origin no longer has is dropped here too.
func (s *Service) ResyncMirror(ctx context.Context, m *SpaceMapping) error {
	origin := m.OriginDomain
	ref := SpaceRef{OriginDomain: origin, OriginSpaceID: id.Format(m.OriginSpaceID)}
	var snap SpaceSnapshot
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/sync", SpaceSyncRequest{Space: ref}, &snap); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			s.dropMirror(ctx, m)
			return nil
		}
		return err
	}
	if snap.Space.OriginDomain != origin || snap.Space.OriginSpaceID != ref.OriginSpaceID {
		return errSpaceNotMirror
	}
	s.completeSnapshot(ctx, origin, &snap)
	spec, err := s.mirrorSpec(ctx, origin, &snap, m.SpaceID)
	if err != nil {
		return err
	}
	removed, err := s.spaceSvc.ReconcileMirror(ctx, spec)
	if err != nil {
		return err
	}
	s.storeMirrorPeers(ctx, m.SpaceID, origin, snap.Peers)
	if len(removed) > 0 {
		if ms, err := s.repo.GetRoomMappings(ctx, removed); err == nil {
			for _, rm := range ms {
				_ = s.repo.DeleteRoomMapping(ctx, rm)
			}
		}
	}
	s.dropMirrorIfEmpty(ctx, m)
	return nil
}

// Maintenance cadence: the first pass soon after start (an instance that was down has
// the most to catch up on), then at a slow steady rate - relays carry the live changes.
const (
	maintenanceInitialDelay = 90 * time.Second
	maintenanceInterval     = 30 * time.Minute
)

// StartMaintenance makes this process the one that delivers queued relays (outbox.go) and
// runs ResyncMirror for every mirrored space on a timer until ctx ends.
func (s *Service) StartMaintenance(ctx context.Context) {
	s.startOutbox(ctx)
	safego.Go("federation", func() {
		timer := time.NewTimer(maintenanceInitialDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			s.resyncAllMirrors(ctx)
			timer.Reset(maintenanceInterval)
		}
	})
}

func (s *Service) resyncAllMirrors(ctx context.Context) {
	list, err := s.repo.ListSpaceMappings(ctx)
	if err != nil {
		logger.Err("federation", err, map[string]any{"stage": "resync"})
		return
	}
	n := 0
	for i := range list {
		m := list[i]
		if s.IsLocalServer(m.OriginDomain) || !s.fcfg.IsAllowedPeer(m.OriginDomain) {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if err := s.ResyncMirror(rctx, &m); err != nil {
			logger.Warn("federation", "resync of space %d from %s: %v", m.SpaceID, m.OriginDomain, err)
		} else {
			n++
		}
		cancel()
	}
	if n > 0 {
		logger.Info("federation", "resynced %d mirrored space(s)", n)
	}
}

// ResyncRemote is spaces.Federator's resync of one mirror.
func (s *Service) ResyncRemote(ctx context.Context, origin string, spaceID int64) error {
	m, err := s.repo.GetSpaceMapping(ctx, spaceID)
	if err != nil {
		return err
	}
	if m == nil || m.OriginDomain != origin {
		return spaces.ErrSpaceNotFound
	}
	if err := s.ResyncMirror(ctx, m); err != nil {
		return spaceOriginError(err, origin, "/spaces/sync")
	}
	return nil
}
