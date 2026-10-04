package routes

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// perUserLimiter rate-limits a route per signed-in account (falling back to the client
// address), so one account cannot flood an upload endpoint from many addresses and one NAT
// does not throttle everyone behind it. Mount it after RequireAuth.
func perUserLimiter(max int, per time.Duration) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        max,
		Expiration: per,
		KeyGenerator: func(c fiber.Ctx) string {
			if u := auth.GetUser(c); u != nil {
				return "u:" + strconv.FormatInt(u.ID, 10)
			}
			return "ip:" + c.IP()
		},
	})
}
