package devices

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// RelationshipChecker reports whether two users can already reach each other (a PM/group
// PM exists, or they share a space) - gates key-exchange endpoints so any authenticated
// user can't enumerate and drain every other user's one-time-key pool. Defense in depth
// alongside rate limiting, not a replacement for it (see routes/devices.go).
type RelationshipChecker interface {
	CanExchangeKeys(ctx context.Context, userA, userB int64) (bool, error)
}

type Handler struct {
	svc *Service
	rel RelationshipChecker
	fid FIDResolver
}

func NewHandler(svc *Service, rel RelationshipChecker) *Handler {
	return &Handler{svc: svc, rel: rel}
}

// SetResolver wires federation so reachability checks understand @id:domain for remote
// users (they resolve to the shadow row the caller shares a room with).
func (h *Handler) SetResolver(r FIDResolver) {
	h.fid = r
}

// localIDFor maps a crypto-engine user id to the local user id used for relationship checks.
func (h *Handler) localIDFor(ctx context.Context, userIDStr string) (int64, bool) {
	if h.fid != nil {
		v, err := h.fid.ResolveLocalID(ctx, userIDStr)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	idPart, _ := splitMatrixUserID(userIDStr)
	v, err := id.Parse(idPart)
	if err != nil {
		return 0, false
	}
	return v, true
}

// CreateDevice registers a new device identity and returns its server-assigned ID. POST /devices
func (h *Handler) CreateDevice(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	deviceID, err := h.svc.CreateDevice(c.Context(), user.ID)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{"device_id": id.Format(deviceID)})
}

// ListDevices returns the caller's own devices. GET /devices
func (h *Handler) ListDevices(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	devices, err := h.svc.ListDevices(c.Context(), user.ID)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := make([]fiber.Map, 0, len(devices))
	for _, d := range devices {
		out = append(out, fiber.Map{
			"device_id":  id.Format(d.DeviceID),
			"has_keys":   d.KeysJSON != "",
			"created_at": d.CreatedAt,
			"updated_at": d.UpdatedAt,
		})
	}
	return c.JSON(out)
}

// RevokeDevice deletes a device's keys. DELETE /devices/:device_id
func (h *Handler) RevokeDevice(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	deviceID, err := id.Parse(c.Params("device_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid device_id"})
	}
	if err := h.svc.RevokeDevice(c.Context(), user.ID, deviceID); err != nil {
		if err == ErrDeviceNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "device not found"})
		}
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "device_id": deviceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// UploadKeys uploads/replenishes a device's key material. POST /devices/:device_id/keys/upload
func (h *Handler) UploadKeys(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	deviceID, err := id.Parse(c.Params("device_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid device_id"})
	}
	var in UploadKeysInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	out, err := h.svc.UploadKeys(c.Context(), user.ID, deviceID, &in)
	if err != nil {
		if err == ErrDeviceNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "device not found - call POST /devices first"})
		}
		if err == ErrInvalidKeys || err == ErrInvalidInput {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "device_id": deviceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(out)
}

// QueryKeys returns device-key claims for the requested users. POST /devices/keys/query
// Gated: every requested user must already share a PM or space with the caller.
func (h *Handler) QueryKeys(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in QueryKeysInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	filtered, err := h.filterReachable(c.Context(), user.ID, in.DeviceKeys)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out, err := h.svc.QueryKeys(c.Context(), filtered)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(out)
}

// ClaimKeys atomically consumes one-time keys for the requested devices. POST /devices/keys/claim
// Gated the same way as QueryKeys.
func (h *Handler) ClaimKeys(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in ClaimKeysInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	userIDs := make(map[string][]string, len(in.OneTimeKeys))
	for userIDStr := range in.OneTimeKeys {
		userIDs[userIDStr] = nil
	}
	filtered, err := h.filterReachable(c.Context(), user.ID, userIDs)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	scoped := make(map[string]map[string]string, len(filtered))
	for userIDStr := range filtered {
		scoped[userIDStr] = in.OneTimeKeys[userIDStr]
	}
	out, err := h.svc.ClaimKeys(c.Context(), scoped)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(out)
}

// filterReachable drops any requested user the caller doesn't already share a room with.
// Silent-drop (not an error) matches Matrix's own /keys/query "failures" convention -
// an unreachable user just doesn't appear in the result, same as one that doesn't exist.
// Defense in depth alongside rate limiting (routes/devices.go), not a replacement for it:
// this stops cheap platform-wide enumeration, not a determined attacker with a shared room.
func (h *Handler) filterReachable(ctx context.Context, callerID int64, req map[string][]string) (map[string][]string, error) {
	out := make(map[string][]string, len(req))
	for userIDStr, deviceIDs := range req {
		targetID, ok := h.localIDFor(ctx, userIDStr)
		if !ok {
			continue
		}
		if targetID == callerID || h.rel == nil {
			out[userIDStr] = deviceIDs
			continue
		}
		ok, err := h.rel.CanExchangeKeys(ctx, callerID, targetID)
		if err != nil {
			return nil, err
		}
		if ok {
			out[userIDStr] = deviceIDs
		}
	}
	return out, nil
}

