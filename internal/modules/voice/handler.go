package voice

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp/fasthttpadaptor"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func voiceError(c fiber.Ctx, err error, fields map[string]any) error {
	switch {
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, spaces.ErrSpaceNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, rooms.ErrNotParticipant), errors.Is(err, ErrMissingPerm), errors.Is(err, spaces.ErrMissingPerm),
		errors.Is(err, spaces.ErrNotMember), errors.Is(err, ErrCannotModerateOwner):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrNotVoiceRoom), errors.Is(err, ErrInvalidTarget), errors.Is(err, spaces.ErrNothingToPatch):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrNotInVoice), errors.Is(err, ErrTargetNotInVoice), errors.Is(err, ErrNoCall):
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrRoomFull):
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": err.Error(), "code": "room_full"})
	}
	logger.Err("voice", err, fields)
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

func parseRoomID(c fiber.Ctx) (int64, error) {
	return id.Parse(c.Params("id"))
}

// Join POST /rooms/:id/voice/join. Body: { "self_mute": bool, "self_deaf": bool }.
func (h *Handler) Join(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var in JoinInput
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &in); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
		}
	}
	res, err := h.svc.Join(c.Context(), user, roomID, in)
	if err != nil {
		return voiceError(c, err, map[string]any{"room_id": roomID, "user_id": user.ID})
	}
	return c.JSON(res)
}

// Leave POST /voice/leave.
func (h *Handler) Leave(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if err := h.svc.Leave(c.Context(), user.ID); err != nil {
		if errors.Is(err, ErrNotInVoice) {
			return c.SendStatus(http.StatusNoContent)
		}
		return voiceError(c, err, map[string]any{"user_id": user.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// UpdateSelf PATCH /voice/state.
func (h *Handler) UpdateSelf(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in SelfInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	st, err := h.svc.UpdateSelf(c.Context(), user, in)
	if err != nil {
		return voiceError(c, err, map[string]any{"user_id": user.ID})
	}
	return c.JSON(st)
}

// States GET /rooms/:id/voice/states.
func (h *Handler) States(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	states, call, err := h.svc.RoomStates(c.Context(), user.ID, roomID)
	if err != nil {
		return voiceError(c, err, map[string]any{"room_id": roomID})
	}
	if states == nil {
		states = []State{}
	}
	return c.JSON(fiber.Map{"states": states, "call": call.View()})
}

type ringRequest struct {
	UserIDs []string `json:"user_ids,omitempty"`
}

// Ring POST /rooms/:id/call/ring. Body: { "user_ids": [...] } (optional).
func (h *Handler) Ring(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var body ringRequest
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &body); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
		}
	}
	ids := make([]int64, 0, len(body.UserIDs))
	for _, raw := range body.UserIDs {
		uid, err := id.Parse(raw)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
		}
		ids = append(ids, uid)
	}
	call, err := h.svc.Ring(c.Context(), user, roomID, ids)
	if err != nil {
		return voiceError(c, err, map[string]any{"room_id": roomID})
	}
	return c.JSON(call.View())
}

// Decline POST /rooms/:id/call/decline.
func (h *Handler) Decline(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	if err := h.svc.Decline(c.Context(), user, roomID); err != nil {
		if errors.Is(err, ErrNoCall) {
			return c.SendStatus(http.StatusNoContent)
		}
		return voiceError(c, err, map[string]any{"room_id": roomID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// Moderate PATCH /spaces/:id/members/:userId/voice. Body: { mute?, deaf?, room_id? }.
func (h *Handler) Moderate(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	targetID, err := id.Parse(c.Params("userId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var in ModerateInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if err := h.svc.Moderate(c.Context(), user, spaceID, targetID, in); err != nil {
		return voiceError(c, err, map[string]any{"space_id": spaceID, "target_id": targetID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// Disconnect DELETE /spaces/:id/members/:userId/voice.
func (h *Handler) Disconnect(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	targetID, err := id.Parse(c.Params("userId"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	if err := h.svc.Disconnect(c.Context(), user, spaceID, targetID); err != nil {
		return voiceError(c, err, map[string]any{"space_id": spaceID, "target_id": targetID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// Webhook POST /voice/webhook - called by LiveKit, authenticated by its signature (a
// JWT over the body's SHA-256, signed with our API secret), never by a session.
func (h *Handler) Webhook(c fiber.Ctx) error {
	var req http.Request
	if err := fasthttpadaptor.ConvertRequest(c.RequestCtx(), &req, true); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "bad request"})
	}
	ev, err := h.svc.lk.ReceiveWebhook(&req)
	if err != nil {
		logger.Warn("voice", "rejected webhook: %v", err)
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid signature"})
	}
	h.svc.HandleWebhook(c.Context(), ev)
	return c.SendStatus(http.StatusOK)
}
