package devices

import (
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterDevice stores public keys for the current user's device. POST /devices
func (h *Handler) RegisterDevice(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in RegisterDeviceInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	if in.DeviceID == 0 || in.IdentityKey == "" || in.SignedPrekey == "" || in.SignedPrekeySig == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "device_id, identity_key, signed_prekey, signed_prekey_signature required"})
	}
	if err := h.svc.RegisterDevice(c.Context(), user.ID, &in); err != nil {
		logger.Err("devices", err, map[string]any{"user_id": user.ID, "device_id": in.DeviceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{"ok": true})
}

// GetPrekeyBundle returns a prekey bundle for establishing a session. GET /users/:user_id/devices/:device_id/prekey_bundle
func (h *Handler) GetPrekeyBundle(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	targetUserID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user_id"})
	}
	deviceID, err := id.Parse(c.Params("device_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid device_id"})
	}
	bundle, err := h.svc.GetPrekeyBundle(c.Context(), targetUserID, deviceID)
	if err != nil {
		logger.Err("devices", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if bundle == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "device not found"})
	}
	return c.JSON(bundle)
}

// ListDevices returns all devices for a user. GET /users/:user_id/devices
func (h *Handler) ListDevices(c fiber.Ctx) error {
	if auth.GetUser(c) == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	targetUserID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user_id"})
	}
	devices, err := h.svc.ListDevices(c.Context(), targetUserID)
	if err != nil {
		logger.Err("devices", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	out := make([]fiber.Map, 0, len(devices))
	for _, d := range devices {
		out = append(out, fiber.Map{
			"device_id": d.DeviceID,
		})
	}
	return c.JSON(out)
}
