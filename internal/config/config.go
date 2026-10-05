package config

import (
	"encoding/hex"
	"errors"
	netmail "net/mail"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"

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
	TwoFactor  TwoFactorConfig
	Mail       MailConfig
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
	// Runtime holds the admin-managed allow/block lists (edited live from the dashboard and
	// persisted in federation_peer_policy), merged with the static env lists by IsAllowedPeer.
	// A shared pointer so every copy of this config value sees the same live lists; the
	// instance module loads it at boot and updates it on change. nil = env lists only.
	Runtime *RuntimePeerLists
	// StaticPeers overrides discovery: domain -> base URL where /.well-known/strafe lives.
	// For local development between instances that have no real DNS/TLS.
	StaticPeers map[string]string
	// AllowInsecure permits plain http:// peer URLs (development only).
	AllowInsecure bool
	// MemberPage is how many members of a hosted space go in one page when another
	// instance builds or resyncs its mirror (1-1000; default 1000). A tuning knob.
	MemberPage int
}

// FederationPolicyReloadChannel is the Redis pub/sub channel the admin dashboard publishes on
// after changing the runtime allow/block list, so every API node reloads it from the database.
const FederationPolicyReloadChannel = "equinox:fed:policy:reload"

// RuntimePeerLists is the admin-managed allow/block list, swapped atomically under a lock.
// Entries are lowercased exact domains. Reads hand back the live slices (never mutated in
// place - Set replaces them), so IsAllowedPeer can iterate without copying.
type RuntimePeerLists struct {
	mu    sync.RWMutex
	allow []string
	block []string
}

// NewRuntimePeerLists returns an empty list set (env-only until the instance module loads it).
func NewRuntimePeerLists() *RuntimePeerLists { return &RuntimePeerLists{} }

// Set replaces the runtime allow/block lists (domains are lowercased and blanks dropped).
func (r *RuntimePeerLists) Set(allow, block []string) {
	a, b := cleanDomains(allow), cleanDomains(block)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allow, r.block = a, b
}

// Lists returns the current runtime allow and block lists.
func (r *RuntimePeerLists) Lists() (allow, block []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.allow, r.block
}

func containsDomain(list []string, domain string) bool {
	for _, d := range list {
		if d == domain {
			return true
		}
	}
	return false
}

