package gifs

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Search handles GET /gifs/search?provider=&q=&limit=&offset=.
func (h *Handler) Search(c fiber.Ctx) error {
	page, err := h.svc.Search(c.Context(), c.Query("provider"), c.Query("q"),
		queryInt(c, "limit", defaultLimit), queryInt(c, "offset", 0))
	if err != nil {
		return gifError(c, err)
	}
	return c.JSON(page)
}

// Trending handles GET /gifs/trending?provider=&limit=&offset=.
func (h *Handler) Trending(c fiber.Ctx) error {
	page, err := h.svc.Trending(c.Context(), c.Query("provider"),
		queryInt(c, "limit", defaultLimit), queryInt(c, "offset", 0))
	if err != nil {
		return gifError(c, err)
	}
	return c.JSON(page)
}

// gifError is the one place a service error becomes a response. Every body is fixed text:
// whatever the provider or net/http said stays in the server log, scrubbed of the key.
func gifError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, ErrUnknownProvider):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unknown gif provider", "code": "gif_provider_unknown"})
	case errors.Is(err, ErrQueryRequired):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "query is required"})
	case errors.Is(err, ErrNotConfigured):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "gif provider not configured", "code": "gif_not_configured"})
	case errors.Is(err, ErrUpstreamThrottled):
		// The instance's key is out of quota for now: temporary, so 503 rather than 502.
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "gif provider unavailable", "code": "gif_upstream"})
	default:
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "gif provider unavailable", "code": "gif_upstream"})
	}
}

// queryInt reads an integer parameter, falling back to def when it is missing or not a
// number. An out-of-range number keeps its sign, so the service clamps it to the nearest
// bound instead of treating limit=99999999999 as "not given".
func queryInt(c fiber.Ctx, key string, def int) int {
	raw := c.Query(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return def
	}
	return int(n)
}
