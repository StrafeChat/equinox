package devices

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/stargate"
)

var (
	ErrDeviceNotFound = errors.New("device not found")
	ErrNotOwnDevice   = errors.New("device does not belong to this user")
	ErrInvalidKeys    = errors.New("device keys do not match this device")
	ErrInvalidInput   = errors.New("invalid request")
)

// Size caps. Everything here is stored verbatim and served back to every peer that asks,
// so an unbounded body would let one account fill the keyspace or make key queries for
// its user huge.
const (
	maxDeviceKeysBytes     = 16 * 1024
	maxOneTimeKeysPerCall  = 500
	maxOneTimeKeyBytes     = 4 * 1024
	maxKeyIDLen            = 128
	maxToDeviceRecipients  = 2000
	maxToDeviceContentSize = 64 * 1024
	maxEventTypeLen        = 100
)

// validateDeviceKeysClaim checks that a self-signed device-keys claim is about the device
// (and user) it is being uploaded for. The engine on the receiving side rejects a mismatch
// anyway; catching it here turns a confusing "nobody can encrypt to me" into a 400.
func (s *Service) validateDeviceKeysClaim(userID, deviceID int64, raw json.RawMessage) error {
	if len(raw) > maxDeviceKeysBytes {
		return ErrInvalidKeys
	}
	var claim struct {
		UserID   string `json:"user_id"`
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return ErrInvalidKeys
	}
	if claim.DeviceID != "" && claim.DeviceID != id.Format(deviceID) {
		return ErrInvalidKeys
	}
	if claim.UserID != "" {
		idPart, server := splitMatrixUserID(claim.UserID)
		if idPart != id.Format(userID) || s.isRemoteServer(server) {
			return ErrInvalidKeys
		}
	}
	return nil
}

func validateOneTimeKeys(keys map[string]json.RawMessage) error {
	for keyID, raw := range keys {
		if len(keyID) == 0 || len(keyID) > maxKeyIDLen || len(raw) == 0 || len(raw) > maxOneTimeKeyBytes {
			return ErrInvalidInput
		}
	}
	return nil
}

// validateToDevice bounds a to-device fan-out before anything is stored.
func validateToDevice(eventType string, messages map[string]map[string]json.RawMessage) error {
	if eventType == "" || len(eventType) > maxEventTypeLen {
		return ErrInvalidInput
	}
	total := 0
	for _, byDevice := range messages {
		for _, content := range byDevice {
			total++
			if total > maxToDeviceRecipients || len(content) > maxToDeviceContentSize {
				return ErrInvalidInput
			}
		}
	}
	return nil
}

type Service struct {
	repo   Repository
	redis  *redis.Client
	cfg    *config.Config
	router KeyRouter

	// reach gates to-device delivery on the sender sharing a room with (and not blocking
	// or being blocked by) each recipient; see SetRelationshipChecker.
	reach RelationshipChecker
}

// SetRelationshipChecker installs the shared-room gate for to-device delivery.
func (s *Service) SetRelationshipChecker(r RelationshipChecker) { s.reach = r }

func NewService(repo Repository, redisClient *redis.Client, cfg *config.Config) *Service {
	return &Service{repo: repo, redis: redisClient, cfg: cfg}
}

func (s *Service) stargateRegion() string {
	if s.cfg == nil || s.cfg.Stargate.Region == "" {
		return "default"
	}
	return s.cfg.Stargate.Region
}

// CreateDevice registers a new device identity for a user and returns its server-assigned
// ID. Device IDs are always server-issued (via the same snowflake generator used
// everywhere else), never client-chosen - the old client-chosen `device_id: 1` default
// meant two genuinely independent devices for the same user could silently collide and
// overwrite each other's keys.
func (s *Service) CreateDevice(ctx context.Context, userID int64) (int64, error) {
	deviceID := id.Next()
	if err := s.repo.UpsertDeviceKeys(ctx, userID, deviceID, ""); err != nil {
		return 0, err
	}
	return deviceID, nil
}

