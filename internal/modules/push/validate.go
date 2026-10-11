package push

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/safehttp"
)

const maxEndpointLen = 2048

// ValidationError is a subscription the instance refuses, with a reason fit to show the
// person setting it up (the handler answers 400 with it).
type ValidationError struct{ Reason string }

func (e *ValidationError) Error() string { return e.Reason }

func invalid(reason string) error { return &ValidationError{Reason: reason} }

// endpointChecker decides which push endpoints this instance will send to. The same rules
// run at subscribe time (a clear 400) and again at send time (the dial-time guard in
// safehttp is the one that cannot be raced, but a policy change - a host dropped from
// PUSH_ALLOWED_HOSTS - must also stop sends to subscriptions made before it).
type endpointChecker struct {
	allowedHosts  []string
	allowInsecure bool
	// lookup resolves a host name at subscribe time; replaceable in tests.
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
}

func newEndpointChecker(cfg config.PushConfig) *endpointChecker {
	return &endpointChecker{
		allowedHosts:  cfg.AllowedHosts,
		allowInsecure: cfg.AllowInsecure,
		lookup:        net.DefaultResolver.LookupIPAddr,
	}
}

// parse checks the shape and policy of an endpoint without touching the network.
func (c *endpointChecker) parse(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, invalid("endpoint is required")
	}
	if len(raw) > maxEndpointLen {
		return nil, invalid("endpoint is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return nil, invalid("endpoint must be an absolute URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !c.allowInsecure {
			return nil, invalid("endpoint must be https")
		}
	default:
		return nil, invalid("endpoint must be https")
	}
	if u.User != nil {
		return nil, invalid("endpoint must not carry credentials")
	}
	if u.Fragment != "" {
		return nil, invalid("endpoint must not have a fragment")
	}
	host := strings.ToLower(u.Hostname())
	if !c.hostAllowed(host) {
		return nil, invalid("this instance does not send push notifications to " + host)
	}
	if ip := net.ParseIP(host); ip != nil && !c.allowInsecure && !safehttp.IsPublicIP(ip) {
		return nil, invalid("endpoint must be a public address")
	}
	if !c.allowInsecure && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return nil, invalid("endpoint must be a public address")
	}
	return u, nil
}

// hostAllowed applies PUSH_ALLOWED_HOSTS: an exact match or a subdomain of an entry.
func (c *endpointChecker) hostAllowed(host string) bool {
	if len(c.allowedHosts) == 0 {
		return true
	}
	for _, h := range c.allowedHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// check is parse plus, outside insecure mode, a resolution of the host name: an endpoint
// whose name points into a private network is refused up front with a reason, instead of
// being accepted and then failing every delivery at the dial-time guard.
func (c *endpointChecker) check(ctx context.Context, raw string) (*url.URL, error) {
	u, err := c.parse(raw)
	if err != nil {
		return nil, err
	}
	if c.allowInsecure || net.ParseIP(u.Hostname()) != nil {
		return u, nil
	}
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := c.lookup(lctx, u.Hostname())
	if err != nil || len(addrs) == 0 {
		return nil, invalid("endpoint host does not resolve")
	}
	for _, a := range addrs {
		if !safehttp.IsPublicIP(a.IP) {
			return nil, invalid("endpoint must be a public address")
		}
	}
	return u, nil
}

// validateSubscribe checks a whole subscription body and returns the normalised
// preferences.
func (c *endpointChecker) validateSubscribe(ctx context.Context, in *SubscribeInput) (Prefs, error) {
	if _, err := c.check(ctx, strings.TrimSpace(in.Endpoint)); err != nil {
		return Prefs{}, err
	}
	if _, _, err := parseReceiverKeys(in.Keys.P256dh, in.Keys.Auth); err != nil {
		return Prefs{}, invalid("keys.p256dh must be an uncompressed P-256 public key and keys.auth 16 bytes, both base64url")
	}
	switch in.Kind {
	case KindFCM, KindAPNs, KindUnifiedPush, KindWeb:
	default:
		return Prefs{}, invalid("kind must be fcm, apns, unifiedpush or web")
	}
	return applyPrefs(DefaultPrefs(), in.Prefs)
}

// applyPrefs overlays a partial preference set on base, refusing unknown values.
func applyPrefs(base Prefs, p PrefsPatch) (Prefs, error) {
	if p.Preview != nil {
		base.Preview = *p.Preview
	}
	if p.Calls != nil {
		base.Calls = *p.Calls
	}
	if p.PM != nil {
		if !validMode(*p.PM, false) {
			return Prefs{}, invalid("prefs.pm must be all or none")
		}
		base.PM = *p.PM
	}
	if p.Space != nil {
		if !validMode(*p.Space, true) {
			return Prefs{}, invalid("prefs.space must be all, mentions or none")
		}
		base.Space = *p.Space
	}
	return base, nil
}

// endpointHost is the part of an endpoint that may be logged or shown: the host. The path
// of a push endpoint is a capability (whoever knows it can push to the device, with a
// VAPID key of their own), so it never goes in a log line or an API response.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}
