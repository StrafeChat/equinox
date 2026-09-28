package instance

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// errorFor maps the module's errors onto status codes. ErrNotAdmin is 403 rather than 404
// because the caller is authenticated and the route exists; there is nothing to hide.
func errorFor(c fiber.Ctx, err error) error {
	switch err {
	case ErrNotAdmin:
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "instance administrator only"})
	case ErrInviteNotFound:
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "invite not found"})
	case ErrInvalidInvite:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	case ErrNoteTooLong:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	default:
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
}

// ListInvites GET /instance/invites
func (h *Handler) ListInvites(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	invites, err := h.svc.ListInvites(c.Context(), user.ID)
	if err != nil {
		return errorFor(c, err)
	}
	if invites == nil {
		invites = []Invite{}
	}
	return c.JSON(invites)
}

// CreateInvite POST /instance/invites
func (h *Handler) CreateInvite(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var body CreateInviteInput
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &body); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
		}
	}
	inv, err := h.svc.CreateInvite(c.Context(), user.ID, &body)
	if err != nil {
		return errorFor(c, err)
	}
	return c.Status(http.StatusCreated).JSON(inv)
}

// RevokeInvite DELETE /instance/invites/:code
func (h *Handler) RevokeInvite(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	// UnescapePath is off on this app, so route params arrive percent-encoded.
	code, err := url.PathUnescape(c.Params("code"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid code"})
	}
	if err := h.svc.RevokeInvite(c.Context(), user.ID, code); err != nil {
		return errorFor(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

// CheckInvite GET /instance/invites/:code/check - unauthenticated, because it is used by
// the registration form before anyone has an account. Returns only whether the code would
// be accepted; rate limiting is what keeps it from being an enumeration oracle.
func (h *Handler) CheckInvite(c fiber.Ctx) error {
	code, err := url.PathUnescape(c.Params("code"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid code"})
	}
	ok, err := h.svc.CheckInvite(c.Context(), code)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(fiber.Map{"valid": ok})
}

// Me GET /instance/me - what the signed-in account may do at instance level. The client
// uses it to decide whether to show the instance settings section at all.
func (h *Handler) Me(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	admin, err := h.svc.IsAdmin(c.Context(), user.ID)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(fiber.Map{"instance_admin": admin})
}
