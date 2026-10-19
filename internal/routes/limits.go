package routes

import (
	"github.com/StrafeChat/equinox/internal/federation"
	"net"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// perUserLimiter rate-limits a route per signed-in account (falling back to the client
// address), so one account cannot flood an endpoint from many addresses and one NAT does not
// throttle everyone behind it. Mount it after RequireAuth.
//
// This is the main brake on spam and automation: message sends, DM/group creation, friend
// requests, space/invite/channel creation, joins, reactions and the expensive list/search
// endpoints are all behind one. The windows are sized so a fast human never notices while a
// script gets throttled within seconds. Counters are in-process (Fiber's default memory
// storage): fine for the single API replica this deploys as - give the limiter a Redis
// Storage before running several replicas, or each one counts separately.
func perUserLimiter(max int, per time.Duration) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        max,
		Expiration: per,
		KeyGenerator: func(c fiber.Ctx) string {
			if u := auth.GetUser(c); u != nil {
				return "u:" + strconv.FormatInt(u.ID, 10)
			}
			return clientKey(c)
		},
		LimitReached: rateLimited,
	})
}

// clientKey is the limiter key for a client that has not signed in: its address, with an
// IPv6 address reduced to its /64. A residential connection has 2^64 of those, and a
// budget per address would be no budget at all; IPv4 stays per address.
func clientKey(c fiber.Ctx) string {
	ip := net.ParseIP(c.IP())
	if ip == nil {
		return "ip:" + c.IP()
	}
	if v4 := ip.To4(); v4 != nil {
		return "ip:" + v4.String()
	}
	return "ip6:" + ip.Mask(net.CIDRMask(64, 128)).String()
}

// perPeerLimiter throttles a signed server-to-server route by the instance calling it.
// Mounted after RequireInstance, which is what records the requester; before it the key
// would be an IP, and a peer's IP is not its identity.
func perPeerLimiter(max int, per time.Duration) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        max,
		Expiration: per,
		KeyGenerator: func(c fiber.Ctx) string {
			if d := federation.RequesterDomain(c); d != "" {
				return "peer:" + d
			}
			return clientKey(c)
		},
		LimitReached: rateLimited,
	})
}

// rateLimited is the shared 429 body: JSON with retry_after seconds, the same shape as a
// slowmode refusal, so the client shows "slow down" with a countdown instead of choking on
// Fiber's plain-text default. The limiter has already set Retry-After when it calls this.
func rateLimited(c fiber.Ctx) error {
	retry := 0
	if v := c.Response().Header.Peek("Retry-After"); len(v) > 0 {
		if n, err := strconv.Atoi(string(v)); err == nil {
			retry = n
		}
	}
	return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
		"error":       "you're doing that too fast - slow down a moment",
		"retry_after": retry,
	})
}