// RevokeDevice deletes a device's keys, its one-time-key pool, and any undelivered
// to-device messages for it, then best-effort notifies the user's other sessions so they
// can drop cached Olm/Megolm sessions with the revoked device.
func (s *Service) RevokeDevice(ctx context.Context, userID, deviceID int64) error {
	existing, err := s.repo.GetDeviceKeys(ctx, userID, deviceID)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrDeviceNotFound
	}
	if err := s.repo.DeleteDeviceKeys(ctx, userID, deviceID); err != nil {
		return err
	}
	if s.redis != nil && s.cfg != nil {
		stargate.PublishToUser(ctx, s.redis, userID, "DEVICE_REVOKED", map[string]interface{}{
			"user_id":   id.Format(userID),
			"device_id": id.Format(deviceID),
		}, s.stargateRegion())
	}
	return nil
}

// ListDevices returns a user's own devices (used for device-management UI - "your
// devices" with a revoke action). Unlike the old ListDevices this is scoped to the
// caller's own account; querying another user's devices goes through QueryKeys instead,
// matching how the Matrix-shaped endpoints split "manage my own devices" from "look up
// someone else's public keys."
func (s *Service) ListDevices(ctx context.Context, userID int64) ([]DeviceKeys, error) {
	return s.repo.ListDeviceKeys(ctx, userID)
}

// UploadKeys stores/replenishes a device's key material. deviceID must already exist
// (via CreateDevice) and belong to userID.
func (s *Service) UploadKeys(ctx context.Context, userID, deviceID int64, in *UploadKeysInput) (*UploadKeysOutput, error) {
	existing, err := s.repo.GetDeviceKeys(ctx, userID, deviceID)
	if err != nil {
		return nil, err
	}
	// A full device-keys claim may (re)create the device row: that is how a client whose
	// device the server lost (a restored backup, a dropped table) resurrects its identity
	// without abandoning the Olm sessions it already holds. One-time keys alone can't.
	if existing == nil && len(in.DeviceKeys) == 0 {
		return nil, ErrDeviceNotFound
	}
	if len(in.OneTimeKeys)+len(in.FallbackKeys) > maxOneTimeKeysPerCall {
		return nil, ErrInvalidInput
	}
	if err := validateOneTimeKeys(in.OneTimeKeys); err != nil {
		return nil, err
	}
	if err := validateOneTimeKeys(in.FallbackKeys); err != nil {
		return nil, err
	}

	if len(in.DeviceKeys) > 0 {
		if err := s.validateDeviceKeysClaim(userID, deviceID, in.DeviceKeys); err != nil {
			return nil, err
		}
		if err := s.repo.UpsertDeviceKeys(ctx, userID, deviceID, string(in.DeviceKeys)); err != nil {
			return nil, err
		}
	}

	var newKeys []OneTimeKey
	for keyID, raw := range in.OneTimeKeys {
		newKeys = append(newKeys, OneTimeKey{KeyID: keyID, KeyJSON: string(raw), IsFallback: false})
	}
	for keyID, raw := range in.FallbackKeys {
		newKeys = append(newKeys, OneTimeKey{KeyID: keyID, KeyJSON: string(raw), IsFallback: true})
	}
	if len(newKeys) > 0 {
		if err := s.repo.AddOneTimeKeys(ctx, userID, deviceID, newKeys); err != nil {
			return nil, err
		}
	}

	regular, hasFallback, err := s.repo.CountOneTimeKeys(ctx, userID, deviceID)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	if regular > 0 || hasFallback {
		// Single supported algorithm today; counts are keyed by algorithm per the
		// Matrix wire shape so a future second algorithm doesn't need a format change.
		counts["signed_curve25519"] = regular
	}
	return &UploadKeysOutput{OneTimeKeyCounts: counts}, nil
}

