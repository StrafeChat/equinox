package instance

import (
	"context"

	"github.com/StrafeChat/equinox/internal/logger"
)

const dataMigrationExistingInstance = "029_instance_already_has_accounts"

// SealBootstrapOnExistingInstance closes the first-account window on an instance that
// already had accounts before instance invites existed.
//
// The window is what lets an operator create their own account on a brand-new invite-only
// instance: with nothing in instance_state, the first registration claims the instance and
// becomes its administrator. On an *upgrade* that is exactly wrong - instance_state is
// empty there too, so the next person to register would be handed the instance, and on an
// open instance that is anybody at all.
//
// So: if this keyspace already has a user, record the instance as claimed by nobody. The
// window is shut, no existing account is promoted on a guess about who the operator is,
// and INSTANCE_ADMINS is how they give themselves the bit - which is what it is for.
func (s *Service) SealBootstrapOnExistingInstance(ctx context.Context) error {
	bootstrapped, err := s.repo.IsBootstrapped(ctx)
	if err != nil || bootstrapped {
		return err
	}
	claimed, err := s.repo.ClaimDataMigration(ctx, dataMigrationExistingInstance)
	if err != nil || !claimed {
		return err
	}
	exists, err := s.repo.AnyUserExists(ctx)
	if err != nil {
		// Hand the claim back: leaving it in place would skip this check forever, and the
		// next start is a better place to decide than a guess made during an outage.
		if relErr := s.repo.ReleaseDataMigration(ctx, dataMigrationExistingInstance); relErr != nil {
			logger.Err("instance", relErr, map[string]any{"data_migration": dataMigrationExistingInstance})
		}
		return err
	}
	if !exists {
		// A genuinely empty instance: leave the window open, that is the whole point.
		return nil
	}
	if _, err := s.repo.ClaimBootstrap(ctx, 0); err != nil {
		return err
	}
	logger.Info("instance", "this instance already had accounts, so the first-account "+
		"administrator claim is closed; set INSTANCE_ADMINS to name an administrator")
	return nil
}
