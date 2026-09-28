package captcha

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestCap points a Cap verifier at a stub standing in for the Cap container.
func newTestCap(t *testing.T, handler http.HandlerFunc) Verifier {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewCap(srv.URL, "site-key", "secret-key")
}

func TestCapRejectsEmptyToken(t *testing.T) {
	v := newTestCap(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("Cap must not be called for an empty token")
	})
	if err := v.Verify(context.Background(), "  ", ""); err != ErrMissingToken {
		t.Fatalf("got %v, want ErrMissingToken", err)
	}
}

func TestCapPostsSiteverifyAndPasses(t *testing.T) {
	v := newTestCap(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		// The site key belongs in the path: one Cap instance serves several sites.
		if r.URL.Path != "/site-key/siteverify" {
			t.Fatalf("path = %q, want /site-key/siteverify", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content-type = %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		var in map[string]string
		if err := json.Unmarshal(body, &in); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if in["secret"] != "secret-key" {
			t.Fatalf("secret = %q", in["secret"])
		}
		if in["response"] != "tok" {
			t.Fatalf("response = %q", in["response"])
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	if err := v.Verify(context.Background(), "tok", "203.0.113.5"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestCapFailsWhenNotSuccessful(t *testing.T) {
	v := newTestCap(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false}`))
	})
	if err := v.Verify(context.Background(), "tok", ""); err != ErrFailed {
		t.Fatalf("got %v, want ErrFailed", err)
	}
}

// A stopped container or a wrong secret is an outage, not a visitor failing a puzzle -
// the caller has to be able to tell those apart.
func TestCapNon200IsUnavailable(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusInternalServerError, http.StatusBadGateway} {
		v := newTestCap(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		})
		if err := v.Verify(context.Background(), "tok", ""); err != ErrUnavailable {
			t.Fatalf("status %d: got %v, want ErrUnavailable", code, err)
		}
	}
}

func TestCapGarbledBodyIsUnavailable(t *testing.T) {
	v := newTestCap(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	if err := v.Verify(context.Background(), "tok", ""); err != ErrUnavailable {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}

// A trailing slash on the URL and a site key written as a path segment are both things an
// operator will copy out of the Cap dashboard; neither may produce a double slash.
func TestCapNormalisesURLAndSiteKey(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)
	v := NewCap(srv.URL+"/", "/site-key/", "secret-key")
	if err := v.Verify(context.Background(), "tok", ""); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotPath != "/site-key/siteverify" {
		t.Fatalf("path = %q, want /site-key/siteverify", gotPath)
	}
}

// Cap answers a token it will not accept with 400 {"success":false}. That is Cap
// answering, so it must read as a failed challenge - otherwise a bot spraying junk
// tokens looks exactly like the container being down.
func TestCapRejectedTokenIs400AndFails(t *testing.T) {
	v := newTestCap(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"error":"Missing required parameters"}`))
	})
	if err := v.Verify(context.Background(), "bogus", ""); err != ErrFailed {
		t.Fatalf("got %v, want ErrFailed", err)
	}
}

// Cap answers 404 {"success":false,"error":"Invalid site key or secret"} for a token it
// does not know - which is what an expired or already-spent one looks like, and is by far
// the common case. It must read as a failed challenge so the form re-arms and says so.
// (The same 404 is what wrong credentials produce; Cap gives no way to tell them apart,
// so the provider logs a one-time warning instead - see Verify.)
func TestCapUnknownTokenIs404AndFails(t *testing.T) {
	v := newTestCap(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"error":"Invalid site key or secret"}`))
	})
	if err := v.Verify(context.Background(), "spent-token", ""); err != ErrFailed {
		t.Fatalf("got %v, want ErrFailed", err)
	}
}
