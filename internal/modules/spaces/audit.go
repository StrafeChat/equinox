package spaces

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

// Audit log action types (space_audit_log.action_type). Discord-style naming; the client
// maps each to a sentence. Targets: member_* -> user id, role_* -> role id, room_* ->
// room id, invite_* -> invite code, emoji_* -> emoji id, override_* -> "room:subject".
const (
	AuditSpaceUpdate       = "space_update"
	AuditRoomCreate        = "room_create"
	AuditRoomUpdate        = "room_update"
	AuditRoomDelete        = "room_delete"
	AuditRoleCreate        = "role_create"
	AuditRoleUpdate        = "role_update"
	AuditRoleDelete        = "role_delete"
	AuditMemberKick        = "member_kick"
	AuditMemberBanAdd      = "member_ban_add"
	AuditMemberBanRemove   = "member_ban_remove"
	AuditMemberRolesUpdate = "member_roles_update"
	AuditInviteCreate      = "invite_create"
	AuditInviteDelete      = "invite_delete"
	AuditEmojiCreate       = "emoji_create"
	AuditEmojiUpdate       = "emoji_update"
	AuditEmojiDelete       = "emoji_delete"
	AuditOverrideUpdate    = "override_update"
	AuditOverrideDelete    = "override_delete"
	// A bot installed through an application's `bot` OAuth2 scope: target is the bot's
	// user id, changes carry the permissions its role was created with.
	AuditBotAdd = "bot_add"
	// Voice moderation (internal/modules/voice): target is the member acted on.
	AuditOwnershipTransfer = "ownership_transfer"

	AuditMemberVoiceMute       = "member_voice_mute"
	AuditMemberVoiceDeafen     = "member_voice_deafen"
	AuditMemberVoiceMove       = "member_voice_move"
	AuditMemberVoiceDisconnect = "member_voice_disconnect"
)

// userTargetActions are the actions whose target_id is a user id (hydrated on read).
var userTargetActions = map[string]bool{
	AuditMemberKick:            true,
	AuditMemberBanAdd:          true,
	AuditMemberBanRemove:       true,
	AuditMemberRolesUpdate:     true,
	AuditBotAdd:                true,
	AuditMemberVoiceMute:       true,
	AuditMemberVoiceDeafen:     true,
	AuditMemberVoiceMove:       true,
	AuditMemberVoiceDisconnect: true,
}

// AuditChange is one before/after pair for RecordAudit. Either side may be nil.
type AuditChange struct {
	Old interface{}
	New interface{}
}

// RecordAudit lets other modules (voice moderation) write to the space's audit log with
// the same shape as the space's own actions. Best-effort like audit().
func (s *Service) RecordAudit(ctx context.Context, spaceID, actorID int64, action, targetID string, changes map[string]AuditChange, reason string) {
	conv := make(map[string]change, len(changes))
	for k, v := range changes {
		conv[k] = change(v)
	}
	s.audit(ctx, spaceID, actorID, action, targetID, conv, reason)
}

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 100
)

// change is one before/after pair inside an audit entry's Changes object.
type change struct {
	Old interface{} `json:"old,omitempty"`
	New interface{} `json:"new,omitempty"`
}

// diff records a change only when the value actually changed.
func diff(changes map[string]change, key string, oldV, newV interface{}) {
	if oldV == newV {
		return
	}
	changes[key] = change{Old: oldV, New: newV}
}

// audit appends one entry to the space's log. Best-effort: a failure is logged, never
// surfaced - the action it describes has already happened.
func (s *Service) audit(ctx context.Context, spaceID, actorID int64, action, targetID string, changes map[string]change, reason string) {
	var enc string
	if len(changes) > 0 {
		if raw, err := json.Marshal(changes); err == nil {
			enc = string(raw)
		}
	}
	e := &SpaceAuditEntry{
		SpaceID:    spaceID,
		ID:         id.Next(),
		ActionType: action,
		UserID:     actorID,
		TargetID:   targetID,
		Changes:    enc,
		Reason:     strings.TrimSpace(reason),
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.repo.InsertAuditEntry(ctx, e); err != nil {
		logger.Err("spaces", err, map[string]any{"space_id": spaceID, "audit_action": action})
	}
}

// ListAuditLog returns a page of the space's audit log (newest first) plus every user it
// refers to (actors and user targets). Requires Manage space, Administrator, or owner.
func (s *Service) ListAuditLog(ctx context.Context, actorID, spaceID, beforeID int64, limit int, action string) ([]SpaceAuditEntry, map[int64]*auth.User, error) {
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, nil, err
	} else if origin != "" {
		return s.fed.RemoteListAuditLog(ctx, origin, spaceID, actor, beforeID, limit, action)
	}
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
		if !permissions.Has(base, permissions.PermManageSpace) && !permissions.Has(base, permissions.PermAdministrator) {
			return nil, nil, ErrMissingPerm
		}
	}
	if limit <= 0 {
		limit = auditDefaultLimit
	}
	if limit > auditMaxLimit {
		limit = auditMaxLimit
	}
	// An action filter is applied after the read, so page through a few extra rows to
	// keep filtered pages from coming back nearly empty.
	fetch := limit
	if action != "" {
		fetch = limit * 4
		if fetch > 400 {
			fetch = 400
		}
	}
	rows, err := s.repo.ListAuditEntries(ctx, spaceID, beforeID, fetch)
	if err != nil {
		return nil, nil, err
	}
	if action != "" {
		filtered := rows[:0]
		for _, r := range rows {
			if r.ActionType == action {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
		if len(rows) > limit {
			rows = rows[:limit]
		}
	}
	userIDs := make([]int64, 0, len(rows)*2)
	seen := make(map[int64]struct{}, len(rows)*2)
	add := func(uid int64) {
		if uid == 0 {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		userIDs = append(userIDs, uid)
	}
	for _, r := range rows {
		add(r.UserID)
		if userTargetActions[r.ActionType] {
			if uid, err := id.Parse(r.TargetID); err == nil {
				add(uid)
			}
		}
	}
	users := make(map[int64]*auth.User, len(userIDs))
	if len(userIDs) > 0 {
		list, err := s.userRepo.GetByIDs(ctx, userIDs)
		if err == nil {
			for _, u := range list {
				if u != nil {
					users[u.ID] = u
				}
			}
		}
	}
	return rows, users, nil
}