// IsAllowedPeer applies the allow/block lists - the static env lists unioned with the runtime
// (admin-managed) lists. Blocklist wins; a non-empty allowlist (from either source) switches
// this instance into allowlist-only mode.
func (f FederationConfig) IsAllowedPeer(domain string) bool {
	if !f.Enabled || domain == "" || domain == f.Domain {
		return false
	}
	var rtAllow, rtBlock []string
	if f.Runtime != nil {
		rtAllow, rtBlock = f.Runtime.Lists()
	}
	if containsDomain(f.Blocklist, domain) || containsDomain(rtBlock, domain) {
		return false
	}
	if len(f.Allowlist) == 0 && len(rtAllow) == 0 {
		return true
	}
	return containsDomain(f.Allowlist, domain) || containsDomain(rtAllow, domain)
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
	// WebURL is the browser-facing URL of the web client (e.g. https://web.strafe.chat),
	// advertised on GET / so clients/tools can discover it. Optional.
	WebURL string
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
	Port   string // WebSocket server port (e.g. 4001)
	Region string // instance region for multi-region (e.g. "us-east", "eu-west")
	// PublicURL is the browser-facing gateway URL advertised on GET / (e.g.
	// wss://chat.example.com/gateway/events). Optional; when empty the / route falls back
	// to Federation.GatewayURL. The gateway itself always runs - this only affects what is
	// advertised to clients.
	PublicURL       string
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
	InviteOnly bool
	// InstanceAdmins may mint and revoke instance invites regardless of what the database
	// says. The account that registers first is made an admin automatically, so this is
	// the escape hatch for an operator who has lost that account - never the usual route.
	InstanceAdmins []int64
	// PMPolicy decides who may START a direct message (or pull someone into a group DM):
	// "shared" (default) only friends and people who share a space with you; "open" anyone,
	// the pre-beta behaviour. Existing conversations are never re-checked, and blocks are
	// enforced in both modes.
	PMPolicy string
	// BlockDisposableEmail refuses registration with an address at a known throwaway-mail
	// provider (auth/disposable_domains.txt), on by default; BlockedEmailDomains is the
	// operator's own additions to that list (EMAIL_BLOCKED_DOMAINS, comma separated; a
	// domain covers its subdomains).
	BlockDisposableEmail bool
	BlockedEmailDomains  []string
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

// TwoFactorConfig is account 2FA: TOTP authenticator codes (always available - the secret
// never leaves this server, so there is nothing to configure) and WebAuthn passkeys/security
// keys (available only once the instance knows its own browser-facing origin, because a
// WebAuthn ceremony is bound to it by the browser itself).
type TwoFactorConfig struct {
	// TOTPEncryptionKey wraps every TOTP secret at rest (32 bytes, hex). Unlike a password
	// hash this must be reversible - the server computes the code to compare - so it is
	// AES-256-GCM'd under this key rather than hashed. Required unconditionally: a key that
	// could vary across restarts would make every enrolled account's codes stop working
	// (or, worse, silently re-derive the wrong ones) the next time the process starts.
	TOTPEncryptionKey string
	// WebAuthnEnabled mirrors WebAuthnRPID != "": derived from App.WebURL, since a passkey
	// is bound to the exact origin it was created on and cannot be configured separately
	// from "what origin does this instance's web client run on".
	WebAuthnEnabled bool
	WebAuthnRPID    string
	WebAuthnRPName  string
	WebAuthnOrigins []string
}

// MailConfig is outbound email: the verification link a new account gets and the
// password-reset link anyone can ask for. Off unless SMTP_HOST is set; everything else has
// a default. The compose deployment points this at its bundled send-only relay
// (deploy/mail, a maddy instance on the private network); any other SMTP server - an
// existing mail server, a provider's submission endpoint - works the same way.
type MailConfig struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	// TLS is how the connection to Host is protected: "starttls" (the default; STARTTLS
	// is required and a server that will not upgrade is refused), "tls" (implicit TLS
	// from the first byte, usually port 465) or "none" (plaintext - only for a relay on
	// the same private network, which is what the bundled one is).
	TLS string
	// From is the sender address; its domain is what SPF/DKIM/DMARC are checked against,
	// so it must be a domain whose DNS you control. Defaults to noreply@<FEDERATION_DOMAIN>
	// (or the WEB_URL host).
	From     string
	FromName string
	// VerificationRequired gates sign-in on a verified address: a new account is sent a
	// link at registration and cannot log in until it is clicked. Accounts that predate
	// the switch are asked to verify once at their next sign-in. Needs Enabled.
	VerificationRequired bool
	// LinkBase is the web client's URL (App.WebURL); every link in an email is under it.
	LinkBase string
	// InstanceName is how the instance introduces itself in subjects and bodies: the
	// federation domain, else the WEB_URL host, else "StrafeChat".
	InstanceName string
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
			WebURL:        strings.TrimRight(strings.TrimSpace(getEnvString("WEB_URL", "")), "/"),
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
			PublicURL:       strings.TrimRight(strings.TrimSpace(getEnvString("STARGATE_PUBLIC_URL", "")), "/"),
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
			Runtime:        NewRuntimePeerLists(),
			StaticPeers:    parseStaticPeers(getEnvString("FEDERATION_STATIC_PEERS", "")),
			AllowInsecure:  getEnvBool("FEDERATION_ALLOW_INSECURE", false),
			MemberPage:     getEnvInt("FEDERATION_MEMBER_PAGE", 1000),
		},
		Flags: FeatureFlags{
			Captcha:        getEnvBool("CAPTCHA", false),
			InviteOnly:     getEnvBool("INVITE_ONLY", false),
			InstanceAdmins: parseIDList(getEnvArray("INSTANCE_ADMINS", nil)),
			PMPolicy:       strings.ToLower(strings.TrimSpace(getEnvString("PM_POLICY", "shared"))),

			BlockDisposableEmail: getEnvBool("EMAIL_BLOCK_DISPOSABLE", true),
			BlockedEmailDomains:  cleanDomains(getEnvArray("EMAIL_BLOCKED_DOMAINS", nil)),
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
	cfg.TwoFactor = loadTwoFactorConfig(getEnvString("TOTP_ENCRYPTION_KEY", ""), cfg.App.WebURL)
	cfg.Mail = loadMailConfig(cfg.App.WebURL, cfg.Federation.Domain)

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
	if cfg.Flags.PMPolicy == "" {
		cfg.Flags.PMPolicy = "shared"
	}
	switch cfg.Flags.PMPolicy {
	case "shared", "open":
	default:
		return errors.New("PM_POLICY must be \"shared\" (friends and shared spaces only) or \"open\"")
	}
	if cfg.Flags.Captcha {
		if err := validateCaptcha(cfg.Captcha); err != nil {
			return err
		}
	}
	if err := validateVoice(cfg.Voice); err != nil {
		return err
	}
	if err := validateTwoFactor(cfg.TwoFactor); err != nil {
		return err
	}
	if err := validateMail(cfg.Mail); err != nil {
		return err
	}
	return nil
}

