package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// fakeEmailSvc answers the email-change methods; anything else on Service is nil and would
// panic, which is the point - the handlers must not reach for it.
type fakeEmailSvc struct {
	Service
	applied bool
	got     EmailChangeInput
	calls   int
}

var testEmailState = &EmailState{
	Email:    "jane@example.com",
	Verified: true,
	Pending:  &PendingEmailChange{Email: "jane@new.example", ExpiresAt: time.Date(2026, 11, 15, 15, 4, 5, 0, time.UTC)},
}

func (f *fakeEmailSvc) GetEmailState(context.Context, *User) (*EmailState, error) {
	f.calls++
	return &EmailState{Email: "jane@example.com", Verified: false}, nil
}
func (f *fakeEmailSvc) RequestEmailChange(_ context.Context, _ *User, in EmailChangeInput) (*EmailState, bool, error) {
	f.calls++
	f.got = in
	if f.applied {
		return &EmailState{Email: in.Email}, true, nil
	}
	return testEmailState, false, nil
}
func (f *fakeEmailSvc) ResendEmailChange(context.Context, *User) (*EmailState, error) {
	f.calls++
	return testEmailState, nil
}
func (f *fakeEmailSvc) CancelEmailChange(context.Context, *User) (*EmailState, error) {
	f.calls++
	return &EmailState{Email: "jane@example.com"}, nil
}
func (f *fakeEmailSvc) ConfirmEmailChange(_ context.Context, _ *User, token string) (*EmailState, error) {
	f.calls++
	if token == "bad" {
		return nil, ErrEmailChangeTokenInvalid
	}
	return &EmailState{Email: "jane@new.example", Verified: true}, nil
}

type emailCall struct {
	method, path, body string
	user               *User
	session            bool
}

func runEmailCall(t *testing.T, svc *fakeEmailSvc, c emailCall) (int, map[string]any, http.Header) {
	t.Helper()
	h := NewHandler(svc)
	app := fiber.New()
	locals := func(ctx fiber.Ctx) error {
		if c.user != nil {
			ctx.Locals(LocalsKeyUser, c.user)
		}
		if c.session {
			ctx.Locals(LocalsKeySession, &Session{UserID: c.user.ID, SessionID: 1})
		}
		return ctx.Next()
	}
	app.Get("/users/@me/email", locals, h.GetEmail)
	app.Post("/users/@me/email", locals, h.RequestEmailChange)
	app.Post("/users/@me/email/confirm", locals, h.ConfirmEmailChange)
	app.Post("/users/@me/email/pending/resend", locals, h.ResendEmailChange)
	app.Delete("/users/@me/email/pending", locals, h.CancelEmailChange)

	var body io.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	}
	req := httptest.NewRequest(c.method, c.path, body)
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out, res.Header
}

func TestEmailChangeHandlersRefuseNonPersons(t *testing.T) {
	person := &User{ID: 7, Email: "jane@example.com"}
	cases := map[string]emailCall{
		"bot token (no session)": {user: &User{ID: 8, Bot: true, Email: "b@bots.invalid"}},
		"bot with a session":     {user: &User{ID: 8, Bot: true}, session: true},
		"system account":         {user: &User{ID: 9, System: true, Email: "sys@x.test"}, session: true},
		"no session":             {user: person},
	}
	routes := []struct{ method, path, body string }{
		{"GET", "/users/@me/email", ""},
		{"POST", "/users/@me/email", `{"email":"a@b.co","password":"pw"}`},
		{"POST", "/users/@me/email/confirm", `{"token":"x"}`},
		{"POST", "/users/@me/email/pending/resend", ""},
		{"DELETE", "/users/@me/email/pending", ""},
	}
	for name, c := range cases {
		for _, r := range routes {
			svc := &fakeEmailSvc{}
			c.method, c.path, c.body = r.method, r.path, r.body
			status, body, hdr := runEmailCall(t, svc, c)
			if status != http.StatusForbidden || body["code"] != "session_required" {
				t.Errorf("%s %s %s: %d %v, want 403 session_required", name, r.method, r.path, status, body)
			}
			if body["error"] != "only a signed-in person can change the account's email" {
				t.Errorf("%s: message %q", name, body["error"])
			}
			if hdr.Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: Cache-Control = %q", name, r.path, hdr.Get("Cache-Control"))
			}
			if svc.calls != 0 {
				t.Errorf("%s %s: the service must not be reached", name, r.path)
			}
		}
	}
}

