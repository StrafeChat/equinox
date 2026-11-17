package devices

import (
	"context"

	"github.com/StrafeChat/equinox/internal/logger"
)

// MaxBackupVersionsPurged bounds the version listing PurgeUser walks. An account keeps the
// current version and the previous one, so a handful is already generous.
const MaxBackupVersionsPurged = 100

// PurgeUser removes every piece of end-to-end encryption material a deleted account left on
// the server: each device's identity keys, its one-time keys and the to-device messages
// still waiting for it (DeleteDeviceKeys does all three in one batch), the legacy
// PIN-wrapped backup, and every key-backup version with its room keys. No DEVICE_REVOKED:
// the account's sockets are already closed. It returns how many devices there were.
// Best-effort per item; the first error is returned at the end so a rerun finishes the job.
func (s *Service) PurgeUser(ctx context.Context, userID int64) (int, error) {
	var firstErr error
	note := func(stage string, err error) {
		if err == nil {
			return
		}
		logger.Err("devices", err, map[string]any{"stage": stage, "user_id": userID})
		if firstErr == nil {
			firstErr = err
		}
	}
	devices, err := s.repo.ListDeviceKeys(ctx, userID)
	note("purge_list_devices", err)
	for _, d := range devices {
		note("purge_device", s.repo.DeleteDeviceKeys(ctx, userID, d.DeviceID))
	}
	note("purge_legacy_backup", s.repo.DeleteKeyBackup(ctx, userID))
	versions, err := s.repo.ListBackupVersions(ctx, userID, MaxBackupVersionsPurged)
	note("purge_list_backups", err)
	for _, v := range versions {
		note("purge_backup_version", s.deleteBackupVersionData(ctx, userID, v.Version))
	}
	return len(devices), firstErr
}

// CountDevices is the deletion preview's device count.
func (s *Service) CountDevices(ctx context.Context, userID int64) (int, error) {
	devices, err := s.repo.ListDeviceKeys(ctx, userID)
	return len(devices), err
}