// QueryKeys returns the device-keys claims for the requested users/devices. Empty device
// list for a user means "all of that user's devices." Users on other instances are
// queried there (see KeyRouter) and merged into the same response.
func (s *Service) QueryKeys(ctx context.Context, requesterID int64, req map[string][]string) (*QueryKeysOutput, error) {
	out := &QueryKeysOutput{DeviceKeys: map[string]map[string]json.RawMessage{}}
	remote := map[string]map[string][]string{}
	for userIDStr, deviceIDStrs := range req {
		idPart, server := splitMatrixUserID(userIDStr)
		if s.isRemoteServer(server) {
			if remote[server] == nil {
				remote[server] = map[string][]string{}
			}
			remote[server][userIDStr] = deviceIDStrs
			continue
		}
		userID, err := id.Parse(idPart)
		if err != nil {
			continue
		}
		devices, err := s.repo.ListDeviceKeys(ctx, userID)
		if err != nil {
			return nil, err
		}
		wanted := make(map[int64]bool, len(deviceIDStrs))
		for _, ds := range deviceIDStrs {
			if did, err := id.Parse(ds); err == nil {
				wanted[did] = true
			}
		}
		byDevice := map[string]json.RawMessage{}
		for _, d := range devices {
			if d.KeysJSON == "" {
				continue // device created but never uploaded keys yet
			}
			if len(wanted) > 0 && !wanted[d.DeviceID] {
				continue
			}
			byDevice[strconv.FormatInt(d.DeviceID, 10)] = d.Keys()
		}
		if len(byDevice) > 0 {
			out.DeviceKeys[userIDStr] = byDevice
		}
	}
	for domain, sub := range remote {
		res, err := s.router.QueryRemote(ctx, domain, requesterID, sub)
		if err != nil {
			// A peer being down must not break key discovery for everyone else in the room.
			logger.Err("devices", err, map[string]any{"federation_peer": domain, "op": "keys/query"})
			continue
		}
		for k, v := range res.DeviceKeys {
			if _, asked := sub[k]; !asked {
				continue // a peer answers for the users it was asked about, nobody else
			}
			out.DeviceKeys[k] = v
		}
	}
	return out, nil
}

// ClaimKeys atomically consumes one one-time (or fallback) key per requested device,
// forwarding to other instances for their users.
func (s *Service) ClaimKeys(ctx context.Context, requesterID int64, req map[string]map[string]string) (*ClaimKeysOutput, error) {
	out := &ClaimKeysOutput{OneTimeKeys: map[string]map[string]json.RawMessage{}}
	remote := map[string]map[string]map[string]string{}
	for userIDStr, byDevice := range req {
		idPart, server := splitMatrixUserID(userIDStr)
		if s.isRemoteServer(server) {
			if remote[server] == nil {
				remote[server] = map[string]map[string]string{}
			}
			remote[server][userIDStr] = byDevice
			continue
		}
		userID, err := id.Parse(idPart)
		if err != nil {
			continue
		}
		claimed := map[string]json.RawMessage{}
		for deviceIDStr := range byDevice {
			deviceID, err := id.Parse(deviceIDStr)
			if err != nil {
				continue
			}
			key, err := s.repo.TakeOneTimeKey(ctx, userID, deviceID)
			if err != nil {
				return nil, err
			}
			if key == nil {
				continue
			}
			claimed[deviceIDStr] = json.RawMessage(`{"` + key.KeyID + `":` + key.KeyJSON + `}`)
		}
		if len(claimed) > 0 {
			out.OneTimeKeys[userIDStr] = claimed
		}
	}
	for domain, sub := range remote {
		res, err := s.router.ClaimRemote(ctx, domain, requesterID, sub)
		if err != nil {
			logger.Err("devices", err, map[string]any{"federation_peer": domain, "op": "keys/claim"})
			continue
		}
		for k, v := range res.OneTimeKeys {
			if _, asked := sub[k]; !asked {
				continue
			}
			out.OneTimeKeys[k] = v
		}
	}
	return out, nil
}

// SendToDevice fans a set of to-device messages out to storage (for offline delivery)
// and pushes them live over the gateway to any currently-connected session. "*" as a
// device ID means every device the recipient currently has registered. Recipients on
// other instances get theirs relayed there.
func (s *Service) SendToDevice(ctx context.Context, senderUserID, senderDeviceID int64, eventType string, messages map[string]map[string]json.RawMessage) error {
	if err := validateToDevice(eventType, messages); err != nil {
		return err
	}
	// The sender device is client-supplied; recipients key their Olm sessions by it, so
	// it has to be one of the sender's own registered devices.
	if sender, err := s.repo.GetDeviceKeys(ctx, senderUserID, senderDeviceID); err != nil {
		return err
	} else if sender == nil {
		return ErrDeviceNotFound
	}
	local := map[string]map[string]json.RawMessage{}
	remote := map[string]map[string]map[string]json.RawMessage{}
	for userIDStr, byDevice := range messages {
		_, server := splitMatrixUserID(userIDStr)
		if s.isRemoteServer(server) {
			if remote[server] == nil {
				remote[server] = map[string]map[string]json.RawMessage{}
			}
			remote[server][userIDStr] = byDevice
			continue
		}
		local[userIDStr] = byDevice
	}
	if err := s.deliverLocal(ctx, senderUserID, senderDeviceID, s.localFID(senderUserID), eventType, local); err != nil {
		return err
	}
	for domain, sub := range remote {
		if err := s.router.SendToDeviceRemote(ctx, domain, senderUserID, senderDeviceID, eventType, sub); err != nil {
			logger.Err("devices", err, map[string]any{"federation_peer": domain, "op": "to_device"})
		}
	}
	return nil
}