// SendToDevice relays to-device messages (Olm session setup, Megolm room-key
// distribution, revocation notices). PUT /devices/send_to_device/:event_type/:txn_id
func (h *Handler) SendToDevice(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	eventType := c.Params("event_type")
	if eventType == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "event_type required"})
	}
	senderDeviceID, err := id.Parse(c.Query("sender_device_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "sender_device_id query param required"})
	}
	var in SendToDeviceInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	if err := h.svc.SendToDevice(c.Context(), user.ID, senderDeviceID, eventType, in.Messages); err != nil {
		if err == ErrDeviceNotFound {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "sender_device_id is not one of your devices"})
		}
		if err == ErrInvalidInput {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "too many recipients or content too large"})
		}
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// PollToDevice lists undelivered to-device messages. GET /devices/:device_id/to_device
func (h *Handler) PollToDevice(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	deviceID, err := id.Parse(c.Params("device_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid device_id"})
	}
	msgs, err := h.svc.PollToDevice(c.Context(), user.ID, deviceID)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "device_id": deviceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := make([]fiber.Map, 0, len(msgs))
	for _, m := range msgs {
		entry := fiber.Map{
			"id":               id.Format(m.MessageID),
			"type":             m.EventType,
			"sender_user_id":   id.Format(m.SenderUserID),
			"sender_device_id": id.Format(m.SenderDeviceID),
			"content":          m.Content(),
		}
		if m.SenderFID != "" {
			entry["sender_fid"] = m.SenderFID
		}
		out = append(out, entry)
	}
	return c.JSON(out)
}

// AckToDevice deletes delivered to-device messages. POST /devices/:device_id/to_device/ack
func (h *Handler) AckToDevice(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	deviceID, err := id.Parse(c.Params("device_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid device_id"})
	}
	var in struct {
		MessageIDs []string `json:"message_ids"`
	}
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	ids := make([]int64, 0, len(in.MessageIDs))
	for _, s := range in.MessageIDs {
		if v, err := id.Parse(s); err == nil {
			ids = append(ids, v)
		}
	}
	if err := h.svc.AckToDevice(c.Context(), user.ID, deviceID, ids); err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "device_id": deviceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// maxRoomKeyBackupBytes caps the encrypted Megolm session export. ~300 bytes per session,
// so this is on the order of a thousand sessions - generous for one account, and small
// enough that the endpoint is not useful as general storage.
const maxRoomKeyBackupBytes = 512 * 1024

// GetKeyBackup returns encrypted backup (exists: true/false). GET /devices/backup
func (h *Handler) GetKeyBackup(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	backup, err := h.svc.GetKeyBackup(c.Context(), user.ID)
	if err != nil {
		logger.Err("devices", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if backup == nil || backup.EncryptedBackup == "" {
		return c.JSON(fiber.Map{"exists": false})
	}
	return c.JSON(fiber.Map{
		"exists":           true,
		"encrypted_backup": backup.EncryptedBackup,
		"salt":             backup.Salt,
		"room_keys":        backup.RoomKeys,
	})
}

// SetKeyBackup stores encrypted backup. PUT /devices/backup
func (h *Handler) SetKeyBackup(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in struct {
		EncryptedBackup string `json:"encrypted_backup"`
		Salt            string `json:"salt"`
		RoomKeys        string `json:"room_keys"`
	}
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	if in.EncryptedBackup == "" || in.Salt == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "encrypted_backup and salt required"})
	}
	// The PIN-wrapped half is a few hundred bytes. room_keys is a Megolm session export -
	// roughly 300 bytes per session - so it needs real room while still being far too small
	// to use the endpoint as free storage. Keep the total under HTTP_BODY_LIMIT_KB (1MB by
	// default) or the request is rejected before it ever reaches this check.
	if len(in.EncryptedBackup) > 64*1024 || len(in.Salt) > 256 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "backup is too large"})
	}
	if len(in.RoomKeys) > maxRoomKeyBackupBytes {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "room key backup is too large"})
	}
	if err := h.svc.SetKeyBackup(c.Context(), user.ID, in.EncryptedBackup, in.Salt, in.RoomKeys); err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(fiber.Map{"ok": true})
}

