package misc

import (
	"strings"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/nebula"
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

// Service is one component that makes up a running instance, advertised on GET / so clients
// and tooling can discover the instance's topology. Only browser-facing public URLs are
// exposed here - never an internal address, API key or upload secret.
type Service struct {
	// Enabled is emitted only for services the instance can run without (voice, the CDN),
	// so a client can tell "the operator turned this off" from "not reachable". Services
	// that always run when the instance is up (the gateway, the web client) omit it.
	Enabled *bool `json:"enabled,omitempty"`
	// URL is where a browser reaches the service. For required services it's only advertised
	// when configured; for optional ones it's present only while the service is enabled.
	URL string `json:"url,omitempty"`
}

// Services is the instance's service map. The gateway and web client are core and carry
// just a URL (advertised when configured). The CDN and voice are optional and always
// appear with an enabled flag, plus their URL when on.
type Services struct {
	Stargate *Service `json:"stargate,omitempty"`
	Web      *Service `json:"web,omitempty"`
	CDN      Service  `json:"cdn"`
	Voice    Service  `json:"voice"`
}

type Response struct {
	Strafe     Strafe     `json:"strafe"`
	Features   Features   `json:"features"`
	Services   Services   `json:"services"`
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

func boolPtr(b bool) *bool { return &b }

// services advertises the instance's components and their browser-facing URLs. Required
// services (gateway, web) are listed only when their public URL is configured; optional
// ones (CDN, voice) are always listed with an enabled flag so a client can tell off from
// missing.
func services(cfg *config.Config) Services {
	s := Services{}

	// Gateway (stargate): prefer an explicit public URL, else the federation gateway URL.
	// The gateway always runs; this only controls what URL is advertised to clients.
	gatewayURL := cfg.Stargate.PublicURL
	if gatewayURL == "" {
		gatewayURL = strings.TrimSpace(cfg.Federation.GatewayURL)
	}
	if gatewayURL != "" {
		s.Stargate = &Service{URL: gatewayURL}
	}

	// Web client.
	if cfg.App.WebURL != "" {
		s.Web = &Service{URL: cfg.App.WebURL}
	}

	// CDN (Nebula): on when uploads are wired up - the same rule the upload handlers use.
	// The URL is the browser-facing asset base, never the internal upload endpoint.
	cdnOn := strings.TrimSpace(cfg.Nebula.BaseURL) != "" && strings.TrimSpace(cfg.Nebula.UploadSecret) != ""
	s.CDN = Service{Enabled: boolPtr(cdnOn)}
	if cdnOn {
		s.CDN.URL = nebula.PublicBase(cfg.Nebula.BaseURL, cfg.Nebula.PublicURL)
	}

	// Voice (LiveKit): the wss:// URL browsers connect to when enabled - never the internal
	// URL, API key or secret.
	s.Voice = Service{Enabled: boolPtr(cfg.Voice.Enabled)}
	if cfg.Voice.Enabled {
		s.Voice.URL = cfg.Voice.PublicURL
	}

	return s
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
		Services: services(h.cfg),
		Federation: Federation{
			Enabled: h.cfg.Federation.Enabled,
			Domain:  h.cfg.Federation.Domain,
		},
	})
}
