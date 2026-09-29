package unfurl

import (
	"net/http"

	"github.com/gofiber/fiber/v3"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Get handles GET /unfurl?url=<url>: the link's preview metadata, or an empty object when the
// link can't be previewed - never an error the client has to special-case, so a dead link just
// renders no card.
func (h *Handler) Get(c fiber.Ctx) error {
	u, ok := ParseURL(c.Query("url"))
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid url"})
	}
	meta, err := h.svc.Unfurl(c.Context(), u.String())
	if err != nil {
		return c.JSON(Metadata{})
	}
	return c.JSON(meta)
}
