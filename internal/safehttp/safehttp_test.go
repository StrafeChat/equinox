package safehttp

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPublicIP(t *testing.T) {
	cases := map[string]bool{
		"1.1.1.1":          true,
		"2606:4700::1111":  true,
		"127.0.0.1":        false,
		"::1":              false,
		"10.0.0.5":         false,
		"172.16.3.4":       false,
		"192.168.1.1":      false,
		"169.254.169.254":  false, // cloud metadata
		"100.64.0.1":       false,
		"0.0.0.0":          false,
		"0.1.2.3":          false,
		"198.18.0.1":       false,
		"192.0.0.8":        false,
		"255.255.255.255":  false,
		"fd00::1":          false,
		"fe80::1":          false,
		"::ffff:127.0.0.1": false,
		"::ffff:8.8.8.8":   true,
		"ff02::1":          false,
		"224.0.0.1":        false,
		// IPv6 that leads to IPv4, judged by where it leads.
		"::127.0.0.1":            false, // IPv4-compatible
		"::8.8.8.8":              true,
		"64:ff9b::a00:1":         false, // NAT64 of 10.0.0.1
		"64:ff9b::7f00:1":        false, // NAT64 of 127.0.0.1
		"64:ff9b::808:808":       true,  // NAT64 of 8.8.8.8 (DNS64 on an IPv6-only host)
		"64:ff9b:1::1":           false,
		"2002:a9fe:a9fe::1":      false, // 6to4 of 169.254.169.254
		"2002:c0a8:101::1":       false, // 6to4 of 192.168.1.1
		"2002:808:808::1":        true,
		"2001:0:4136:e378::1":    false, // Teredo
		"100::1":                 false,
		"fec0::1":                false,
		"::ffff:169.254.169.254": false,
	}
	for addr, want := range cases {
		if got := IsPublicIP(net.ParseIP(addr)); got != want {
			t.Errorf("IsPublicIP(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestDialControl(t *testing.T) {
	if err := DialControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("public address refused: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "10.1.2.3:80", "[64:ff9b::a9fe:a9fe]:80", "[::ffff:10.0.0.1]:443"} {
		if err := DialControl("tcp", addr, nil); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: err = %v, want ErrBlockedAddress", addr, err)
		}
	}
	if err := DialControl("udp", "8.8.8.8:53", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("udp allowed: %v", err)
	}
}

// The client refuses this host's own network unless told it is a development setup, and a
// redirect neither leaks the previous URL nor runs past the limit.
func TestNewClient(t *testing.T) {
	var referers []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referers = append(referers, r.Header.Get("Referer"))
		switch r.URL.Path {
		case "/hop":
			http.Redirect(w, r, srv.URL+"/end", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	guarded := NewClient(ClientOptions{Timeout: 2 * time.Second, MaxRedirects: 2})
	if _, err := guarded.Get(srv.URL + "/end"); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("loopback reached: %v", err)
	}

	dev := NewClient(ClientOptions{Timeout: 2 * time.Second, MaxRedirects: 2, AllowPrivate: true})
	resp, err := dev.Get(srv.URL + "/hop")
	if err != nil {
		t.Fatalf("dev client: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(referers) != 2 || referers[1] != "" {
		t.Fatalf("status %d, referers %q", resp.StatusCode, referers)
	}
	if _, err := dev.Get(srv.URL + "/loop"); err == nil || !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("redirect loop: %v", err)
	}
}
