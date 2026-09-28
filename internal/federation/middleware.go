package federation

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/logger"
)

const localsKeyDomain = "federation_domain"

// RequesterDomain is the verified peer domain of an inbound federation request.
func RequesterDomain(c fiber.Ctx) string {
	v, _ := c.Locals(localsKeyDomain).(string)
	return v
}

// RequireInstance verifies the request signature (see signing.go), the peer policy, the
// timestamp window and nonce uniqueness, then records the peer domain in Locals.
func (s *Service) RequireInstance() fiber.Handler {
	return func(c fiber.Ctx) error {
		domain := strings.ToLower(strings.TrimSpace(c.Get(HeaderInstance)))
		ts := strings.TrimSpace(c.Get(HeaderTimestamp))
		nonce := strings.TrimSpace(c.Get(HeaderNonce))
		sigHeader := strings.TrimSpace(c.Get(HeaderSignature))
		if domain == "" || ts == "" || nonce == "" || sigHeader == "" {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "missing federation signature headers"})
		}
		if len(nonce) > 128 || !ValidPeerDomain(s.fcfg, domain) || !s.fcfg.IsAllowedPeer(domain) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "instance not allowed"})
		}
		tsInt, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "bad timestamp"})
		}
		if skew := time.Now().Unix() - tsInt; skew > MaxClockSkewSeconds || skew < -MaxClockSkewSeconds {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "request timestamp out of range"})
		}
		eq := strings.IndexByte(sigHeader, '=')
		if eq <= 0 {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "bad signature header"})
		}
		keyID, sig := sigHeader[:eq], sigHeader[eq+1:]

		fedPath := strings.TrimPrefix(c.Path(), BasePath)
		if fedPath == "" {
			fedPath = "/"
		}
		if q := c.Request().URI().QueryString(); len(q) > 0 {
			fedPath += "?" + string(q)
		}
		payload := signingPayload(c.Method(), fedPath, ts, nonce, c.Body())

		ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
		defer cancel()
		info, err := s.discovery.Lookup(ctx, domain, false)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": domain, "stage": "discovery"})
			return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "could not verify requesting instance"})
		}
		ok := info.KeyID == keyID && VerifySignature(info.PublicKey, payload, sig)
		if !ok {
			// The peer may have rotated its key: refresh once before giving up.
			if fresh, ferr := s.discovery.Lookup(ctx, domain, true); ferr == nil {
				info = fresh
				ok = info.KeyID == keyID && VerifySignature(info.PublicKey, payload, sig)
			}
		}
		if !ok {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid federation signature"})
		}
		if s.redis != nil {
			set, rerr := s.redis.SetNX(ctx, "fed:nonce:"+domain+":"+nonce, "1", 2*MaxClockSkewSeconds*time.Second).Result()
			if rerr == nil && !set {
				return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "replayed request"})
			}
		}
		c.Locals(localsKeyDomain, domain)
		return c.Next()
	}
}
