package push

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// Handler serves the push REST API. Every /users/@me/push route acts on the subscription
// of the session making the request: a subscription is a signed-in device, so a bot token
// (no session) has none, and an OAuth2 token never gets this far (RequireAuth refuses it).
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Disabled answers every push route when PUSH_ENABLED is off.
func Disabled(c fiber.Ctx) error {
	return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": ErrDisabled.Error()})
}

// VAPIDKey handles GET /push/vapid-key: the instance's application-server public key, which
// a browser's PushManager needs to subscribe. Public and harmless.
func (h *Handler) VAPIDKey(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"public_key": h.svc.VAPIDPublicKey()})
}

func session(c fiber.Ctx) (int64, int64, error) {
	u, s := auth.GetUser(c), auth.GetSession(c)
	if u == nil || s == nil {
		return 0, 0, ErrSessionRequired
	}
	return u.ID, s.SessionID, nil
}

// Put handles PUT /users/@me/push: subscribe (or re-subscribe) this session.
func (h *Handler) Put(c fiber.Ctx) error {
	userID, sessionID, err := session(c)
	if err != nil {
		return pushError(c, err)
	}
	var in SubscribeInput
	if len(c.Body()) == 0 || json.Unmarshal(c.Body(), &in) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	sub, err := h.svc.Subscribe(c.Context(), userID, sessionID, &in)
	if err != nil {
		return pushError(c, err)
	}
	return c.JSON(fiber.Map{"subscription": sub.View()})
}

// Patch handles PATCH /users/@me/push: change this session's preferences.
func (h *Handler) Patch(c fiber.Ctx) error {
	userID, sessionID, err := session(c)
	if err != nil {
		return pushError(c, err)
	}
	var in UpdateInput
	if len(c.Body()) == 0 || json.Unmarshal(c.Body(), &in) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	sub, err := h.svc.Update(c.Context(), userID, sessionID, &in)
	if err != nil {
		return pushError(c, err)
	}
	return c.JSON(fiber.Map{"subscription": sub.View()})
}

// Get handles GET /users/@me/push.
func (h *Handler) Get(c fiber.Ctx) error {
	userID, sessionID, err := session(c)
	if err != nil {
		return pushError(c, err)
	}
	sub, err := h.svc.Get(c.Context(), userID, sessionID)
	if err != nil {
		return pushError(c, err)
	}
	return c.JSON(fiber.Map{"subscription": sub.View()})
}

// Delete handles DELETE /users/@me/push. Idempotent.
func (h *Handler) Delete(c fiber.Ctx) error {
	userID, sessionID, err := session(c)
	if err != nil {
		return pushError(c, err)
	}
	if err := h.svc.Delete(c.Context(), userID, sessionID); err != nil {
		return pushError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

// Test handles POST /users/@me/push/test.
func (h *Handler) Test(c fiber.Ctx) error {
	userID, sessionID, err := session(c)
	if err != nil {
		return pushError(c, err)
	}
	res, err := h.svc.Test(c.Context(), userID, sessionID)
	if err != nil {
		return pushError(c, err)
	}
	return c.JSON(res)
}

func pushError(c fiber.Ctx, err error) error {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": ve.Reason})
	case errors.Is(err, ErrNoSubscription):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrSessionRequired):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	}
	logger.Err("push", err, map[string]any{"path": c.Path()})
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}
