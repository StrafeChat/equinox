package routes

import (
	"github.com/StrafeChat/equinox/internal/nebula"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"net/http"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// noBots refuses bot tokens on routes that are a person's to use: creating applications,
// and consenting to an OAuth2 authorization (including a bot install) on someone's behalf.
func noBots(c fiber.Ctx) error {
	if u := auth.GetUser(c); u != nil && u.Bot {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not available to bots"})
	}
	return c.Next()
}

func SetupApplicationRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	svc := applications.NewService(applications.NewRepository(d.Scylla), userRepo)
	svc.SetBanChecker(newBanChecker(d))
	svc.SetIconCheck(func(u string) bool {
		return strings.HasPrefix(u, nebula.PublicBase(d.Config.Nebula.BaseURL, d.Config.Nebula.PublicURL)+"/v1/")
	})
	if d.Redis != nil {
		region := ""
		if d.Config != nil {
			region = d.Config.Stargate.Region
		}
		svc.SetGateway(d.Redis, region)
	}
	h := applications.NewHandler(svc)

	// A bot authenticates its REST and gateway calls with `Authorization: Bot <token>`;
	// this is the one place that resolves such a token to its bot account.
	middleware.SetBotResolver(svc.ResolveBotToken)

	// Creating an app mints a bot user and secrets; keep a runaway client in check.
	writeLimiter := limiter.New(limiter.Config{KeyGenerator: clientKey, Max: 20, Expiration: time.Minute})

	// A bot has no business here: it cannot create applications (and so more bots).
	r := d.App.Group("/applications", requireAuth, noBots)
	r.Get("", h.List)
	r.Post("", writeLimiter, h.Create)
	r.Get("/:id", h.Get)
	r.Get("/:id/public", h.GetPublic)
	r.Patch("/:id", h.Patch)
	r.Delete("/:id", h.Delete)
	r.Post("/:id/secret", writeLimiter, h.ResetSecret)
	r.Post("/:id/bot", writeLimiter, h.AddBot)
	r.Post("/:id/bot/token", writeLimiter, h.ResetBotToken)
}
