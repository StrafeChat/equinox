package config

import (
	"errors"
	"strconv"
	"strings"

	"github.com/StrafeChat/equinox/internal/logger"
)

type Config struct {
	App        AppConfig
	HTTP       HTTPConfig
	Session    SessionConfig
	Stargate   StargateConfig
	Nebula     NebulaConfig
	Federation FederationConfig
	Flags      FeatureFlags
	Captcha    CaptchaConfig
	Voice      VoiceConfig
	Database   DatabaseConfig
	Log        LogConfig
}

// VoiceConfig points the API at a LiveKit server, which carries every voice/video call
// (PMs, group PMs, space voice rooms). Disabled unless LIVEKIT_URL, LIVEKIT_API_KEY and
// LIVEKIT_API_SECRET are all set; a partial configuration is a startup error.
type VoiceConfig struct {
	Enabled bool
	// PublicURL is what browsers connect to (wss://chat.example.com/livekit in the
	// compose deployment, ws://localhost:7880 in development).
	PublicURL string
	// InternalURL is where the API reaches LiveKit's HTTP API for tokens, moderation and
	// participant listing (http://livekit:7880). Defaults to PublicURL with ws -> http.
	InternalURL string
	APIKey      string
	APISecret   string
}

// FederationConfig makes this instance addressable by other StrafeChat instances.
// Disabled unless FEDERATION_DOMAIN is set: then local users are `name#0001@Domain`,
// remote users can be messaged by handle, and every cross-instance request is signed with
// this instance's Ed25519 key and verified against the peer's published key.
type FederationConfig struct {
	Enabled bool
	// Domain is this instance's public name, e.g. "chat.example.com". It is the server part
	// of every local user's federated id (@<id>:<Domain>) and where peers look for
	// https://<Domain>/.well-known/strafe.
	Domain string
	// PublicURL is the browser/peer-facing base URL of this API (e.g. https://chat.example.com/api).
	// Peers call <PublicURL>/federation/v1/...; it's advertised in /.well-known/strafe.
	PublicURL string
	// GatewayURL is advertised in /.well-known/strafe for clients that discover an instance
	// (wss://chat.example.com/gateway/events).
	GatewayURL string
	// SigningKeySeed is a base64 32-byte Ed25519 seed. Empty = load/create KeyFile.
	SigningKeySeed string
	// KeyFile holds the auto-generated seed when SigningKeySeed is empty (default ./federation.key).
	KeyFile string
	// Allowlist, when non-empty, is the only set of domains this instance will talk to.
	Allowlist []string
	// Blocklist domains are refused both ways.
	Blocklist []string
	// StaticPeers overrides discovery: domain -> base URL where /.well-known/strafe lives.
	// For local development between instances that have no real DNS/TLS.
	StaticPeers map[string]string
	// AllowInsecure permits plain http:// peer URLs (development only).
	AllowInsecure bool
}

// IsAllowedPeer applies the allow/block lists.
func (f FederationConfig) IsAllowedPeer(domain string) bool {
	if !f.Enabled || domain == "" || domain == f.Domain {
		return false
	}
	for _, b := range f.Blocklist {
		if b == domain {
			return false
		}
	}
	if len(f.Allowlist) == 0 {
		return true
	}
	for _, a := range f.Allowlist {
		if a == domain {
			return true
		}
	}
	return false
}

// NebulaConfig is optional. When BaseURL and UploadSecret are set, POST /users/@me/avatar and /banner upload to Nebula.
type NebulaConfig struct {
	BaseURL        string // internal base URL (e.g. http://nebula:4010)
	PublicURL      string // browser-facing base for asset URLs (e.g. https://cdn.example.com); defaults to BaseURL
	UploadSecret   string // Bearer token for Nebula PUT
	AvatarMaxBytes int    // max multipart file size for avatars
	BannerMaxBytes int    // max multipart file size for profile banners (defaults to AvatarMaxBytes if 0)
	// AttachmentMaxBytes caps a single message attachment. HTTP.BodyLimitKB (and Nebula's
	// HTTP_BODY_LIMIT_MB) must be at least this large or uploads fail before reaching us.
	AttachmentMaxBytes int
	// EmojiMaxBytes caps a custom emoji image.
	EmojiMaxBytes int
}

