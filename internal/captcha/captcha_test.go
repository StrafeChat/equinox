package captcha

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestTurnstile(t *testing.T, handler http.HandlerFunc) Verifier {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	v := NewTurnstile("secret").(*turnstile)
	v.url = srv.URL
	return v
}

func TestVerifyRejectsEmptyToken(t *testing.T) {
	// No request should be made at all for an empty token.
	v := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("provider must not be called for an empty token")
	})
	if err := v.Verify(context.Background(), "   ", ""); err != ErrMissingToken {
		t.Fatalf("got %v, want ErrMissingToken", err)
	}
}

func TestVerifyPassesOnSuccess(t *testing.T) {
	v := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.FormValue("secret"); got != "secret" {
			t.Fatalf("secret = %q", got)
		}
		if got := r.FormValue("response"); got != "tok" {
			t.Fatalf("response = %q", got)
		}
		if got := r.FormValue("remoteip"); got != "203.0.113.5" {
			t.Fatalf("remoteip = %q", got)
		}
		w.Write([]byte(`{"success":true}`))
	})
	if err := v.Verify(context.Background(), "tok", "203.0.113.5"); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

func TestVerifyFailsOnRejection(t *testing.T) {
	v := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	})
	if err := v.Verify(context.Background(), "tok", ""); err != ErrFailed {
		t.Fatalf("got %v, want ErrFailed", err)
	}
}

// An outage must be distinguishable from a rejection, so the caller can choose to fail
// closed rather than quietly letting everyone through.
func TestVerifyReportsProviderOutageSeparately(t *testing.T) {
	v := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if err := v.Verify(context.Background(), "tok", ""); err != ErrUnavailable {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}

func TestVerifyReportsUnparseableBodyAsUnavailable(t *testing.T) {
	v := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	if err := v.Verify(context.Background(), "tok", ""); err != ErrUnavailable {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}

// remoteip is optional: omitting it must not send an empty field, which Turnstile treats
// as a malformed request.
func TestVerifyOmitsEmptyRemoteIP(t *testing.T) {
	v := newTestTurnstile(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if _, ok := r.Form["remoteip"]; ok {
			t.Fatal("remoteip should be absent when no IP is known")
		}
		w.Write([]byte(`{"success":true}`))
	})
	if err := v.Verify(context.Background(), "tok", ""); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}
