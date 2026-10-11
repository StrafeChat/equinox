package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/StrafeChat/equinox/internal/config"
)

func TestEndpointValidation(t *testing.T) {
	resolve := map[string][]string{
		"push.strafe.chat":     {"203.0.113.10"},
		"ntfy.sh":              {"198.51.100.4"},
		"internal.example":     {"10.0.0.3"},
		"rebind.example":       {"203.0.113.11", "127.0.0.1"},
		"sub.push.strafe.chat": {"203.0.113.12"},
	}
	checker := func(insecure bool, allowed ...string) *endpointChecker {
		c := newEndpointChecker(config.PushConfig{AllowInsecure: insecure, AllowedHosts: allowed})
		c.lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
			ips, ok := resolve[host]
			if !ok {
				return nil, errors.New("no such host")
			}
			out := make([]net.IPAddr, len(ips))
			for i, ip := range ips {
				out[i] = net.IPAddr{IP: net.ParseIP(ip)}
			}
			return out, nil
		}
		return c
	}
	cases := []struct {
		name     string
		endpoint string
		c        *endpointChecker
		wantErr  string
	}{
		{"https public", "https://push.strafe.chat/v1/push/abc", checker(false), ""},
		{"http refused", "http://push.strafe.chat/v1/push/abc", checker(false), "https"},
		{"http in dev", "http://127.0.0.1:4020/v1/push/abc", checker(true), ""},
		{"relative", "/v1/push/abc", checker(false), "absolute"},
		{"other scheme", "ftp://push.strafe.chat/x", checker(false), "https"},
		{"credentials", "https://user:pw@push.strafe.chat/x", checker(false), "credentials"},
		{"loopback literal", "https://127.0.0.1/x", checker(false), "public"},
		{"private literal", "https://[fd00::1]/x", checker(false), "public"},
		{"localhost name", "https://localhost/x", checker(false), "public"},
		{"resolves private", "https://internal.example/x", checker(false), "public"},
		{"one private answer", "https://rebind.example/x", checker(false), "public"},
		{"does not resolve", "https://nowhere.example/x", checker(false), "resolve"},
		{"too long", "https://push.strafe.chat/" + strings.Repeat("a", 2100), checker(false), "too long"},
		{"empty", "", checker(false), "required"},
		{"allowlist hit", "https://push.strafe.chat/x", checker(false, "push.strafe.chat", "ntfy.sh"), ""},
		{"allowlist subdomain", "https://sub.push.strafe.chat/x", checker(false, "push.strafe.chat"), ""},
		{"allowlist miss", "https://ntfy.sh/x", checker(false, "push.strafe.chat"), "does not send"},
		{"allowlist is not a substring match", "https://evilpush.strafe.chat.example/x", checker(false, "push.strafe.chat"), "does not send"},
	}
	for _, c := range cases {
		_, err := c.c.check(context.Background(), c.endpoint)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want one mentioning %q", c.name, err, c.wantErr)
		}
		var ve *ValidationError
		if err != nil && !errors.As(err, &ve) {
			t.Errorf("%s: %T is not a ValidationError", c.name, err)
		}
	}
}

func TestValidateSubscribe(t *testing.T) {
	c := newEndpointChecker(config.PushConfig{AllowInsecure: true})
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	good := func() *SubscribeInput {
		in := &SubscribeInput{Endpoint: "http://127.0.0.1:4020/v1/push/x", Kind: KindUnifiedPush}
		in.Keys.P256dh = b64(priv.PublicKey().Bytes())
		in.Keys.Auth = b64(make([]byte, 16))
		return in
	}
	prefs, err := c.validateSubscribe(context.Background(), good())
	if err != nil {
		t.Fatal(err)
	}
	if prefs != DefaultPrefs() {
		t.Errorf("missing prefs = %+v, want the defaults", prefs)
	}
	bad := map[string]func(*SubscribeInput){
		"kind":       func(in *SubscribeInput) { in.Kind = "sms" },
		"no kind":    func(in *SubscribeInput) { in.Kind = "" },
		"bad p256dh": func(in *SubscribeInput) { in.Keys.P256dh = b64(make([]byte, 65)) },
		"short auth": func(in *SubscribeInput) { in.Keys.Auth = b64(make([]byte, 12)) },
		"pm mode":    func(in *SubscribeInput) { in.Prefs.PM = strPtr("mentions") },
		"space mode": func(in *SubscribeInput) { in.Prefs.Space = strPtr("loud") },
	}
	for name, mod := range bad {
		in := good()
		mod(in)
		if _, err := c.validateSubscribe(context.Background(), in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEndpointHostNeverLeaksPath(t *testing.T) {
	if h := endpointHost("https://push.strafe.chat:8443/v1/push/SECRET"); h != "push.strafe.chat:8443" {
		t.Errorf("host = %q", h)
	}
}
