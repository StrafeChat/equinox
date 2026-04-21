package users

import (
	"context"
	"fmt"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// broadcastUserProfile sends USER_UPDATE over Redis so Stargate fans out to subscribers (self, friends, shared rooms).
func (h *Handler) broadcastUserProfile(ctx context.Context, u *auth.User) {
	if h.redis == nil || u == nil {
		return
	}
	region := "default"
	if h.cfg != nil && h.cfg.Stargate.Region != "" {
		region = h.cfg.Stargate.Region
	}
	payload := map[string]interface{}{
		"user_id":       id.Format(u.ID),
		"avatar":        u.Avatar,
		"banner":        u.Banner,
		"display_name":  u.DisplayName,
		"username":      u.Username,
		"discriminator": fmt.Sprintf("%04d", u.Discriminator),
		"bio":           u.Bio,
		"about_me":      u.AboutMe,
		"accent_color":  u.AccentColor,
	}
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
	for _, row := range rows {
		if row.RoomID == 0 {
			continue
		}
		if _, ok := seen[row.RoomID]; ok {
			continue
		}
		seen[row.RoomID] = struct{}{}
		stargate.PublishToSpace(ctx, h.redis, row.RoomID, "USER_UPDATE", payload, region)
	}
}