// DeliverFromRemote stores/pushes to-device messages relayed by another instance. Only
// recipients on this instance are considered; senderUserID is the sender's local shadow
// row and senderFID their federated id (what the recipient's crypto engine keys the Olm
// session by).
func (s *Service) DeliverFromRemote(ctx context.Context, senderUserID, senderDeviceID int64, senderFID, eventType string, messages map[string]map[string]json.RawMessage) error {
	if err := validateToDevice(eventType, messages); err != nil {
		return err
	}
	local := map[string]map[string]json.RawMessage{}
	for userIDStr, byDevice := range messages {
		if _, server := splitMatrixUserID(userIDStr); s.isRemoteServer(server) {
			continue
		}
		local[userIDStr] = byDevice
	}
	return s.deliverLocal(ctx, senderUserID, senderDeviceID, senderFID, eventType, local)
}

func (s *Service) deliverLocal(ctx context.Context, senderUserID, senderDeviceID int64, senderFID, eventType string, messages map[string]map[string]json.RawMessage) error {
	var toStore []ToDeviceMessage
	type livePush struct {
		userID, deviceID int64
		msg              ToDeviceMessage
	}
	var toPush []livePush

	for userIDStr, byDevice := range messages {
		idPart, _ := splitMatrixUserID(userIDStr)
		userID, err := id.Parse(idPart)
		if err != nil {
			continue
		}
		// To-device traffic reaches only people the sender may exchange keys with (a
		// shared room; no block), the gate the key endpoints apply - otherwise any account
		// could push arbitrary events to every device of anyone.
		if userID != senderUserID && s.reach != nil {
			ok, rerr := s.reach.CanExchangeKeys(ctx, senderUserID, userID)
			if rerr != nil {
				return rerr
			}
			if !ok {
				continue
			}
		}
		deviceIDStrs := make([]string, 0, len(byDevice))
		for k := range byDevice {
			deviceIDStrs = append(deviceIDStrs, k)
		}
		if len(deviceIDStrs) == 1 && deviceIDStrs[0] == "*" {
			devices, err := s.repo.ListDeviceKeys(ctx, userID)
			if err != nil {
				return err
			}
			content := byDevice["*"]
			for _, d := range devices {
				m := ToDeviceMessage{
					RecipientUserID: userID, RecipientDeviceID: d.DeviceID, MessageID: id.Next(),
					SenderUserID: senderUserID, SenderDeviceID: senderDeviceID, SenderFID: senderFID,
					EventType: eventType, ContentJSON: string(content),
				}
				toStore = append(toStore, m)
				toPush = append(toPush, livePush{userID, d.DeviceID, m})
			}
			continue
		}
		for deviceIDStr, content := range byDevice {
			deviceID, err := id.Parse(deviceIDStr)
			if err != nil {
				continue
			}
			m := ToDeviceMessage{
				RecipientUserID: userID, RecipientDeviceID: deviceID, MessageID: id.Next(),
				SenderUserID: senderUserID, SenderDeviceID: senderDeviceID, SenderFID: senderFID,
				EventType: eventType, ContentJSON: string(content),
			}
			toStore = append(toStore, m)
			toPush = append(toPush, livePush{userID, deviceID, m})
		}
	}

	if len(toStore) == 0 {
		return nil
	}
	if err := s.repo.AddToDeviceMessages(ctx, toStore); err != nil {
		return err
	}
	if s.redis != nil && s.cfg != nil {
		region := s.stargateRegion()
		for _, p := range toPush {
			payload := map[string]interface{}{
				"id":               id.Format(p.msg.MessageID),
				"type":             p.msg.EventType,
				"sender_user_id":   id.Format(p.msg.SenderUserID),
				"sender_device_id": id.Format(p.msg.SenderDeviceID),
				"content":          p.msg.Content(),
			}
			if p.msg.SenderFID != "" {
				payload["sender_fid"] = p.msg.SenderFID
			}
			stargate.PublishToUser(ctx, s.redis, p.userID, "TO_DEVICE", payload, region)
		}
	}
	return nil
}

