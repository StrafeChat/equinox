package users

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// DeletedUserPayload is the USER_UPDATE an account's audience gets when the account is
// deleted: the flag and every profile field blanked, so a client replaces what it shows
// with its "Deleted User" placeholder without a reload.
func DeletedUserPayload(userID int64) map[string]interface{} {
	return map[string]interface{}{
		"user_id":      id.Format(userID),
		"deleted":      true,
		"username":     "",
		"display_name": "",
		"avatar":       "",
		"banner":       "",
		"bio":          "",
		"about_me":     "",
		"pronouns":     "",
	}
}

// BroadcastDeleted tells the people who could see an account that it was deleted: its
// friends and every room it is in - the audience of a profile update - but without the
// federation profile relay (peers get the account-deleted notice instead). u is the
// account as it was read before anything was torn down.
func BroadcastDeleted(ctx context.Context, rdb *redis.Client, roomsRepo rooms.Repository, region string, u *auth.User) {
	if rdb == nil || u == nil {
		return
	}
	publishToProfileAudience(ctx, rdb, roomsRepo, region, u, DeletedUserPayload(u.ID), false)
}
