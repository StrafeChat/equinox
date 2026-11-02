package users

import (
	"context"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// broadcastUserProfile sends USER_UPDATE over Redis so Stargate fans out to subscribers (self, friends, shared rooms).
func (h *Handler) broadcastUserProfile(ctx context.Context, u *auth.User) {
	if u != nil && h.federator != nil {
		h.federator.AfterProfileUpdated(ctx, u)
	}
	if h.redis == nil || u == nil {
		return
	}
	region := "default"
	if h.cfg != nil && h.cfg.Stargate.Region != "" {
		region = h.cfg.Stargate.Region
	}
	payload := map[string]interface{}{
		"user_id":      id.Format(u.ID),
		"avatar":       u.Avatar,
		"banner":       u.Banner,
		"display_name": u.DisplayName,
		"username":     u.Username,
		"bio":          u.Bio,
		"about_me":     u.AboutMe,
		"accent_color": u.AccentColor,
		"bot":          u.Bot,
	}
	auth.MergeProfilePublicExtras(payload, u)
	stargate.PublishToUser(ctx, h.redis, u.ID, "USER_UPDATE", payload, region)
	if len(u.Relationships) > 0 {
		stargate.PublishToUsers(ctx, h.redis, u.Relationships, "USER_UPDATE", payload, region)
	}
	if h.roomsRepo == nil {
		return
	}
	rows, err := h.roomsRepo.ListByUser(ctx, u.ID)
	if err != nil || len(rows) == 0 {
		return
	}
	seen := make(map[int64]struct{})
	roomIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.RoomID == 0 {
			continue
		}
		if _, ok := seen[row.RoomID]; ok {
			continue
		}
		seen[row.RoomID] = struct{}{}
		roomIDs = append(roomIDs, row.RoomID)
	}
	// Each copy names its room's space; the rooms are read in one go, and a room the read
	// misses still gets its copy, scoped to the room alone.
	spaceOf := make(map[int64]*int64, len(roomIDs))
	if list, err := h.roomsRepo.GetByIDs(ctx, roomIDs); err == nil {
		for _, r := range list {
			if r != nil {
				spaceOf[r.ID] = r.SpaceID
			}
		}
	}
	for _, rid := range roomIDs {
		stargate.PublishToRoom(ctx, h.redis, stargate.InRoom(spaceOf[rid], rid), "USER_UPDATE", payload, region)
	}
}