func TestRequestEmailChangeBody(t *testing.T) {
	u := &User{ID: 7, Email: "jane@example.com"}
	bad := map[string]string{
		"":                               "email and password are required",
		"not json":                       "email and password are required",
		`{"email":"a@b.co"}`:             "email and password are required",
		`{"password":"pw"}`:              "email and password are required",
		`{"email":"  ","password":"pw"}`: "email and password are required",
		`{"email":"a@b.co","password":"pw","code":"123456","recovery_code":"ABCDE-12345"}`: "send code or recovery_code, not both",
	}
	for in, msg := range bad {
		svc := &fakeEmailSvc{}
		status, body, hdr := runEmailCall(t, svc, emailCall{method: "POST", path: "/users/@me/email", body: in, user: u, session: true})
		if status != http.StatusBadRequest || body["code"] != "invalid_body" || body["error"] != msg {
			t.Errorf("%q: %d %v, want 400 invalid_body %q", in, status, body, msg)
		}
		if hdr.Get("Cache-Control") != "no-store" {
			t.Errorf("%q: Cache-Control = %q", in, hdr.Get("Cache-Control"))
		}
		if svc.calls != 0 {
			t.Errorf("%q: the service must not be reached", in)
		}
	}
	svc := &fakeEmailSvc{}
	status, body, _ := runEmailCall(t, svc, emailCall{method: "POST", path: "/users/@me/email/confirm", body: `{}`, user: u, session: true})
	if status != http.StatusBadRequest || body["code"] != "invalid_body" || body["error"] != "token is required" {
		t.Errorf("confirm without token: %d %v", status, body)
	}
}

func TestRequestEmailChangeStatusByMode(t *testing.T) {
	u := &User{ID: 7, Email: "jane@example.com"}
	call := emailCall{method: "POST", path: "/users/@me/email", body: `{"email":"jane@new.example","password":"pw","recovery_code":"ABCDE-12345"}`, user: u, session: true}

	svc := &fakeEmailSvc{}
	status, body, hdr := runEmailCall(t, svc, call)
	if status != http.StatusAccepted {
		t.Errorf("mail mode: %d, want 202", status)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", hdr.Get("Cache-Control"))
	}
	if svc.got.Email != "jane@new.example" || svc.got.Password != "pw" || svc.got.RecoveryCode != "ABCDE-12345" || svc.got.Code != "" {
		t.Errorf("service got %+v", svc.got)
	}
	pending, _ := body["pending"].(map[string]any)
	if body["email"] != "jane@example.com" || body["verified_email"] != true || pending["email"] != "jane@new.example" || pending["expires_at"] != "2026-11-15T15:04:05Z" {
		t.Errorf("EmailState JSON = %v", body)
	}

	svc = &fakeEmailSvc{applied: true}
	status, body, _ = runEmailCall(t, svc, call)
	if status != http.StatusOK {
		t.Errorf("direct mode: %d, want 200", status)
	}
	if v, ok := body["pending"]; !ok || v != nil {
		t.Errorf("pending must be present and null: %v", body)
	}
	if body["verified_email"] != false {
		t.Errorf("a direct change is unverified: %v", body)
	}
}

func TestEmailChangeOtherRoutes(t *testing.T) {
	u := &User{ID: 7, Email: "jane@example.com"}
	cases := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"GET", "/users/@me/email", "", http.StatusOK, ""},
		{"POST", "/users/@me/email/pending/resend", "", http.StatusAccepted, ""},
		{"DELETE", "/users/@me/email/pending", "", http.StatusOK, ""},
		{"POST", "/users/@me/email/confirm", `{"token":"abc"}`, http.StatusOK, ""},
		{"POST", "/users/@me/email/confirm", `{"token":"bad"}`, http.StatusBadRequest, "email_change_token_invalid"},
	}
	for _, tc := range cases {
		status, body, hdr := runEmailCall(t, &fakeEmailSvc{}, emailCall{method: tc.method, path: tc.path, body: tc.body, user: u, session: true})
		if status != tc.status {
			t.Errorf("%s %s %s: %d, want %d (%v)", tc.method, tc.path, tc.body, status, tc.status, body)
		}
		if tc.code != "" && body["code"] != tc.code {
			t.Errorf("%s %s: code %v, want %s", tc.method, tc.path, body["code"], tc.code)
		}
		if hdr.Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: Cache-Control = %q", tc.method, tc.path, hdr.Get("Cache-Control"))
		}
	}
}