// PollToDevice lists undelivered to-device messages (fallback for a device that was
// offline when SendToDevice's live push went out).
func (s *Service) PollToDevice(ctx context.Context, userID, deviceID int64) ([]ToDeviceMessage, error) {
	return s.repo.ListToDeviceMessages(ctx, userID, deviceID, 100)
}

func (s *Service) AckToDevice(ctx context.Context, userID, deviceID int64, messageIDs []int64) error {
	return s.repo.AckToDeviceMessages(ctx, userID, deviceID, messageIDs)
}

func (s *Service) SetKeyBackup(ctx context.Context, userID int64, encryptedBackup, salt, roomKeys string) error {
	return s.repo.UpsertKeyBackup(ctx, userID, encryptedBackup, salt, roomKeys)
}

func (s *Service) GetKeyBackup(ctx context.Context, userID int64) (*DeviceKeyBackup, error) {
	return s.repo.GetKeyBackup(ctx, userID)
}

// DeleteKeyBackup removes the legacy PIN-wrapped backup. The client calls this once it has
// copied the same room keys into the asymmetric backup - see migration 027.
func (s *Service) DeleteKeyBackup(ctx context.Context, userID int64) error {
	return s.repo.DeleteKeyBackup(ctx, userID)
}

// --- Asymmetric key backup

// ErrBackupVersionNotFound means the client is uploading to a version that no longer exists
// - usually because another device created a new one. Clients re-read the current version
// and retry rather than treating it as a hard failure.
var ErrBackupVersionNotFound = errors.New("backup version not found")

// ErrBackupFull means this account's backup has hit its session ceiling.
var ErrBackupFull = errors.New("backup is full")

// Only the one scheme the crypto engine implements. Rejecting anything else keeps the
// endpoint from becoming general-purpose storage for arbitrary blobs.
const backupAlgorithm = "m.megolm_backup.v1.curve25519-aes-sha2"

const (
	maxBackupAuthDataBytes    = 4 * 1024
	maxWrappedPrivateKeyBytes = 4 * 1024
	maxBackupSaltBytes        = 256
	maxSessionsPerBackupCall  = 1000
	maxSessionDataBytes       = 8 * 1024
	maxSessionIDLen           = 256
	maxBackupRoomIDLen        = 256
	// One Megolm session per room per sending device per rotation. A heavy account across a
	// year of busy rooms lands in the low thousands, so this leaves a wide margin while
	// still bounding what one account can store.
	maxSessionsPerAccount = 50_000
)

// validateBackupAuthData checks the one thing the server needs auth_data to contain.
//
// It is otherwise opaque scheme parameters, but it carries the public key every device
// encrypts room keys to. Without this check a malformed version is stored happily, every
// client then quietly declines to use it (there is nothing to encrypt to), and the account
// ends up with a backup that can never hold a single key - with no error anywhere saying so.
func validateBackupAuthData(authData string) error {
	var auth struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal([]byte(authData), &auth); err != nil || auth.PublicKey == "" {
		return ErrInvalidInput
	}
	return nil
}

// supersededBackupRetention is how long the version a new one replaced stays readable.
//
// A superseded version's rows are room keys wrapped under a recovery code the user has
// stopped using, so for this long an old code still opens history. The alternative was
// worse: creating a version used to delete the previous one in the same request, which made
// "create a version" - something any session token, or the operator, could do - also
// "destroy the user's only copy of their history". A week is long enough for the user to
// notice (their other devices are told at once, see KEY_BACKUP_UPDATE) and restore from the
// version they actually made.
const supersededBackupRetention = 7 * 24 * time.Hour

