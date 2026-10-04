package spaces

import (
	"context"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
)

// Data migrations that run from Go, once per keyspace (claimed in data_migrations).
const dataMigrationVoicePermissions = "028_voice_default_permissions"

// BackfillVoicePermissions grants the default voice bits (connect, speak, video, voice
// activity) to every @everyone role created before voice shipped. New spaces get them
// through permissions.DefaultEveryone; without this, nobody in an existing space could
// join a voice room until an admin edited the role by hand. Custom roles are left alone:
// they only ever add to @everyone, so they need no change.
//
// Exactly once: the name is claimed with a lightweight transaction first, so a restart
// or a second API replica cannot re-grant bits an admin has since removed on purpose.
func (s *Service) BackfillVoicePermissions(ctx context.Context) error {
	claimed, err := s.repo.ClaimDataMigration(ctx, dataMigrationVoicePermissions)
	if err != nil || !claimed {
		return err
	}
	updated := 0
	err = s.repo.ForEachSpaceRole(ctx, func(spaceID, roleID int64, name string, perms int64) error {
		if name != EveryoneRoleName || perms&permissions.DefaultVoice == permissions.DefaultVoice {
			return nil
		}
		if err := s.repo.UpdateSpaceRolePermissions(ctx, spaceID, roleID, perms|permissions.DefaultVoice, time.Now().UTC()); err != nil {
			return err
		}
		s.invalidateSnapshot(ctx, spaceID)
		updated++
		return nil
	})
	if err != nil {
		// Give the claim back so the next start retries instead of leaving every existing
		// space without voice access forever. (Roles already updated are idempotent to
		// touch again: the OR is a no-op.)
		if relErr := s.repo.ReleaseDataMigration(ctx, dataMigrationVoicePermissions); relErr != nil {
			logger.Err("spaces", relErr, map[string]any{"data_migration": dataMigrationVoicePermissions})
		}
		return err
	}
	logger.Info("spaces", "voice permissions backfilled on %d @everyone role(s)", updated)
	return nil
}

// Data migration for the Attach Files bit (permissions.PermAttachFiles, 1<<23).
const dataMigrationAttachFiles = "043_attach_files_permission"

// BackfillAttachFilesPermission grants Attach Files everywhere Send Messages was already
// granted when the bit did not exist, so an existing space keeps behaving exactly as it
// did: every role that can send can attach; a room override that allows sending allows
// attaching, one that denies sending denies it. New spaces get the bit through
// permissions.DefaultEveryone. Exactly once per keyspace (claimed in data_migrations), and
// idempotent if ever re-run: the ORs are no-ops the second time.
func (s *Service) BackfillAttachFilesPermission(ctx context.Context) error {
	claimed, err := s.repo.ClaimDataMigration(ctx, dataMigrationAttachFiles)
	if err != nil || !claimed {
		return err
	}
	const send, attach = permissions.PermSendMessages, permissions.PermAttachFiles
	now := time.Now().UTC()
	spaceIDs := map[int64]struct{}{}
	roles := 0
	err = s.repo.ForEachSpaceRole(ctx, func(spaceID, roleID int64, _ string, perms int64) error {
		spaceIDs[spaceID] = struct{}{}
		if perms&send == 0 || perms&attach != 0 {
			return nil
		}
		if err := s.repo.UpdateSpaceRolePermissions(ctx, spaceID, roleID, perms|attach, now); err != nil {
			return err
		}
		roles++
		return nil
	})
	overrides := 0
	if err == nil {
	spaces:
		for spaceID := range spaceIDs {
			roomRows, lerr := s.roomRepo.ListBySpace(ctx, spaceID)
			if lerr != nil {
				err = lerr
				break
			}
			for _, r := range roomRows {
				n, oerr := s.backfillAttachOverrides(ctx, spaceID, r.RoomID, now)
				overrides += n
				if oerr != nil {
					err = oerr
					break spaces
				}
			}
		}
	}
	if err != nil {
		// Hand the claim back so the next start retries rather than leaving members of an
		// existing space unable to attach anything.
		if relErr := s.repo.ReleaseDataMigration(ctx, dataMigrationAttachFiles); relErr != nil {
			logger.Err("spaces", relErr, map[string]any{"data_migration": dataMigrationAttachFiles})
		}
		return err
	}
	for spaceID := range spaceIDs {
		s.invalidateSnapshot(ctx, spaceID)
	}
	logger.Info("spaces", "attach-files permission backfilled on %d role(s) and %d override(s) across %d space(s)", roles, overrides, len(spaceIDs))
	return nil
}

// backfillAttachOverrides mirrors Send Messages into Attach Files on every override of one
// room (allow follows allow, deny follows deny). Returns how many it changed.
func (s *Service) backfillAttachOverrides(ctx context.Context, spaceID, roomID int64, now time.Time) (int, error) {
	const send, attach = permissions.PermSendMessages, permissions.PermAttachFiles
	mirror := func(allow, deny *int64) bool {
		changed := false
		if *allow&send != 0 && *allow&attach == 0 {
			*allow |= attach
			changed = true
		}
		if *deny&send != 0 && *deny&attach == 0 {
			*deny |= attach
			changed = true
		}
		return changed
	}
	n := 0
	roleOvs, err := s.repo.ListRoomRoleOverrides(ctx, spaceID, roomID)
	if err != nil {
		return 0, err
	}
	for i := range roleOvs {
		o := roleOvs[i]
		if mirror(&o.Allow, &o.Deny) {
			o.UpdatedAt = now
			if err := s.repo.UpsertRoomRoleOverride(ctx, &o); err != nil {
				return n, err
			}
			n++
		}
	}
	userOvs, err := s.repo.ListRoomUserOverrides(ctx, spaceID, roomID)
	if err != nil {
		return n, err
	}
	for i := range userOvs {
		o := userOvs[i]
		if mirror(&o.Allow, &o.Deny) {
			o.UpdatedAt = now
			if err := s.repo.UpsertRoomUserOverride(ctx, &o); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
