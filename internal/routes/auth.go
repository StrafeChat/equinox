package routes

import (
	"time"

	"github.com/StrafeChat/equinox/internal/captcha"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

// newCaptchaVerifier returns nil when the instance has no challenge configured, which is
// what turns the whole feature off. config.validate already rejected a CAPTCHA=true
// instance with missing keys, so reaching here with the flag on means it is usable.
func newCaptchaVerifier(d Deps) captcha.Verifier {
	cfg := d.Config
	if !cfg.Flags.Captcha {
		return nil
	}
	switch cfg.Captcha.Provider {
	case "turnstile":
		return captcha.NewTurnstile(cfg.Captcha.SecretKey)
	case "friendly":
		return captcha.NewFriendlyCaptcha(cfg.Captcha.APIKey, cfg.Captcha.SiteKey)
	case "cap":
		// Verification goes to the Cap container over the private network; the browser
		// talks to it separately at the public URL to solve the challenge.
		return captcha.NewCap(cfg.Captcha.CapInternalURL, cfg.Captcha.SiteKey, cfg.Captcha.SecretKey)
	case "altcha":
		// Spent challenges are shared through Redis so a solution accepted by one API
		// replica cannot be replayed against another; a single process falls back to memory.
		var guard captcha.ReplayGuard
		if d.Redis != nil {
			guard = captcha.NewRedisReplayGuard(d.Redis, cfg.Database.Redis.CachePrefix)
		}
		return captcha.NewAltcha(cfg.Captcha.HMACKey, guard)
	default:
		return nil
	}
}

func SetupAuthRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	svc := auth.NewService(d.Config, userRepo, sessionRepo)
	// Registration needs the instance module to redeem an invite and to hand the first
	// account its administrator bit. Without this, INVITE_ONLY can only close the door.
	if setter, ok := svc.(auth.GateSetter); ok {
		inst := newInstanceService(d, userRepo, sessionRepo)
		setter.SetInviteGate(inst)
		setter.SetBanChecker(inst)
	}
	h := auth.NewHandlerWithCaptcha(svc, newCaptchaVerifier(d))

	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	// Rate limit auth endpoints: 10 attempts per minute per IP
	authLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
	})

	// Challenge fetches happen on every registration page load and on every widget reset,
	// so they get a looser budget than the login/register attempts themselves.
	challengeLimiter := limiter.New(limiter.Config{
		Max:        30,
		Expiration: time.Minute,
	})

	r := d.App.Group("/auth")

	r.Post("/login", authLimiter, h.Login)
	r.Post("/register", authLimiter, h.Register)
	// Only meaningful for a self-hosted provider (ALTCHA); 404 for the others.
	r.Get("/captcha/challenge", challengeLimiter, h.CaptchaChallenge)

	r.Post("/logout", requireAuth, h.Logout)
	r.Post("/logout_all", requireAuth, h.LogoutAll)
}