// loadTwoFactorConfig derives the WebAuthn relying-party identity from App.WebURL: the RP
// ID is that URL's host (what a passkey is bound to) and the sole allowed origin is the URL
// itself. WebAuthn stays off (WebAuthnEnabled false) until WEB_URL is set - there is no
// separate WEBAUTHN_* variable to fill in, because a passkey registered under the wrong
// origin simply fails in the browser, so the one place this can come from is the same
// browser-facing URL the rest of the app already advertises.
func loadTwoFactorConfig(totpKey, webURL string) TwoFactorConfig {
	tf := TwoFactorConfig{TOTPEncryptionKey: strings.TrimSpace(totpKey)}
	if webURL == "" {
		return tf
	}
	u, err := neturl.Parse(webURL)
	if err != nil || u.Hostname() == "" {
		return tf
	}
	tf.WebAuthnEnabled = true
	tf.WebAuthnRPID = u.Hostname()
	tf.WebAuthnRPName = "StrafeChat"
	tf.WebAuthnOrigins = []string{strings.TrimRight(webURL, "/")}
	return tf
}

func validateTwoFactor(tf TwoFactorConfig) error {
	if len(tf.TOTPEncryptionKey) != 64 {
		return errors.New("TOTP_ENCRYPTION_KEY is required and must be 64 hex characters / 32 bytes (openssl rand -hex 32)")
	}
	if _, err := hex.DecodeString(tf.TOTPEncryptionKey); err != nil {
		return errors.New("TOTP_ENCRYPTION_KEY must be hex-encoded (openssl rand -hex 32)")
	}
	return nil
}

// loadMailConfig reads SMTP_* / MAIL_* / EMAIL_VERIFICATION. Enabled is simply "SMTP_HOST
// is set": there is no separate on/off flag to fall out of step with the host, the same
// shape voice uses with LIVEKIT_URL. The sender address and the instance's display name
// fall back to the identity the instance already has (its federation domain, its web URL)
// so a compose deployment needs to fill in nothing beyond the host.
func loadMailConfig(webURL, federationDomain string) MailConfig {
	env := func(k string) string { return strings.TrimSpace(getEnvString(k, "")) }
	m := MailConfig{
		Host:                 env("SMTP_HOST"),
		Port:                 getEnvInt("SMTP_PORT", 0),
		Username:             env("SMTP_USERNAME"),
		Password:             getEnvString("SMTP_PASSWORD", ""),
		TLS:                  strings.ToLower(env("SMTP_TLS")),
		From:                 env("MAIL_FROM"),
		FromName:             env("MAIL_FROM_NAME"),
		VerificationRequired: getEnvBool("EMAIL_VERIFICATION", false),
		LinkBase:             strings.TrimRight(webURL, "/"),
	}
	m.Enabled = m.Host != ""
	if m.TLS == "" {
		m.TLS = "starttls"
	}
	if m.Port == 0 {
		if m.TLS == "tls" {
			m.Port = 465
		} else {
			m.Port = 587
		}
	}
	if m.FromName == "" {
		m.FromName = "StrafeChat"
	}
	domain := federationDomain
	if domain == "" {
		if u, err := neturl.Parse(webURL); err == nil {
			domain = u.Hostname()
		}
	}
	m.InstanceName = domain
	if m.InstanceName == "" {
		m.InstanceName = "StrafeChat"
	}
	if m.From == "" && domain != "" {
		m.From = "noreply@" + domain
	}
	return m
}

// validateMail refuses the configurations that would fail at the first email rather than
// at boot: a verification requirement with nothing to send through, a sender address that
// is not one, links with nowhere to point. Reaching the SMTP server itself is checked
// per send - a relay being down must not keep the whole API from starting.
func validateMail(m MailConfig) error {
	if !m.Enabled {
		if m.VerificationRequired {
			return errors.New("EMAIL_VERIFICATION=true requires SMTP_HOST (an SMTP server to send the verification links through)")
		}
		return nil
	}
	switch m.TLS {
	case "starttls", "tls", "none":
	default:
		return errors.New("SMTP_TLS must be starttls, tls or none")
	}
	if m.Port < 1 || m.Port > 65535 {
		return errors.New("SMTP_PORT must be 1-65535")
	}
	if (m.Username == "") != (m.Password == "") {
		return errors.New("SMTP_USERNAME and SMTP_PASSWORD must be set together (or neither, for a relay that trusts the network)")
	}
	if m.From == "" {
		return errors.New("MAIL_FROM is required when SMTP_HOST is set and neither FEDERATION_DOMAIN nor WEB_URL gives a domain to derive it from")
	}
	if addr, err := netmail.ParseAddress(m.From); err != nil || addr.Address != m.From {
		return errors.New("MAIL_FROM must be a bare address like noreply@chat.example.com (the display name comes from MAIL_FROM_NAME)")
	}
	if m.LinkBase == "" {
		return errors.New("WEB_URL is required when SMTP_HOST is set - the links in every email point at the web client")
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
