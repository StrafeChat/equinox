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