type LogConfig struct {
	Level logger.Level
}

type AppConfig struct {
	Version       string
	SnowflakeNode int64 // node ID for snowflake generator (0-1023)
}

type HTTPConfig struct {
	Port        string
	BodyLimitKB int      // max request body size in KB (default 1024 = 1MB)
	CORSOrigins []string // allowed origins; nil = allow all (dev)
	// TrustedProxies lists the reverse proxies whose X-Forwarded-For header is believed
	// (IPs, CIDRs, or the words "loopback", "linklocal", "private"). Empty = none: c.IP()
	// is the TCP peer, so behind a proxy every request would share the proxy's address and
	// the per-IP rate limits would throttle all users together.
	TrustedProxies []string
}

type StargateConfig struct {
	Port            string   // WebSocket server port (e.g. 4001)
	Region          string   // instance region for multi-region (e.g. "us-east", "eu-west")
	AllowedOrigins  []string // allowed origins for CheckOrigin (e.g. https://web.strafe.chat)
	ReadBufferSize  int      // bytes (default 4096)
	WriteBufferSize int      // bytes (default 4096)
}

type SessionConfig struct {
	TTLSeconds int
	TokenBytes int
}

type FeatureFlags struct {
	Captcha    bool
	Email      bool
	InviteOnly bool
	// InstanceAdmins may mint and revoke instance invites regardless of what the database
	// says. The account that registers first is made an admin automatically, so this is
	// the escape hatch for an operator who has lost that account - never the usual route.
	InstanceAdmins []int64
}

// CaptchaConfig configures the registration challenge. It only matters when
// Flags.Captcha is on; Load() refuses to start a half-configured instance rather than
// letting an operator believe registration is protected when it isn't.
type CaptchaConfig struct {
	// Provider is "turnstile", "friendly", "altcha" or "cap" (see internal/captcha).
	Provider string
	// SiteKey is public and is handed to clients so they can render the widget
	// (turnstile, friendly, cap). ALTCHA has none: the challenge comes from this server.
	SiteKey string
	// SecretKey verifies Turnstile and Cap tokens server-side. Never leaves the backend.
	SecretKey string
	// APIKey authenticates Friendly Captcha siteverify calls. Never leaves the backend.
	APIKey string
	// HMACKey signs ALTCHA challenges. Any random string of 32+ bytes; never leaves the
	// backend.
	HMACKey string
	// CapPublicURL is the Cap instance as a *browser* must reach it (the widget fetches
	// its challenge straight from Cap, not through this API), so it is public and goes
	// out in GET /. CapInternalURL is how this server reaches it - inside compose that is
	// http://cap:3000, which never leaves the private network - and defaults to
	// CapPublicURL when unset, which is right for a single-host deployment.
	CapPublicURL   string
	CapInternalURL string
}

type DatabaseConfig struct {
	Scylla ScyllaConfig
	Redis  RedisConfig
}

type ScyllaConfig struct {
	Hosts    []string
	Port     int
	Keyspace string
}

type RedisConfig struct {
	Addr         string
	DB           int    // logical database index (default 0) - lets two instances share one Redis
	PoolSize     int    // max connections in pool (default 10)
	CacheEnabled bool   // enable Redis cache-aside for sessions, users, relationships
	CachePrefix  string // Redis key prefix (e.g. "equinox:") for cache keys
}

