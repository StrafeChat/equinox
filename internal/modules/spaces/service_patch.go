package spaces

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	ErrInvalidSystemRoom  = errors.New("system messages room must be a text room in this space")
	ErrInvalidAFKRoom     = errors.New("inactive room must be a voice room in this space")
	ErrInvalidAFKTimeout  = errors.New("inactive timeout must be 1, 5, 15, 30 or 60 minutes")
	ErrInvalidNotifLevel  = errors.New("default notifications must be 0 (all messages) or 1 (only mentions)")
	ErrInvalidSystemFlags = errors.New("unknown system message flag")
	ErrInvalidWidgetRoom  = errors.New("widget invite room must be a text room in this space")
)

// roomInSpaceOfType resolves a string room id and checks it belongs to the space and has
// the wanted type. An empty id clears the setting (nil, nil).
func (s *Service) roomInSpaceOfType(ctx context.Context, spaceID int64, raw string, wantType int, bad error) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	rid, err := id.Parse(raw)
	if err != nil {
		return nil, bad
	}
	room, err := s.roomRepo.GetByID(ctx, rid)
	if err != nil {
		return nil, err
	}
	if room == nil || room.SpaceID == nil || *room.SpaceID != spaceID || room.Type != wantType {
		return nil, bad
	}
	return &rid, nil
}

func fmtRoomID(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return id.Format(*p)
}

// PatchSpace updates space settings. Requires Manage space, Administrator, or owner.
// Every field is validated against the space (rooms must belong to it and be of the right
// type), only changed columns are written, and the change set is audited.
func (s *Service) PatchSpace(ctx context.Context, actorID, spaceID int64, in *PatchSpaceInput) (*Space, error) {
	if in == nil {
		return nil, ErrNothingToPatch
	}
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemotePatchSpace(ctx, origin, spaceID, actor, in)
	}
	base, err := s.SpacePermissionBase(ctx, actorID, spaceID)
	if err != nil {
		return nil, err
	}
	if !permissions.Has(base, permissions.PermManageSpace) && !permissions.Has(base, permissions.PermAdministrator) {
		return nil, ErrInsufficientSpacePermission
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, ErrSpaceNotFound
	}

	set := map[string]interface{}{}
	changes := map[string]change{}

	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || utf8.RuneCountInString(name) > MaxSpaceNameRunes {
			return nil, ErrInvalidSpaceName
		}
		if name != sp.Name {
			set["name"] = name
			set["name_acronym"] = nameAcronym(name)
			diff(changes, "name", sp.Name, name)
		}
	}
	if in.Description != nil {
		desc := strings.TrimSpace(*in.Description)
		if utf8.RuneCountInString(desc) > MaxSpaceDescriptionRunes {
			return nil, ErrInvalidDescription
		}
		if desc != sp.Description {
			set["description"] = desc
			diff(changes, "description", sp.Description, desc)
		}
	}
	if in.SystemRoomID != nil {
		rid, err := s.roomInSpaceOfType(ctx, spaceID, *in.SystemRoomID, rooms.TypeSpaceText, ErrInvalidSystemRoom)
		if err != nil {
			return nil, err
		}
		if fmtRoomID(rid) != fmtRoomID(sp.SystemRoomID) {
			set["system_room_id"] = nullableID(rid)
			diff(changes, "system_room_id", fmtRoomID(sp.SystemRoomID), fmtRoomID(rid))
		}
	}
	if in.SystemRoomFlags != nil {
		f := *in.SystemRoomFlags
		if f < 0 || f&^systemRoomFlagsAll != 0 {
			return nil, ErrInvalidSystemFlags
		}
		if f != sp.SystemRoomFlags {
			set["system_room_flags"] = f
			diff(changes, "system_room_flags", sp.SystemRoomFlags, f)
		}
	}
	if in.DefaultMessageNotif != nil {
		n := *in.DefaultMessageNotif
		if n != DefaultNotifAllMessages && n != DefaultNotifOnlyMentions {
			return nil, ErrInvalidNotifLevel
		}
		if n != sp.DefaultMessageNotif {
			set["default_message_notifications"] = n
			diff(changes, "default_message_notifications", sp.DefaultMessageNotif, n)
		}
	}
	if in.AFKRoomID != nil {
		rid, err := s.roomInSpaceOfType(ctx, spaceID, *in.AFKRoomID, rooms.TypeSpaceVoice, ErrInvalidAFKRoom)
		if err != nil {
			return nil, err
		}
		if fmtRoomID(rid) != fmtRoomID(sp.AFKRoomID) {
			set["afk_room_id"] = nullableID(rid)
			diff(changes, "afk_room_id", fmtRoomID(sp.AFKRoomID), fmtRoomID(rid))
		}
	}
	if in.AFKTimeout != nil {
		t := *in.AFKTimeout
		if !AFKTimeouts[t] {
			return nil, ErrInvalidAFKTimeout
		}
		if t != sp.AFKTimeout {
			set["afk_timeout"] = t
			diff(changes, "afk_timeout", sp.AFKTimeout, t)
		}
	}
	if in.WidgetRoomID != nil {
		rid, err := s.roomInSpaceOfType(ctx, spaceID, *in.WidgetRoomID, rooms.TypeSpaceText, ErrInvalidWidgetRoom)
		if err != nil {
			return nil, err
		}
		if fmtRoomID(rid) != fmtRoomID(sp.WidgetRoomID) {
			set["widget_room_id"] = nullableID(rid)
			diff(changes, "widget_room_id", fmtRoomID(sp.WidgetRoomID), fmtRoomID(rid))
			// The widget invite is tied to the room it was made for; drop it so the next
			// widget read mints one for the new room (or none, when cleared).
			if sp.WidgetInviteCode != "" {
				_ = s.repo.DeleteInvite(ctx, spaceID, sp.WidgetInviteCode)
				set["widget_invite_code"] = ""
			}
		}
	}
	if in.WidgetEnabled != nil && *in.WidgetEnabled != sp.WidgetEnabled {
		set["widget_enabled"] = *in.WidgetEnabled
		diff(changes, "widget_enabled", sp.WidgetEnabled, *in.WidgetEnabled)
	}

	if len(set) == 0 {
		if in.Name == nil && in.Description == nil && in.SystemRoomID == nil && in.SystemRoomFlags == nil &&
			in.DefaultMessageNotif == nil && in.AFKRoomID == nil && in.AFKTimeout == nil &&
			in.WidgetEnabled == nil && in.WidgetRoomID == nil {
			return nil, ErrNothingToPatch
		}
		// Everything supplied already matched: nothing to write, nothing to announce.
		return sp, nil
	}
	set["updated_at"] = time.Now().UTC()
	if err := s.repo.UpdateSpaceFields(ctx, spaceID, set); err != nil {
		return nil, err
	}
	s.audit(ctx, spaceID, actorID, AuditSpaceUpdate, id.Format(spaceID), changes, "")
	sp, err = s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	if s.redis != nil {
		stargate.PublishToSpace(ctx, s.redis, spaceID, "SPACE_UPDATE", spaceToEventPayload(sp), s.stargateRegion())
	}
	s.fedSpaceUpdated(ctx, sp)
	return sp, nil
}

// nullableID turns an optional id into a bind value: the int64 itself, or SQL null.
func nullableID(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