// DeleteLegacyKeyBackup removes the PIN-wrapped backup. DELETE /devices/backup
//
// The client calls this once the asymmetric backup holds the same keys - see
// migration 027. Nothing else should: for an account that has not migrated yet, this is
// the only copy of its history's keys.
func (h *Handler) DeleteLegacyKeyBackup(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if err := h.svc.DeleteKeyBackup(c.Context(), user.ID); err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// --- Asymmetric key backup (m.megolm_backup.v1.curve25519-aes-sha2)

// backupVersionParam reads the ?version= query parameter shared by the key endpoints.
func backupVersionParam(c fiber.Ctx) (int64, bool) {
	v, err := id.Parse(c.Query("version"))
	if err != nil {
		return 0, false
	}
	return v, true
}

// CreateBackupVersion starts a new backup version. POST /devices/backup/version
//
// `auth_data` carries the Curve25519 public key every device encrypts room keys to, and is
// public by design. `wrapped_private_key` is the matching private key encrypted under the
// user's recovery code - ciphertext the server cannot open.
func (h *Handler) CreateBackupVersion(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in struct {
		Algorithm         string          `json:"algorithm"`
		AuthData          json.RawMessage `json:"auth_data"`
		WrappedPrivateKey string          `json:"wrapped_private_key"`
		Salt              string          `json:"salt"`
	}
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	v, err := h.svc.CreateBackupVersion(c.Context(), user.ID, in.Algorithm, string(in.AuthData), in.WrappedPrivateKey, in.Salt)
	if err != nil {
		if err == ErrInvalidInput {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid backup version"})
		}
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(fiber.Map{"version": id.Format(v.Version)})
}

// GetBackupVersion returns the current backup's public parameters. GET /devices/backup/version
//
// Deliberately *not* the wrapped private key: every device reads this to know what to encrypt
// room keys to, and it is on a hot path (each sync checks it), whereas the wrapped key is
// only ever needed by someone actually restoring. Serving them separately keeps recovery
// material off every page load, and keeps the hot path off the tight rate limit that material
// deserves. See GetBackupRecovery.
func (h *Handler) GetBackupVersion(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	v, err := h.svc.LatestBackupVersion(c.Context(), user.ID)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if v == nil {
		return c.JSON(fiber.Map{"exists": false})
	}
	count, err := h.svc.CountBackupSessions(c.Context(), user.ID, v.Version)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "version": v.Version})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(fiber.Map{
		"exists":     true,
		"version":    id.Format(v.Version),
		"algorithm":  v.Algorithm,
		"auth_data":  json.RawMessage(v.AuthData),
		"etag":       v.Etag,
		"count":      count,
		"created_at": v.CreatedAt,
	})
}

// GetBackupRecovery returns the backup's wrapped private key. GET /devices/backup/recovery
//
// Useless without the user's recovery code - it is AES-GCM ciphertext under a key derived
// from 128 bits of CSPRNG - but it is the only recovery material the server holds, so it is
// served on its own, from the tight rate-limit bucket, and only when a restore asks for it.
func (h *Handler) GetBackupRecovery(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	v, err := h.svc.LatestBackupVersion(c.Context(), user.ID)
	if err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if v == nil {
		return c.JSON(fiber.Map{"exists": false})
	}
	return c.JSON(fiber.Map{
		"exists":              true,
		"version":             id.Format(v.Version),
		"algorithm":           v.Algorithm,
		"auth_data":           json.RawMessage(v.AuthData),
		"wrapped_private_key": v.WrappedPrivateKey,
		"salt":                v.Salt,
	})
}

// DeleteBackupVersion discards a backup and every key in it.
// DELETE /devices/backup/version?version=N
func (h *Handler) DeleteBackupVersion(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	version, ok := backupVersionParam(c)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "version required"})
	}
	if err := h.svc.DeleteBackupVersion(c.Context(), user.ID, version); err != nil {
		if err == ErrBackupVersionNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "backup version not found"})
		}
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "version": version})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

// PutBackupKeys stores a batch of encrypted room keys.
// PUT /devices/backup/keys?version=N
//
// Body and response mirror Matrix's /room_keys/keys so the client can hand the crypto
// engine's own request body straight through and feed the response back to it.
func (h *Handler) PutBackupKeys(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	version, ok := backupVersionParam(c)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "version required"})
	}
	var in PutBackupKeysInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	count, etag, err := h.svc.PutBackupKeys(c.Context(), user.ID, version, &in)
	if err != nil {
		switch err {
		case ErrBackupVersionNotFound:
			// A different device replaced the version. The client re-reads it and retries,
			// so this is expected traffic rather than an error worth logging.
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "backup version not found"})
		case ErrInvalidInput:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room keys"})
		case ErrBackupFull:
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "backup is full"})
		}
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "version": version})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(fiber.Map{"count": count, "etag": etag})
}

// GetBackupKeys returns one page of a version's backed-up room keys.
// GET /devices/backup/keys?version=N[&after=<cursor>]
//
// `next` in the response is the cursor for the following page, and is absent on the last
// one. A restore walks these rather than asking for everything at once - see the service.
func (h *Handler) GetBackupKeys(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	version, ok := backupVersionParam(c)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "version required"})
	}
	rooms, next, err := h.svc.GetBackupKeys(c.Context(), user.ID, version, c.Query("after"))
	if err != nil {
		switch err {
		case ErrBackupVersionNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "backup version not found"})
		case ErrInvalidInput:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid cursor"})
		}
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "version": version})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := fiber.Map{"rooms": rooms}
	if next != "" {
		out["next"] = next
	}
	return c.JSON(out)
}
