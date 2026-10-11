// Package pushgateway is the Apple/Google relay for Strafe's push notifications: a small
// stateless HTTP service that speaks standard Web Push (RFC 8030 + RFC 8291 bodies +
// RFC 8292 VAPID) on one side and FCM HTTP v1 / APNs on the other.
//
// It exists because a phone can only be woken through its platform's push service, and
// only the publisher of an app build holds that build's APNs key and Firebase
// credentials. Instances never need either: a device registers its platform token here,
// gets back an opaque Web Push endpoint, and hands that endpoint to its instance like any
// other push subscription. The gateway keeps nothing - the platform token travels inside
// the endpoint itself, sealed with AES-256-GCM - and sees only ciphertext: the body is
// encrypted end to end from the instance to the device and is forwarded byte for byte.
package pushgateway

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Config is the gateway's whole configuration. It is deliberately separate from the API's
// internal/config: the gateway is its own binary, usually run by whoever publishes an app
// build rather than by an instance operator, and needs none of an instance's settings.
type Config struct {
	// Listen is the address the HTTP server binds (PUSHGW_LISTEN, default ":4020").
	Listen string
	// PublicURL is the externally visible base URL, without a trailing slash. Endpoints
	// are built from it, and a VAPID token's audience must equal its origin.
	PublicURL *url.URL
	// SealKey seals new endpoints and opens existing ones; OldSealKeys only open, so a
	// rotated key keeps every endpoint issued under the previous one working.
	SealKey     []byte
	OldSealKeys [][]byte
	// FCM and APNs are nil when that platform is not configured.
	FCM  *FCMCredentials
	APNs *APNsConfig
	// TrustedProxies are the reverse proxies whose X-Forwarded-For is believed: IPs,
	// CIDRs, or the words "loopback", "linklocal", "private" (as the API's TRUSTED_PROXIES).
	TrustedProxies []string
	// Debug enables the development-only "debug" platform (see debug.go).
	Debug bool
}

// FCMCredentials is the part of a Firebase service-account JSON the gateway uses.
type FCMCredentials struct {
	ProjectID    string
	ClientEmail  string
	PrivateKeyID string
	TokenURI     string
	PrivateKey   *rsa.PrivateKey
}

// APNsConfig holds a token-based (.p8) APNs provider identity.
type APNsConfig struct {
	Key    *ecdsa.PrivateKey
	KeyID  string
	TeamID string
	// Topic is the app's bundle id; every push names it in apns-topic.
	Topic string
}

// LoadConfig reads the configuration from the environment through getenv (os.Getenv in
// production; a map in tests), reading the credential files it points at.
func LoadConfig(getenv func(string) string) (*Config, error) {
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg := &Config{Listen: env("PUSHGW_LISTEN")}
	if cfg.Listen == "" {
		cfg.Listen = ":4020"
	}

	pub, err := parsePublicURL(env("PUSHGW_PUBLIC_URL"))
	if err != nil {
		return nil, err
	}
	cfg.PublicURL = pub

	if cfg.SealKey, err = decodeSealKey(env("PUSHGW_SEAL_KEY")); err != nil {
		return nil, fmt.Errorf("PUSHGW_SEAL_KEY: %w", err)
	}
	for _, raw := range splitList(env("PUSHGW_SEAL_KEYS_OLD")) {
		k, err := decodeSealKey(raw)
		if err != nil {
			return nil, fmt.Errorf("PUSHGW_SEAL_KEYS_OLD: %w", err)
		}
		cfg.OldSealKeys = append(cfg.OldSealKeys, k)
	}

	if path := env("PUSHGW_FCM_CREDENTIALS"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("PUSHGW_FCM_CREDENTIALS: %w", err)
		}
		if cfg.FCM, err = ParseFCMCredentials(data); err != nil {
			return nil, fmt.Errorf("PUSHGW_FCM_CREDENTIALS: %w", err)
		}
	}

	if cfg.APNs, err = loadAPNs(env); err != nil {
		return nil, err
	}

	cfg.TrustedProxies = splitList(env("PUSHGW_TRUSTED_PROXIES"))
	if _, err := newProxyList(cfg.TrustedProxies); err != nil {
		return nil, fmt.Errorf("PUSHGW_TRUSTED_PROXIES: %w", err)
	}

	switch strings.ToLower(env("PUSHGW_DEBUG")) {
	case "", "0", "false", "no", "off":
	case "1", "true", "yes", "on":
		cfg.Debug = true
	default:
		return nil, errors.New("PUSHGW_DEBUG: expected true or false")
	}
	// The debug platform hands every push it receives to anyone who asks, so it must never
	// be switched on for a gateway that real devices can reach: not under a public name or
	// address (over http as much as https), and not beside real Apple/Google credentials,
	// which only a gateway serving real devices has.
	if cfg.Debug && !isDevelopmentHost(pub.Hostname()) {
		return nil, errors.New("PUSHGW_DEBUG is for development only and refuses to run with a public PUSHGW_PUBLIC_URL (use localhost, a private address, or a .local/.test/.internal name)")
	}
	if cfg.Debug && (cfg.FCM != nil || cfg.APNs != nil) {
		return nil, errors.New("PUSHGW_DEBUG is for development only and refuses to run with FCM or APNs credentials configured")
	}
	return cfg, nil
}

func parsePublicURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("PUSHGW_PUBLIC_URL is required (e.g. https://push.example.com)")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("PUSHGW_PUBLIC_URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// decodeSealKey accepts the 32 bytes in standard or URL-safe base64, padded or not, so
// the output of `openssl rand -base64 32` works as is.
func decodeSealKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("required: 32 random bytes in base64 (openssl rand -base64 32)")
	}
	trimmed := strings.TrimRight(raw, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(trimmed); err == nil {
			if len(k) != 32 {
				return nil, fmt.Errorf("decodes to %d bytes, want 32", len(k))
			}
			return k, nil
		}
	}
	return nil, errors.New("not valid base64")
}

// ParseFCMCredentials parses a Firebase service-account key file (Firebase console ->
// Project settings -> Service accounts -> Generate new private key).
func ParseFCMCredentials(data []byte) (*FCMCredentials, error) {
	var sa struct {
		Type         string `json:"type"`
		ProjectID    string `json:"project_id"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
		ClientEmail  string `json:"client_email"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, fmt.Errorf("not a service-account JSON: %w", err)
	}
	if sa.Type != "" && sa.Type != "service_account" {
		return nil, fmt.Errorf("credential type %q is not a service account", sa.Type)
	}
	if sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("service-account JSON lacks project_id, client_email or private_key")
	}
	key, err := parsePrivateKeyPEM([]byte(sa.PrivateKey))
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("service-account private_key is not an RSA key")
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	if u, err := url.Parse(tokenURI); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("service-account token_uri is not an absolute URL")
	}
	return &FCMCredentials{
		ProjectID:    sa.ProjectID,
		ClientEmail:  sa.ClientEmail,
		PrivateKeyID: sa.PrivateKeyID,
		TokenURI:     tokenURI,
		PrivateKey:   rsaKey,
	}, nil
}

func loadAPNs(env func(string) string) (*APNsConfig, error) {
	path, keyID, teamID, topic := env("PUSHGW_APNS_KEY"), env("PUSHGW_APNS_KEY_ID"), env("PUSHGW_APNS_TEAM_ID"), env("PUSHGW_APNS_TOPIC")
	if path == "" && keyID == "" && teamID == "" && topic == "" {
		return nil, nil
	}
	// All four or none: a half-configured APNs would fail every push at Apple, and a
	// wrong topic would get every device token refused (DeviceTokenNotForTopic).
	if path == "" || keyID == "" || teamID == "" || topic == "" {
		return nil, errors.New("APNs needs all of PUSHGW_APNS_KEY, PUSHGW_APNS_KEY_ID, PUSHGW_APNS_TEAM_ID and PUSHGW_APNS_TOPIC")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("PUSHGW_APNS_KEY: %w", err)
	}
	key, err := ParseAPNsKey(data)
	if err != nil {
		return nil, fmt.Errorf("PUSHGW_APNS_KEY: %w", err)
	}
	return &APNsConfig{Key: key, KeyID: keyID, TeamID: teamID, Topic: topic}, nil
}

// ParseAPNsKey parses the .p8 file Apple issues for token-based APNs authentication: a
// PKCS#8 PEM holding a P-256 key.
func ParseAPNsKey(data []byte) (*ecdsa.PrivateKey, error) {
	key, err := parsePrivateKeyPEM(data)
	if err != nil {
		return nil, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve.Params().Name != "P-256" {
		return nil, errors.New("not a P-256 EC private key")
	}
	return ec, nil
}

func parsePrivateKeyPEM(data []byte) (any, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM private key found")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("unsupported private key encoding")
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// isDevelopmentHost reports whether a PUBLIC_URL host can only be a development setup:
// localhost, a loopback/private/link-local address (an emulator reaching the host as
// 10.0.2.2, a LAN machine), or a name under a suffix that never resolves publicly.
func isDevelopmentHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".test", ".internal", ".lan", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}