// backupVersionListLimit bounds the read behind the retention rule. The rule keeps at most
// two rows, so anything past this is leftovers for the next pass to sweep.
const backupVersionListLimit = 4

// retainedBackupVersions splits an account's versions, newest first, into the ones the
// retention rule keeps and the ones it drops: the current version, plus the one it
// superseded while the current one is younger than supersededBackupRetention. The clock is
// the current version's age rather than the previous one's, because "superseded for a
// week" is the promise - a version that served for a year is not stale the day it is
// replaced.
func retainedBackupVersions(versions []KeyBackupVersion, now time.Time) (keep, drop []KeyBackupVersion) {
	if len(versions) == 0 {
		return nil, nil
	}
	n := 1
	if len(versions) > 1 && now.Sub(versions[0].CreatedAt) < supersededBackupRetention {
		n = 2
	}
	return versions[:n], versions[n:]
}

// backupVersions reads the account's current version and the superseded one still
// retained (either may be nil). With sweep set it also deletes whatever the retention rule
// no longer keeps - lazily, on the writes and the recovery read, never on the per-sync
// GetBackupVersion poll, which only needs to read. A failed sweep is logged, not returned:
// the request in hand is correct either way, and the next pass gets another go.
func (s *Service) backupVersions(ctx context.Context, userID int64, sweep bool) (current, previous *KeyBackupVersion, err error) {
	versions, err := s.repo.ListBackupVersions(ctx, userID, backupVersionListLimit)
	if err != nil {
		return nil, nil, err
	}
	keep, drop := retainedBackupVersions(versions, time.Now().UTC())
	if sweep {
		for _, v := range drop {
			if err := s.deleteBackupVersionData(ctx, userID, v.Version); err != nil {
				logger.Err("devices", err, map[string]any{"user_id": userID, "version": v.Version, "note": "superseded backup version not fully deleted"})
			}
		}
	}
	if len(keep) > 0 {
		current = &keep[0]
	}
	if len(keep) > 1 {
		previous = &keep[1]
	}
	return current, previous, nil
}

// BackupVersions is the read-only view every sync polls: the current version and, while
// retained, the one it replaced.
func (s *Service) BackupVersions(ctx context.Context, userID int64) (current, previous *KeyBackupVersion, err error) {
	return s.backupVersions(ctx, userID, false)
}

// RecoverableBackupVersion returns the version a restore may read recovery material for:
// the current one when version is 0 (nil if the account has none), otherwise the named
// version provided it is the current or the retained previous one. The retention sweep runs
// first, so a version past its week is gone rather than quietly still restorable.
func (s *Service) RecoverableBackupVersion(ctx context.Context, userID, version int64) (*KeyBackupVersion, error) {
	current, previous, err := s.backupVersions(ctx, userID, true)
	if err != nil {
		return nil, err
	}
	switch {
	case version == 0:
		return current, nil
	case current != nil && current.Version == version:
		return current, nil
	case previous != nil && previous.Version == version:
		return previous, nil
	}
	return nil, ErrBackupVersionNotFound
}

// CreateBackupVersion starts a new backup version. The one it supersedes is kept for
// supersededBackupRetention; anything older goes now.
//
// The caller has already re-checked the password (see Handler.checkPassword): this is one
// of the two requests that decide where every device sends the account's room keys. Devices
// pinned to the old version are told over the gateway and stop uploading; the retained copy
// is what the user restores from if the new version was not theirs.
func (s *Service) CreateBackupVersion(ctx context.Context, userID int64, algorithm, authData, wrappedPrivateKey, salt string) (*KeyBackupVersion, error) {
	if algorithm != backupAlgorithm {
		return nil, ErrInvalidInput
	}
	if authData == "" || wrappedPrivateKey == "" || salt == "" {
		return nil, ErrInvalidInput
	}
	if len(authData) > maxBackupAuthDataBytes || len(wrappedPrivateKey) > maxWrappedPrivateKeyBytes || len(salt) > maxBackupSaltBytes {
		return nil, ErrInvalidInput
	}
	if err := validateBackupAuthData(authData); err != nil {
		return nil, err
	}

	v := &KeyBackupVersion{
		UserID:            userID,
		Version:           id.Next(),
		Algorithm:         algorithm,
		AuthData:          authData,
		WrappedPrivateKey: wrappedPrivateKey,
		Salt:              salt,
		Etag:              id.Format(id.Next()),
		CreatedAt:         time.Now().UTC(),
	}
	if err := s.repo.CreateBackupVersion(ctx, v); err != nil {
		return nil, err
	}
	// The row just written is the newest, so the sweep keeps it and the one it replaced.
	if _, _, err := s.backupVersions(ctx, userID, true); err != nil {
		logger.Err("devices", err, map[string]any{"user_id": userID, "note": "backup retention sweep skipped"})
	}
	s.publishBackupUpdate(ctx, userID, "created", v.Version)
	return v, nil
}

