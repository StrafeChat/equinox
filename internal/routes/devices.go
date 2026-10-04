package routes

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/devices"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// relationshipChecker implements devices.RelationshipChecker by combining the rooms and
// spaces repos: two users can exchange keys if they already share a PM/group PM, or a space.
type relationshipChecker struct {
	rooms  rooms.Repository
	spaces *spaces.Service
	users  auth.UserRepository
}

func (c *relationshipChecker) CanExchangeKeys(ctx context.Context, userA, userB int64) (bool, error) {
	// A block ends key exchange too: no new Olm sessions (and so no new E2EE messages) can be
	// set up with someone who has blocked you, whatever rooms or spaces are shared.
	if c.users != nil {
		ua, err := c.users.GetByID(ctx, userA)
		if err != nil {
			return false, err
		}
		ub, err := c.users.GetByID(ctx, userB)
		if err != nil {
			return false, err
		}
		if ua != nil && ub != nil {
			for _, v := range ua.Blocks {
				if v == userB {
					return false, nil
				}
			}
			for _, v := range ub.Blocks {
				if v == userA {
					return false, nil
				}
			}
		}
	}
	if room, err := c.rooms.GetPMRoom(ctx, userA, userB); err != nil {
		return false, err
	} else if room != nil {
		return true, nil
	}
	spacesA, err := c.spaces.ListSpacesForUser(ctx, userA)
	if err != nil {
		return false, err
	}
	if len(spacesA) == 0 {
		return false, nil
	}
	spacesB, err := c.spaces.ListSpacesForUser(ctx, userB)
	if err != nil {
		return false, err
	}
	setA := make(map[int64]struct{}, len(spacesA))
	for _, s := range spacesA {
		setA[s.SpaceID] = struct{}{}
	}
	for _, s := range spacesB {
		if _, ok := setA[s.SpaceID]; ok {
			return true, nil
		}
	}
	return false, nil
}

func SetupDevicesRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	devRepo := devices.NewRepository(d.Scylla)
	devSvc := devices.NewService(devRepo, d.Redis, d.Config)

	roomRepo := rooms.NewRepository(d.Scylla)
	spaceRepo := spaces.NewRepository(d.Scylla)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, d.Redis, d.Config)
	rel := &relationshipChecker{rooms: roomRepo, spaces: spaceSvc, users: userRepo}

	devHandler := devices.NewHandler(devSvc, rel)
	if d.Federation != nil {
		// Key queries/claims/to-device for @id:otherdomain users are forwarded there;
		// reachability checks resolve those ids to the shadow rows the caller shares a
		// room with.
		devSvc.SetRouter(d.Federation)
		devHandler.SetResolver(d.Federation)
	}

	// Key-exchange endpoints see real traffic (a group message may need to claim/query
	// keys for many devices at once) but must stay far below what'd let a script
	// enumerate/drain every user on the platform - unlike /auth's 10/min, this budget is
	// per-endpoint-class and more generous, since legitimate use is bursty by nature.
	keysLimiter := limiter.New(limiter.Config{
		Max:        60,
		Expiration: time.Minute,
	})
	// Recovery-material endpoints: the wrapped backup key, and the legacy PIN blob. Rate
	// limiting only protects the *fetch* here (everything after it happens client-side), so
	// this is tighter than the general key-exchange budget, matching /auth's login/register
	// limit. It matters much less for the asymmetric backup than it did for the PIN one - a
	// recovery code is 128 bits, so there is nothing to brute-force offline - but a fetch of
	// key material still has no legitimate reason to be hot.
	backupLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
	})

	r := d.App.Group("/devices", requireAuth)
	r.Post("", devHandler.CreateDevice)
	r.Get("", devHandler.ListDevices)

	// Static paths first: Fiber matches in registration order, so "/:device_id" below would
	// otherwise swallow DELETE /devices/backup as a revoke of a device called "backup".
	//
	// Asymmetric key backup. The version endpoints move recovery material, so they get the
	// tight budget; the keys endpoints are ordinary traffic - a device uploads room keys as
	// it acquires them, in batches - so they share the key-exchange budget.
	r.Post("/backup/version", backupLimiter, devHandler.CreateBackupVersion)
	// Public parameters only, and read on every sync - so it shares the ordinary key-exchange
	// budget. The recovery material it used to carry moved to /backup/recovery below.
	r.Get("/backup/version", keysLimiter, devHandler.GetBackupVersion)
	r.Get("/backup/recovery", backupLimiter, devHandler.GetBackupRecovery)
	r.Delete("/backup/version", backupLimiter, devHandler.DeleteBackupVersion)
	r.Put("/backup/keys", keysLimiter, devHandler.PutBackupKeys)
	r.Get("/backup/keys", keysLimiter, devHandler.GetBackupKeys)

	// Legacy PIN-wrapped backup: read and delete only, so accounts created before the
	// asymmetric scheme can be migrated over (see migration 027). Nothing writes one any more.
	r.Get("/backup", backupLimiter, devHandler.GetKeyBackup)
	r.Put("/backup", backupLimiter, devHandler.SetKeyBackup)
	r.Delete("/backup", backupLimiter, devHandler.DeleteLegacyKeyBackup)

	r.Post("/keys/query", keysLimiter, devHandler.QueryKeys)
	r.Post("/keys/claim", keysLimiter, devHandler.ClaimKeys)
	r.Put("/send_to_device/:event_type/:txn_id", keysLimiter, devHandler.SendToDevice)

	r.Delete("/:device_id", devHandler.RevokeDevice)
	r.Post("/:device_id/keys/upload", keysLimiter, devHandler.UploadKeys)
	r.Get("/:device_id/to_device", devHandler.PollToDevice)
	r.Post("/:device_id/to_device/ack", devHandler.AckToDevice)
}
