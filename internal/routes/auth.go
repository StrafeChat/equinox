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
	twoFactorRepo := auth.NewTwoFactorRepository(d.Scylla)
	svc := auth.NewService(d.Config, userRepo, sessionRepo, twoFactorRepo, d.Redis)
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

	// Tighter than authLimiter: this is where a guessed TOTP/recovery code or a forged
	// passkey assertion would be tried. The per-token attempt budget in the service (5,
	// then the pending login is burned) is the other half of bounding this - the same
	// belt-and-suspenders shape the devices module uses for its own recovery endpoints.
	mfaLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
	})

	r := d.App.Group("/auth")

	r.Post("/login", authLimiter, h.Login)
	r.Post("/register", authLimiter, h.Register)
	// Only meaningful for a self-hosted provider (ALTCHA); 404 for the others.
	r.Get("/captcha/challenge", challengeLimiter, h.CaptchaChallenge)

	// Resolving the mfa_token a challenged /login returned - deliberately unauthenticated
	// (there is no session yet), gated by the token itself plus these limiters instead.
	r.Post("/2fa/totp", mfaLimiter, h.VerifyTOTP)
	r.Post("/2fa/recovery", mfaLimiter, h.VerifyRecoveryCode)
	r.Post("/2fa/webauthn/begin", mfaLimiter, h.BeginWebAuthnLogin)
	r.Post("/2fa/webauthn/finish", mfaLimiter, h.FinishWebAuthnLogin)

	r.Post("/logout", requireAuth, h.Logout)
	r.Post("/logout_all", requireAuth, h.LogoutAll)
}
