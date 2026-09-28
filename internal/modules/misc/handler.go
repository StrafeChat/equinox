package misc

import (
	"github.com/StrafeChat/equinox/internal/config"
	"github.com/gofiber/fiber/v3"
)

type Handler struct {
	cfg *config.Config
}

func NewHandler(cfg *config.Config) *Handler {
	return &Handler{cfg: cfg}
}

type Feature struct {
	Enabled bool `json:"enabled"`
}

// CaptchaFeature carries the public half of the challenge config. The secret key is
// never included - only the site key, which is meant to be in the page.
type CaptchaFeature struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider,omitempty"`
	SiteKey  string `json:"site_key,omitempty"`
	// APIURL is where the widget fetches its challenge when the provider runs as its own
	// service the browser talks to directly (cap). Empty for every other provider.
	APIURL string `json:"api_url,omitempty"`
}

type Features struct {
	Captcha    CaptchaFeature `json:"captcha"`
	Email      Feature        `json:"email"`
	InviteOnly Feature        `json:"invite_only"`
	// Voice is on when a LiveKit server is configured; clients hide every call control
	// otherwise.
	Voice Feature `json:"voice"`
}

type Response struct {
	Strafe     Strafe     `json:"strafe"`
	Features   Features   `json:"features"`
	Federation Federation `json:"federation"`
}

type Strafe struct {
	Version string `json:"version"`
}

// Federation tells clients whether this instance federates and, if so, its domain -
// which is the server part of every local user's E2EE identity (@id:domain).
type Federation struct {
	Enabled bool   `json:"enabled"`
	Domain  string `json:"domain,omitempty"`
}

func captchaFeature(cfg *config.Config) CaptchaFeature {
	if !cfg.Flags.Captcha {
		return CaptchaFeature{Enabled: false}
	}
	f := CaptchaFeature{Enabled: true, Provider: cfg.Captcha.Provider, SiteKey: cfg.Captcha.SiteKey}
	if cfg.Captcha.Provider == "cap" {
		f.APIURL = cfg.Captcha.CapPublicURL
	}
	return f
}

func (h *Handler) Index(c fiber.Ctx) error {
	return c.JSON(Response{
		Strafe: Strafe{
			Version: h.cfg.App.Version,
		},
		Features: Features{
			Captcha:    captchaFeature(h.cfg),
			Email:      Feature{Enabled: h.cfg.Flags.Email},
			InviteOnly: Feature{Enabled: h.cfg.Flags.InviteOnly},
			Voice:      Feature{Enabled: h.cfg.Voice.Enabled},
		},
		Federation: Federation{
			Enabled: h.cfg.Federation.Enabled,
			Domain:  h.cfg.Federation.Domain,
		},
	})
}