func Load() (*Config, error) {
	cfg := &Config{
		App: AppConfig{
			Version:       getEnvString("VERSION", "1.0.0"),
			SnowflakeNode: int64(getEnvInt("SNOWFLAKE_NODE_ID", 0)),
		},
		HTTP: HTTPConfig{
			Port:           getEnvString("PORT", "4000"),
			BodyLimitKB:    getEnvInt("HTTP_BODY_LIMIT_KB", 0), // 0 = derived below from the attachment cap
			CORSOrigins:    getEnvArray("CORS_ORIGINS", nil),
			TrustedProxies: getEnvArray("TRUSTED_PROXIES", nil),
		},
		Session: SessionConfig{
			TTLSeconds: getEnvInt("SESSION_TTL_SECONDS", 86400*7), // 7 days
			TokenBytes: getEnvInt("SESSION_TOKEN_BYTES", 32),
		},
		Stargate: StargateConfig{
			Port:            getEnvString("STARGATE_PORT", "4001"),
			Region:          getEnvString("STARGATE_REGION", "default"),
			AllowedOrigins:  getEnvArray("STARGATE_ALLOWED_ORIGINS", nil),
			ReadBufferSize:  getEnvInt("STARGATE_READ_BUFFER", 4096),
			WriteBufferSize: getEnvInt("STARGATE_WRITE_BUFFER", 4096),
		},
		Nebula: NebulaConfig{
			BaseURL:            getEnvString("NEBULA_BASE_URL", ""),
			PublicURL:          getEnvString("NEBULA_PUBLIC_URL", ""),
			UploadSecret:       getEnvString("NEBULA_UPLOAD_SECRET", ""),
			AvatarMaxBytes:     getEnvInt("NEBULA_AVATAR_MAX_MB", 8) * 1024 * 1024,
			BannerMaxBytes:     getEnvInt("NEBULA_BANNER_MAX_MB", 8) * 1024 * 1024,
			AttachmentMaxBytes: getEnvInt("NEBULA_ATTACHMENT_MAX_MB", 25) * 1024 * 1024,
			EmojiMaxBytes:      getEnvInt("NEBULA_EMOJI_MAX_KB", 512) * 1024,
		},
		Federation: FederationConfig{
			Domain:         strings.ToLower(strings.TrimSpace(getEnvString("FEDERATION_DOMAIN", ""))),
			PublicURL:      strings.TrimRight(strings.TrimSpace(getEnvString("FEDERATION_PUBLIC_URL", "")), "/"),
			GatewayURL:     strings.TrimSpace(getEnvString("FEDERATION_GATEWAY_URL", "")),
			SigningKeySeed: strings.TrimSpace(getEnvString("FEDERATION_SIGNING_KEY", "")),
			KeyFile:        getEnvString("FEDERATION_KEY_FILE", "./federation.key"),
			Allowlist:      cleanDomains(getEnvArray("FEDERATION_ALLOWLIST", nil)),
			Blocklist:      cleanDomains(getEnvArray("FEDERATION_BLOCKLIST", nil)),
			StaticPeers:    parseStaticPeers(getEnvString("FEDERATION_STATIC_PEERS", "")),
			AllowInsecure:  getEnvBool("FEDERATION_ALLOW_INSECURE", false),
		},
		Flags: FeatureFlags{
			Captcha:        getEnvBool("CAPTCHA", false),
			Email:          getEnvBool("EMAIL", false),
			InviteOnly:     getEnvBool("INVITE_ONLY", false),
			InstanceAdmins: parseIDList(getEnvArray("INSTANCE_ADMINS", nil)),
		},
		Captcha: loadCaptchaConfig(),
		Voice:   loadVoiceConfig(),
		Database: DatabaseConfig{
			Scylla: ScyllaConfig{
				Hosts:    getEnvArray("SCYLLA_HOSTS", []string{"localhost"}),
				Port:     getEnvInt("SCYLLA_PORT", 9042),
				Keyspace: getEnvString("SCYLLA_KEYSPACE", "strafechat"),
			},
			Redis: RedisConfig{
				Addr:         getEnvStringOr("REDIS_ADDR", "REDIS_ADDRS", "localhost:6379"),
				DB:           getEnvInt("REDIS_DB", 0),
				PoolSize:     getEnvInt("REDIS_POOL_SIZE", 10),
				CacheEnabled: getEnvBool("REDIS_CACHE_ENABLED", true),
				CachePrefix:  getEnvString("REDIS_CACHE_PREFIX", "equinox:"),
			},
		},
		Log: LogConfig{
			Level: logger.ParseLevel(getEnvString("LOG_LEVEL", "info")),
		},
	}

	cfg.Federation.Enabled = cfg.Federation.Domain != ""

	// The body limit has to fit the largest attachment plus multipart overhead, or uploads
	// fail with a 413 before the attachment code ever sees them. Deriving it means raising
	// ATTACHMENT_MAX_MB is one change, not two that have to be kept in step by hand.
	if cfg.HTTP.BodyLimitKB <= 0 {
		cfg.HTTP.BodyLimitKB = max(1024, cfg.Nebula.AttachmentMaxBytes/1024+2048)
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// parseIDList turns a list of snowflake strings into ids, skipping anything unparseable
// rather than refusing to boot: a typo in INSTANCE_ADMINS should cost that one entry, not
// the whole instance.
func parseIDList(in []string) []int64 {
	out := make([]int64, 0, len(in))
	for _, raw := range in {
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanDomains(list []string) []string {
	out := make([]string, 0, len(list))
	for _, d := range list {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// parseStaticPeers parses "domain=https://host[,domain2=http://host2]".
func parseStaticPeers(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, '=')
		if i <= 0 {
			continue
		}
		domain := strings.ToLower(strings.TrimSpace(part[:i]))
		base := strings.TrimRight(strings.TrimSpace(part[i+1:]), "/")
		if domain != "" && base != "" {
			out[domain] = base
		}
	}
	return out
}

func validate(cfg *Config) error {
	if cfg.Federation.Enabled {
		if cfg.Federation.PublicURL == "" {
			return errors.New("FEDERATION_PUBLIC_URL is required when FEDERATION_DOMAIN is set")
		}
		if !cfg.Federation.AllowInsecure && !strings.HasPrefix(cfg.Federation.PublicURL, "https://") {
			return errors.New("FEDERATION_PUBLIC_URL must be https:// (or set FEDERATION_ALLOW_INSECURE=true for development)")
		}
		// A federating instance is a public one. With no allowed origins the gateway
		// accepts a WebSocket from any site, and since it also accepts the session cookie,
		// any page a signed-in user visits could open an authenticated gateway connection
		// in their name (cross-site WebSocket hijacking). Refuse to boot that way rather
		// than run open while the operator believes the instance is locked down.
		if !cfg.Federation.AllowInsecure && len(cfg.Stargate.AllowedOrigins) == 0 {
			return errors.New("STARGATE_ALLOWED_ORIGINS is required on a federating instance (the web client's origin, e.g. https://chat.example.org)")
		}
	}
	if cfg.HTTP.Port == "" {
		return errors.New("PORT is required")
	}
	if _, err := strconv.Atoi(cfg.HTTP.Port); err != nil {
		return errors.New("PORT must be numeric")
	}
	if cfg.App.SnowflakeNode < 0 || cfg.App.SnowflakeNode > 1023 {
		return errors.New("SNOWFLAKE_NODE_ID must be 0-1023 (unique per instance)")
	}
	// Refuse to boot half-configured rather than accepting every registration while the
	// operator believes the instance is protected.
	if cfg.Flags.Captcha {
		if err := validateCaptcha(cfg.Captcha); err != nil {
			return err
		}
	}
	if err := validateVoice(cfg.Voice); err != nil {
		return err
	}
	return nil
}

func loadVoiceConfig() VoiceConfig {
	env := func(k string) string { return strings.TrimSpace(getEnvString(k, "")) }
	v := VoiceConfig{
		PublicURL:   strings.TrimRight(env("LIVEKIT_URL"), "/"),
		InternalURL: strings.TrimRight(env("LIVEKIT_INTERNAL_URL"), "/"),
		APIKey:      env("LIVEKIT_API_KEY"),
		APISecret:   env("LIVEKIT_API_SECRET"),
	}
	v.Enabled = v.PublicURL != "" || v.APIKey != "" || v.APISecret != ""
	if v.InternalURL == "" && v.PublicURL != "" {
		// ws(s):// for browsers, http(s):// for the server API - same host and port.
		switch {
		case strings.HasPrefix(v.PublicURL, "wss://"):
			v.InternalURL = "https://" + strings.TrimPrefix(v.PublicURL, "wss://")
		case strings.HasPrefix(v.PublicURL, "ws://"):
			v.InternalURL = "http://" + strings.TrimPrefix(v.PublicURL, "ws://")
		default:
			v.InternalURL = v.PublicURL
		}
	}
	return v
}

func validateVoice(v VoiceConfig) error {
	if !v.Enabled {
		return nil
	}
	if v.PublicURL == "" || v.APIKey == "" || v.APISecret == "" {
		return errors.New("voice needs all of LIVEKIT_URL, LIVEKIT_API_KEY and LIVEKIT_API_SECRET (unset all three to run without voice)")
	}
	if !strings.HasPrefix(v.PublicURL, "ws://") && !strings.HasPrefix(v.PublicURL, "wss://") {
		return errors.New("LIVEKIT_URL must be a ws:// or wss:// URL (what browsers connect to)")
	}
	if !strings.HasPrefix(v.InternalURL, "http://") && !strings.HasPrefix(v.InternalURL, "https://") {
		return errors.New("LIVEKIT_INTERNAL_URL must be an http:// or https:// URL")
	}
	// LiveKit itself refuses secrets shorter than this; fail here with a clearer message.
	if len(v.APISecret) < 32 {
		return errors.New("LIVEKIT_API_SECRET must be at least 32 characters (openssl rand -hex 32)")
	}
	return nil
}

// Supported registration challenge providers. The site key (public, sent to browsers) and
// the server-side secret have provider-specific names so an operator switching provider
// cannot accidentally hand one service's secret to another.
const supportedCaptchaProviders = "turnstile, friendly, altcha, cap"

func loadCaptchaConfig() CaptchaConfig {
	env := func(k string) string { return strings.TrimSpace(getEnvString(k, "")) }
	capPublic := strings.TrimRight(env("CAP_API_URL"), "/")
	c := CaptchaConfig{
		Provider:       strings.ToLower(strings.TrimSpace(getEnvString("CAPTCHA_PROVIDER", "altcha"))),
		APIKey:         env("FRIENDLY_CAPTCHA_API_KEY"),
		HMACKey:        env("ALTCHA_HMAC_KEY"),
		CapPublicURL:   capPublic,
		CapInternalURL: firstNonEmpty(strings.TrimRight(env("CAP_INTERNAL_URL"), "/"), capPublic),
	}
	// The site and secret keys are read from the chosen provider's own variables, never
	// from whichever happens to be filled in. An .env that still carries a previous
	// provider's keys is extremely normal - switching provider is exactly when they are
	// both present - and taking the wrong one would hand one service's key to another.
	switch c.Provider {
	case "turnstile":
		c.SiteKey, c.SecretKey = env("TURNSTILE_SITE_KEY"), env("TURNSTILE_SECRET_KEY")
	case "friendly":
		c.SiteKey = env("FRIENDLY_CAPTCHA_SITE_KEY")
	case "cap":
		c.SiteKey, c.SecretKey = env("CAP_SITE_KEY"), env("CAP_SECRET_KEY")
	}
	return c
}

func validateCaptcha(c CaptchaConfig) error {
	switch c.Provider {
	case "turnstile":
		if c.SiteKey == "" || c.SecretKey == "" {
			return errors.New("CAPTCHA_PROVIDER=turnstile requires TURNSTILE_SITE_KEY and TURNSTILE_SECRET_KEY")
		}
	case "friendly":
		if c.SiteKey == "" || c.APIKey == "" {
			return errors.New("CAPTCHA_PROVIDER=friendly requires FRIENDLY_CAPTCHA_SITE_KEY and FRIENDLY_CAPTCHA_API_KEY")
		}
	case "altcha":
		// Anything shorter is a key someone typed rather than generated, and the challenge
		// signature is only as strong as this.
		if len(c.HMACKey) < 32 {
			return errors.New("CAPTCHA_PROVIDER=altcha requires ALTCHA_HMAC_KEY of at least 32 characters (openssl rand -hex 32)")
		}
	case "cap":
		if c.CapPublicURL == "" || c.SiteKey == "" || c.SecretKey == "" {
			return errors.New("CAPTCHA_PROVIDER=cap requires CAP_API_URL, CAP_SITE_KEY and CAP_SECRET_KEY (create the site in the Cap dashboard)")
		}
		// The browser is sent here, so a bare host or a typo'd scheme would leave the
		// widget unable to fetch its challenge with nothing to say why.
		if !strings.HasPrefix(c.CapPublicURL, "http://") && !strings.HasPrefix(c.CapPublicURL, "https://") {
			return errors.New("CAP_API_URL must start with http:// or https://")
		}
	case "":
		return errors.New("CAPTCHA=true requires CAPTCHA_PROVIDER (supported: " + supportedCaptchaProviders + ")")
	default:
		return errors.New("unsupported CAPTCHA_PROVIDER " + c.Provider + " (supported: " + supportedCaptchaProviders + ")")
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