// DeleteBackupVersion removes a version and every key in it, at once - what "I lost my
// recovery code, start over" does. Deleting the current version makes the retained previous
// one current again, which is how a user undoes a replacement that was not theirs.
func (s *Service) DeleteBackupVersion(ctx context.Context, userID, version int64) error {
	existing, err := s.repo.GetBackupVersion(ctx, userID, version)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrBackupVersionNotFound
	}
	if err := s.deleteBackupVersionData(ctx, userID, version); err != nil {
		return err
	}
	s.publishBackupUpdate(ctx, userID, "deleted", version)
	return nil
}

func (s *Service) deleteBackupVersionData(ctx context.Context, userID, version int64) error {
	if err := s.repo.DeleteBackupSessions(ctx, userID, version); err != nil {
		return err
	}
	return s.repo.DeleteBackupVersion(ctx, userID, version)
}

// publishBackupUpdate tells the account's other devices the backup version changed, so one
// pinned to a version that is no longer current stops uploading and shows its
// needs-confirming state now, rather than at whatever upload next gets a 404.
func (s *Service) publishBackupUpdate(ctx context.Context, userID int64, action string, version int64) {
	if s.redis == nil || s.cfg == nil {
		return
	}
	stargate.PublishToUser(ctx, s.redis, userID, "KEY_BACKUP_UPDATE", map[string]interface{}{
		"action":  action,
		"version": id.Format(version),
	}, s.stargateRegion())
}

// PutBackupKeys stores a batch of encrypted room keys, keeping the better copy where a
// session is already present. Returns the backup's total session count and a new etag,
// which is what the client's crypto engine expects back from the upload.
func (s *Service) PutBackupKeys(ctx context.Context, userID, version int64, in *PutBackupKeysInput) (int, string, error) {
	existing, _, err := s.backupVersions(ctx, userID, true)
	if err != nil {
		return 0, "", err
	}
	// Only the current version takes uploads. The retained previous one exists to be read
	// back, and a device still pointed at it has to learn the version moved - this 404 is
	// how one that missed KEY_BACKUP_UPDATE finds out.
	if existing == nil || existing.Version != version {
		return 0, "", ErrBackupVersionNotFound
	}

	total := 0
	for _, room := range in.Rooms {
		total += len(room.Sessions)
	}
	if total == 0 {
		count, err := s.repo.CountBackupSessions(ctx, userID, version)
		return count, existing.Etag, err
	}
	if total > maxSessionsPerBackupCall {
		return 0, "", ErrInvalidInput
	}

	count, err := s.repo.CountBackupSessions(ctx, userID, version)
	if err != nil {
		return 0, "", err
	}
	if count+total > maxSessionsPerAccount {
		return 0, "", ErrBackupFull
	}

	now := time.Now().UTC()
	var toWrite []KeyBackupSession
	added := 0 // rows that did not exist before, so the new total is count+added
	for roomID, room := range in.Rooms {
		if roomID == "" || len(roomID) > maxBackupRoomIDLen || len(room.Sessions) == 0 {
			return 0, "", ErrInvalidInput
		}
		incoming := make(map[string]KeyBackupSession, len(room.Sessions))
		ids := make([]string, 0, len(room.Sessions))
		for sessionID, data := range room.Sessions {
			if sessionID == "" || len(sessionID) > maxSessionIDLen {
				return 0, "", ErrInvalidInput
			}
			if len(data.SessionData) == 0 || len(data.SessionData) > maxSessionDataBytes {
				return 0, "", ErrInvalidInput
			}
			if !json.Valid(data.SessionData) {
				return 0, "", ErrInvalidInput
			}
			if data.FirstMessageIndex < 0 || data.ForwardedCount < 0 {
				return 0, "", ErrInvalidInput
			}
			incoming[sessionID] = KeyBackupSession{
				UserID:            userID,
				Version:           version,
				RoomID:            roomID,
				SessionID:         sessionID,
				SessionData:       string(data.SessionData),
				FirstMessageIndex: data.FirstMessageIndex,
				ForwardedCount:    data.ForwardedCount,
				IsVerified:        data.IsVerified,
				CreatedAt:         now,
			}
			ids = append(ids, sessionID)
		}
		stored, err := s.repo.GetBackupSessions(ctx, userID, version, roomID, ids)
		if err != nil {
			return 0, "", err
		}
		for sessionID, candidate := range incoming {
			if prev, ok := stored[sessionID]; ok {
				if !candidate.BetterThan(&prev) {
					continue
				}
			} else {
				added++
			}
			toWrite = append(toWrite, candidate)
		}
	}

	if err := s.repo.PutBackupSessions(ctx, toWrite); err != nil {
		return 0, "", err
	}
	// count + added rather than a second COUNT(*): the only rows that change the total are the
	// ones that had no stored copy, and a partition-wide count is a real scan - doing it twice
	// per upload batch was pure waste.
	return count + added, id.Format(id.Next()), nil
}

