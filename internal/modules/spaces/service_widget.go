package spaces

import (
	"context"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// WidgetInfo is the public widget document for a space: enough to render a "join us"
// card outside Strafe. Nothing member-identifying is included.
type WidgetInfo struct {
	ID            int64
	Name          string
	Icon          string
	Description   string
	MemberCount   int
	PresenceCount int
	// InviteCode is empty when no widget room is configured.
	InviteCode string
	RoomName   string
}

// Widget builds the public widget document. ErrSpaceNotFound when the space doesn't
// exist or its widget is disabled - a disabled widget must look identical to a missing
// space to an outsider.
func (s *Service) Widget(ctx context.Context, spaceID int64) (*WidgetInfo, error) {
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil || !sp.WidgetEnabled {
		return nil, ErrSpaceNotFound
	}
	members, err := s.repo.ListMembers(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	info := &WidgetInfo{
		ID:          sp.ID,
		Name:        sp.Name,
		Icon:        sp.Icon,
		Description: sp.Description,
		MemberCount: len(members),
	}
	if len(members) > 0 {
		ids := make([]int64, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.UserID)
		}
		if users, err := s.userRepo.GetByIDs(ctx, ids); err == nil {
			for _, u := range users {
				if u != nil && auth.ToPublicPresence(u.Presence, true).Status != "offline" {
					info.PresenceCount++
				}
			}
		}
	}
	if sp.WidgetRoomID != nil {
		if room, err := s.roomRepo.GetByID(ctx, *sp.WidgetRoomID); err == nil && room != nil {
			info.RoomName = room.Name
		}
		code, err := s.ensureWidgetInvite(ctx, sp)
		if err != nil {
			return nil, err
		}
		info.InviteCode = code
	}
	return info, nil
}

// ensureWidgetInvite returns the space's widget invite, minting a permanent one (in the
// owner's name) when there isn't a live one yet.
func (s *Service) ensureWidgetInvite(ctx context.Context, sp *Space) (string, error) {
	if sp.WidgetInviteCode != "" {
		inv, err := s.repo.GetInviteByCode(ctx, sp.WidgetInviteCode)
		if err != nil {
			return "", err
		}
		if inv != nil && inv.SpaceID == sp.ID {
			return inv.Code, nil
		}
	}
	inv, err := s.mintInvite(ctx, sp.ID, sp.OwnerID, nil)
	if err != nil {
		return "", err
	}
	if err := s.repo.UpdateSpaceFields(ctx, sp.ID, map[string]interface{}{"widget_invite_code": inv.Code}); err != nil {
		return "", err
	}
	sp.WidgetInviteCode = inv.Code
	return inv.Code, nil
}
