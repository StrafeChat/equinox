package discover

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func errorFor(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotApproved), errors.Is(err, applications.ErrNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrNotAdmin), errors.Is(err, applications.ErrNotOwner):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, ErrNotListable), errors.Is(err, ErrInvalidKind), errors.Is(err, ErrInvalidStatus), errors.Is(err, ErrInvalidTagline),
		errors.Is(err, ErrInvalidTags), errors.Is(err, ErrInvalidNote), errors.Is(err, ErrInvalidDecision):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if status, msg, ok := spaces.HTTPError(err); ok {
		return c.Status(status).JSON(fiber.Map{"error": msg})
	}
	logger.Err("discover", err, nil)
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

func (h *Handler) actor(c fiber.Ctx) (*auth.User, bool) {
	user := auth.GetUser(c)
	if user == nil {
		_ = c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		return nil, false
	}
	return user, true
}

func idParam(c fiber.Ctx, what string) (int64, bool) {
	v, err := id.Parse(c.Params("id"))
	if err != nil {
		_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid " + what + " id"})
		return 0, false
	}
	return v, true
}

// Spaces GET /discover/spaces?q= and Bots GET /discover/bots?q= - the directory.
func (h *Handler) Spaces(c fiber.Ctx) error { return h.directory(c, KindSpace) }
func (h *Handler) Bots(c fiber.Ctx) error   { return h.directory(c, KindBot) }

func (h *Handler) directory(c fiber.Ctx, kind string) error {
	if _, ok := h.actor(c); !ok {
		return nil
	}
	entries, err := h.svc.Directory(c.Context(), kind, c.Query("q"))
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(entries)
}

// JoinSpace POST /discover/spaces/:id/join.
func (h *Handler) JoinSpace(c fiber.Ctx) error {
	user, ok := h.actor(c)
	if !ok {
		return nil
	}
	sid, ok := idParam(c, "space")
	if !ok {
		return nil
	}
	sp, err := h.svc.JoinSpace(c.Context(), user.ID, sid)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(fiber.Map{"id": id.Format(sp.ID), "name": sp.Name})
}

// ---- applicants -----------------------------------------------------------------------------

func (h *Handler) SpaceStatus(c fiber.Ctx) error   { return h.status(c, KindSpace, "space") }
func (h *Handler) BotStatus(c fiber.Ctx) error     { return h.status(c, KindBot, "application") }
func (h *Handler) SpaceApply(c fiber.Ctx) error    { return h.apply(c, KindSpace, "space") }
func (h *Handler) BotApply(c fiber.Ctx) error      { return h.apply(c, KindBot, "application") }
func (h *Handler) SpaceWithdraw(c fiber.Ctx) error { return h.withdraw(c, KindSpace, "space") }
func (h *Handler) BotWithdraw(c fiber.Ctx) error   { return h.withdraw(c, KindBot, "application") }

// status GET /spaces/:id/discover, GET /applications/:id/discover - {listing: ...|null}.
func (h *Handler) status(c fiber.Ctx, kind, what string) error {
	user, ok := h.actor(c)
	if !ok {
		return nil
	}
	target, ok := idParam(c, what)
	if !ok {
		return nil
	}
	l, err := h.svc.Status(c.Context(), user.ID, kind, target)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(fiber.Map{"listing": l})
}

// apply PUT /spaces/:id/discover, PUT /applications/:id/discover.
func (h *Handler) apply(c fiber.Ctx, kind, what string) error {
	user, ok := h.actor(c)
	if !ok {
		return nil
	}
	target, ok := idParam(c, what)
	if !ok {
		return nil
	}
	var in ApplyInput
	if len(c.Body()) > 0 && json.Unmarshal(c.Body(), &in) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	l, err := h.svc.Apply(c.Context(), user.ID, kind, target, in)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(l)
}

// withdraw DELETE /spaces/:id/discover, DELETE /applications/:id/discover.
func (h *Handler) withdraw(c fiber.Ctx, kind, what string) error {
	user, ok := h.actor(c)
	if !ok {
		return nil
	}
	target, ok := idParam(c, what)
	if !ok {
		return nil
	}
	if err := h.svc.Withdraw(c.Context(), user.ID, kind, target); err != nil {
		return errorFor(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

// ---- administrators -------------------------------------------------------------------------

// Queue GET /instance/discover?status=pending|approved|denied.
func (h *Handler) Queue(c fiber.Ctx) error {
	user, ok := h.actor(c)
	if !ok {
		return nil
	}
	status := c.Query("status")
	if status == "" {
		status = StatusPending
	}
	entries, err := h.svc.Queue(c.Context(), user.ID, status)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(entries)
}

// Review POST /instance/discover/:kind/:id/review {decision: approve|deny, note?}.
func (h *Handler) Review(c fiber.Ctx) error {
	user, ok := h.actor(c)
	if !ok {
		return nil
	}
	kind := c.Params("kind")
	target, ok := idParam(c, "listing")
	if !ok {
		return nil
	}
	var in struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if len(c.Body()) == 0 || json.Unmarshal(c.Body(), &in) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	l, err := h.svc.Review(c.Context(), user.ID, kind, target, in.Decision, in.Note)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(l)
}
