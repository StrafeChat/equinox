package media

import (
	"errors"

	"github.com/gofiber/fiber/v3"
)

// Handler serves GET /media/v1/external/:sig/:src.
type Handler struct {
	svc *Service
	// clientKey is the rate-limit key of the reader (their address, an IPv6 one by /64).
	clientKey func(fiber.Ctx) string
}

func NewHandler(svc *Service, clientKey func(fiber.Ctx) string) *Handler {
	return &Handler{svc: svc, clientKey: clientKey}
}

// External sends the image a signed proxy address names. No auth: the signature is the
// permission (an <img> cannot send a token), checked before anything is fetched or read.
func (h *Handler) External(c fiber.Ctx) error {
	src, ok := h.svc.Source(c.Params("sig"), c.Params("src"))
	if !ok {
		return fail(c, ErrBadURL)
	}
	key := ""
	if h.clientKey != nil {
		key = h.clientKey(c)
	}
	img, err := h.svc.Open(c.Context(), src, key)
	if err != nil {
		return fail(c, err)
	}
	safetyHeaders(c)
	c.Set(fiber.HeaderContentType, img.ContentType)
	c.Set(fiber.HeaderCacheControl, "public, max-age=86400")
	return c.SendStream(img.Body, int(img.Size))
}

// safetyHeaders go on every answer. The bytes are same-origin with the web client (both are
// behind the one domain), so a browser must never treat them as anything but the image type
// sent, never run anything in them if opened directly, and never tell the next site where
// they were seen; other origins (the desktop app's) may still show them.
func safetyHeaders(c fiber.Ctx) {
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderContentSecurityPolicy, "default-src 'none'; sandbox")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	c.Set(fiber.HeaderCrossOriginResourcePolicy, "cross-origin")
}

func fail(c fiber.Ctx, err error) error {
	var e *Error
	if !errors.As(err, &e) {
		e = ErrUpstream
	}
	safetyHeaders(c)
	// The failure is remembered here for a while; a browser need not ask again sooner.
	c.Set(fiber.HeaderCacheControl, "public, max-age=900")
	if e == ErrBusy || e == ErrFull || e == ErrBadURL {
		c.Set(fiber.HeaderCacheControl, "no-store")
	}
	if e == ErrBusy || e == ErrFull {
		c.Set(fiber.HeaderRetryAfter, retryAfter(e))
	}
	return c.Status(e.Status).JSON(fiber.Map{"error": e.Reason})
}
