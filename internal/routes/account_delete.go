package routes

import (
	"context"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/devices"
	"github.com/StrafeChat/equinox/internal/modules/discover"
	"github.com/StrafeChat/equinox/internal/modules/instance"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/oauth"
	"github.com/StrafeChat/equinox/internal/modules/push"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/users"
	"github.com/StrafeChat/equinox/internal/nebula"
	"github.com/StrafeChat/equinox/internal/safego"
)

// wireAccountDeletion gives an instance service the account-deletion pipeline
// (instance/account_delete.go). Every module is reached through services wired the way
// their own routes wire them, so a deletion's leaves, unfriends and revocations post the
// same system lines, end the same calls and relay to the same peers a person's own would.
func wireAccountDeletion(d Deps, svc *instance.Service, userRepo auth.UserRepository) {
	region := "default"
	if d.Config.Stargate.Region != "" {
		region = d.Config.Stargate.Region
	}
	roomRepo := rooms.NewRepository(d.Scylla)
	msgRepo := messages.NewRepository(d.Scylla)
	onSystemEvent := newSystemMessenger(d, roomRepo, msgRepo)

	spaceSvc := spaces.NewService(spaces.NewRepository(d.Scylla), roomRepo, userRepo, d.Redis, d.Config)
	spaceSvc.SetSystemMessenger(spaces.SystemMessenger(onSystemEvent))
	roomSvc := rooms.NewService(roomRepo, userRepo, d.Redis, d.Config, onSystemEvent, spaceSvc)
	if d.Federation != nil {
		spaceSvc.SetFederator(d.Federation)
		roomSvc.SetFederator(d.Federation)
	}
	ev := newVoiceEvictor(d, roomSvc, spaceSvc, userRepo, onSystemEvent)
	if ev != nil {
		spaceSvc.SetMemberRemovedHook(ev.OnSpaceMemberRemoved)
		spaceSvc.SetRoomRemovedHook(ev.OnSpaceRoomRemoved)
		roomSvc.SetParticipantRemovedHook(ev.OnRoomParticipantRemoved)
	}

	relSvc := relationships.NewService(relationships.NewRepository(d.Scylla), userRepo, d.Redis, d.Config)
	if d.Federation != nil {
		relSvc.SetFederator(d.Federation)
	}
	appRepo := applications.NewRepository(d.Scylla)
	apps := newApplicationsService(d, appRepo, userRepo)

	deps := instance.AccountDeletionDeps{
		Spaces:        spaceSvc,
		Rooms:         roomRepo,
		Groups:        roomSvc,
		Relationships: relSvc,
		Devices:       devices.NewService(devices.NewRepository(d.Scylla), d.Redis, d.Config),
		OAuth:         oauth.NewService(oauth.NewRepository(d.Scylla), appRepo, userRepo),
		Push:          push.NewPurger(push.NewRepository(d.Scylla), d.Redis),
		Apps:          apps,
		Partitions:    instance.NewAccountPartitions(d.Scylla),
		Broadcast: func(ctx context.Context, u *auth.User) {
			users.BroadcastDeleted(ctx, d.Redis, roomRepo, region, u)
		},
		DeleteProfileImages: func(ctx context.Context, u *auth.User) { deleteProfileImages(ctx, d, u) },
	}
	if ev != nil {
		deps.Voice = ev
	}
	if d.Federation != nil {
		deps.Federation = d.Federation
	}
	svc.SetAccountDeletion(deps)
}

// newApplicationsService is an applications service as the developer routes build it, with
// the after-delete hook that takes a deleted application's Discover listing with it.
func newApplicationsService(d Deps, repo applications.Repository, userRepo auth.UserRepository) *applications.Service {
	svc := applications.NewService(repo, userRepo)
	if d.Redis != nil {
		svc.SetGateway(d.Redis, d.Config.Stargate.Region)
	}
	svc.SetAfterDelete(removeBotListing(d))
	return svc
}

// removeBotListing is the applications after-delete hook: a deleted application's Discover
// listing goes with it (the directory would otherwise drop it lazily on its next read).
func removeBotListing(d Deps) func(ctx context.Context, a *applications.Application) {
	discoverRepo := discover.NewRepository(d.Scylla)
	return func(ctx context.Context, a *applications.Application) {
		if err := discover.RemoveListing(ctx, discoverRepo, d.Redis, d.Config.Database.Redis.CachePrefix, discover.KindBot, a.ID); err != nil {
			logger.Err("applications", err, map[string]any{"stage": "discover_listing", "application_id": a.ID})
		}
	}
}

// deleteProfileImages removes a deleted account's current avatar and banner from this
// instance's object store - only objects under its own avatars/<id>/ or banners/<id>/ keys
// on this instance's CDN, never anything else a URL might point at. Older replaced uploads
// are not listable and stay. Off the request goroutine, best-effort.
func deleteProfileImages(ctx context.Context, d Deps, u *auth.User) {
	neb := d.Config.Nebula
	if strings.TrimSpace(neb.BaseURL) == "" || strings.TrimSpace(neb.UploadSecret) == "" || u == nil {
		return
	}
	bases := []string{nebula.PublicBase(neb.BaseURL, neb.PublicURL), strings.TrimRight(strings.TrimSpace(neb.BaseURL), "/")}
	var keys []string
	for _, url := range []string{u.Avatar, u.Banner} {
		if key := ownProfileImageKey(bases, u.ID, url); key != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}
	bg := context.WithoutCancel(ctx)
	safego.Go("instance", func() {
		ctx, cancel := context.WithTimeout(bg, 15*time.Second)
		defer cancel()
		for _, key := range keys {
			if err := nebula.Delete(ctx, neb.BaseURL, neb.UploadSecret, key); err != nil {
				logger.Err("instance", err, map[string]any{"stage": "account_delete_image", "user_id": u.ID})
			}
		}
	})
}

// ownProfileImageKey is the object key of url when it is one of this account's own profile
// uploads on this instance (<base>/v1/avatars/<id>/... or .../banners/<id>/...), else "".
func ownProfileImageKey(bases []string, userID int64, url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	for _, base := range bases {
		if base == "" || !strings.HasPrefix(url, base+"/v1/") {
			continue
		}
		key := nebula.KeyFromURL(url)
		uid := id.Format(userID)
		// Only a plain key under the account's own prefix: no dot segments, no escapes or
		// query that the object store could decode into a different path.
		if (strings.HasPrefix(key, "avatars/"+uid+"/") || strings.HasPrefix(key, "banners/"+uid+"/")) &&
			!strings.Contains(key, "..") && !strings.ContainsAny(key, "%\\?#") {
			return key
		}
	}
	return ""
}