// GetBackupKeys returns one page of a version's backed-up sessions, in the /room_keys/keys
// response shape, plus a cursor for the next page (empty when the page is the last). Each
// session_data value is ciphertext only the recovery code can open.
//
// Paged rather than all-at-once: an account may hold tens of thousands of sessions, and
// assembling those into a single response would mean a body of tens of megabytes built
// entirely in memory - on the one request path a user cannot afford to have fail.
func (s *Service) GetBackupKeys(ctx context.Context, userID, version int64, after string) (rooms map[string]any, next string, err error) {
	existing, err := s.repo.GetBackupVersion(ctx, userID, version)
	if err != nil {
		return nil, "", err
	}
	if existing == nil {
		return nil, "", ErrBackupVersionNotFound
	}
	afterRoomID, afterSessionID, err := decodeBackupCursor(after)
	if err != nil {
		return nil, "", ErrInvalidInput
	}
	sessions, err := s.repo.ListBackupSessions(ctx, userID, version, afterRoomID, afterSessionID)
	if err != nil {
		return nil, "", err
	}
	rooms = make(map[string]any, 8)
	perRoom := make(map[string]map[string]any, 8)
	for _, sess := range sessions {
		room, ok := perRoom[sess.RoomID]
		if !ok {
			room = make(map[string]any, 4)
			perRoom[sess.RoomID] = room
			rooms[sess.RoomID] = map[string]any{"sessions": room}
		}
		room[sess.SessionID] = map[string]any{
			"first_message_index": sess.FirstMessageIndex,
			"forwarded_count":     sess.ForwardedCount,
			"is_verified":         sess.IsVerified,
			"session_data":        json.RawMessage(sess.SessionData),
		}
	}
	// A full page may or may not be the last one; only a short page proves the end. Handing
	// back a cursor costs one extra (empty) request rather than risking a truncated restore.
	if len(sessions) == BackupKeysPageSize {
		last := sessions[len(sessions)-1]
		next = encodeBackupCursor(last.RoomID, last.SessionID)
	}
	return rooms, next, nil
}

// The cursor is the clustering position the previous page ended on. Opaque to the client,
// but deliberately a position rather than a driver page state, which can expire between
// requests and would fail a restore halfway through.
func encodeBackupCursor(roomID, sessionID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(roomID + "\x00" + sessionID))
}

func decodeBackupCursor(cursor string) (roomID, sessionID string, err error) {
	if cursor == "" {
		return "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", err
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 {
		return "", "", errors.New("malformed backup cursor")
	}
	return parts[0], parts[1], nil
}

func (s *Service) CountBackupSessions(ctx context.Context, userID, version int64) (int, error) {
	return s.repo.CountBackupSessions(ctx, userID, version)
}
