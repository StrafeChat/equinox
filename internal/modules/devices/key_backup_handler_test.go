package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

const testBackupPassword = "correct horse battery staple"

// acceptTestPassword stands in for auth.VerifyUserPassword: same contract, no bcrypt.
func acceptTestPassword(_ context.Context, _ int64, password string) error {
	if password == testBackupPassword {
		return nil
	}
	return auth.ErrInvalidCredentials
}

// newBackupApp mounts the backup version endpoints behind a stub of RequireAuth that signs
// every request in as testBackupUser. A nil reauth leaves the hook unwired on purpose.
func newBackupApp(reauth func(context.Context, int64, string) error) (*fiber.App, *Service, *fakeBackupRepo) {
	svc, repo := newBackupService()
	h := NewHandler(svc, nil)
	if reauth != nil {
		h.SetReauth(reauth)
	}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(auth.LocalsKeyUser, &auth.User{ID: testBackupUser})
		return c.Next()
	})
	app.Post("/devices/backup/version", h.CreateBackupVersion)
	app.Get("/devices/backup/version", h.GetBackupVersion)
	app.Get("/devices/backup/recovery", h.GetBackupRecovery)
	app.Delete("/devices/backup/version", h.DeleteBackupVersion)
	return app, svc, repo
}

func call(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: body is not JSON: %q", method, path, raw)
		}
	}
	return res.StatusCode, out
}

func createBody(password string) string {
	b, _ := json.Marshal(map[string]any{
		"algorithm":           backupAlgorithm,
		"auth_data":           map[string]string{"public_key": "pk"},
		"wrapped_private_key": "wrapped",
		"salt":                "salt",
		"password":            password,
	})
	return string(b)
}

// The password is the step-up: a session token on its own (a hijacked tab, an admin's
// recovery-code reset) must not be able to point every device's uploads at a new key.
func TestCreateBackupVersionRequiresPassword(t *testing.T) {
	app, _, repo := newBackupApp(acceptTestPassword)

	status, body := call(t, app, http.MethodPost, "/devices/backup/version", createBody(""))
	if status != http.StatusBadRequest || body["error"] != "password is required" {
		t.Fatalf("no password: %d %v, want 400 password is required", status, body)
	}
	status, body = call(t, app, http.MethodPost, "/devices/backup/version", createBody("wrong"))
	if status != http.StatusForbidden || body["error"] != "invalid password" {
		t.Fatalf("wrong password: %d %v, want 403 invalid password", status, body)
	}
	if len(repo.versionIDs(testBackupUser)) != 0 {
		t.Fatal("a refused request still created a version")
	}

	status, body = call(t, app, http.MethodPost, "/devices/backup/version", createBody(testBackupPassword))
	if status != http.StatusOK || body["version"] == "" {
		t.Fatalf("right password: %d %v, want 200 with a version", status, body)
	}
	assertVersions(t, repo, mustParseID(t, body["version"]))
}

// The explicit delete takes the password too, from a JSON body - the version stays a query
// parameter for the client that already sends it that way.
func TestDeleteBackupVersionRequiresPassword(t *testing.T) {
	app, svc, repo := newBackupApp(acceptTestPassword)
	v := mustCreateBackup(t, svc)
	path := fmt.Sprintf("/devices/backup/version?version=%d", v.Version)

	status, body := call(t, app, http.MethodDelete, path, "")
	if status != http.StatusBadRequest || body["error"] != "password is required" {
		t.Fatalf("no body: %d %v, want 400 password is required", status, body)
	}
	status, body = call(t, app, http.MethodDelete, path, `{"password":"wrong"}`)
	if status != http.StatusForbidden || body["error"] != "invalid password" {
		t.Fatalf("wrong password: %d %v, want 403 invalid password", status, body)
	}
	assertVersions(t, repo, v.Version)

	status, _ = call(t, app, http.MethodDelete, path, `{"password":"`+testBackupPassword+`"}`)
	if status != http.StatusNoContent {
		t.Fatalf("right password: %d, want 204", status)
	}
	assertVersions(t, repo)

	status, _ = call(t, app, http.MethodDelete, "/devices/backup/version", `{"password":"`+testBackupPassword+`"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("missing version: %d, want 400", status)
	}
}

// Without the hook wired the endpoint would silently be back to token-only. It refuses.
func TestBackupVersionEndpointsFailClosedWithoutReauth(t *testing.T) {
	app, svc, repo := newBackupApp(nil)
	status, _ := call(t, app, http.MethodPost, "/devices/backup/version", createBody(testBackupPassword))
	if status != http.StatusInternalServerError {
		t.Fatalf("create without a re-auth hook: %d, want 500", status)
	}
	assertVersions(t, repo)

	v := mustCreateBackup(t, svc)
	status, _ = call(t, app, http.MethodDelete, fmt.Sprintf("/devices/backup/version?version=%d", v.Version), `{"password":"`+testBackupPassword+`"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("delete without a re-auth hook: %d, want 500", status)
	}
	assertVersions(t, repo, v.Version)
}

// The version poll tells a client there is something to fall back to, and the recovery
// endpoint serves either the current version or that one - and nothing else.
func TestBackupVersionReadsExposeTheRetainedPrevious(t *testing.T) {
	app, svc, _ := newBackupApp(acceptTestPassword)

	status, body := call(t, app, http.MethodGet, "/devices/backup/version", "")
	if status != http.StatusOK || body["exists"] != false {
		t.Fatalf("no backup: %d %v", status, body)
	}

	v1 := mustCreateBackup(t, svc)
	status, body = call(t, app, http.MethodGet, "/devices/backup/version", "")
	if status != http.StatusOK || body["version"] != id.Format(v1.Version) {
		t.Fatalf("one version: %d %v", status, body)
	}
	if _, present := body["previous"]; present {
		t.Fatalf("previous reported with a single version: %v", body["previous"])
	}

	v2 := mustCreateBackup(t, svc)
	status, body = call(t, app, http.MethodGet, "/devices/backup/version", "")
	if status != http.StatusOK || body["version"] != id.Format(v2.Version) {
		t.Fatalf("two versions: %d %v", status, body)
	}
	previous, _ := body["previous"].(map[string]any)
	if previous["version"] != id.Format(v1.Version) || previous["created_at"] == nil {
		t.Fatalf("previous = %v, want version %d with created_at", previous, v1.Version)
	}

	for _, tc := range []struct {
		query  string
		status int
		want   int64
	}{
		{"", http.StatusOK, v2.Version},
		{"?version=" + id.Format(v2.Version), http.StatusOK, v2.Version},
		{"?version=" + id.Format(v1.Version), http.StatusOK, v1.Version},
		{"?version=" + id.Format(v1.Version-1), http.StatusNotFound, 0},
		{"?version=not-a-snowflake", http.StatusBadRequest, 0},
	} {
		status, body := call(t, app, http.MethodGet, "/devices/backup/recovery"+tc.query, "")
		if status != tc.status {
			t.Fatalf("recovery%s: %d %v, want %d", tc.query, status, body, tc.status)
		}
		if tc.status == http.StatusOK && (body["version"] != id.Format(tc.want) || body["wrapped_private_key"] != "wrapped") {
			t.Fatalf("recovery%s: %v, want version %d with its wrapped key", tc.query, body, tc.want)
		}
	}
}

func mustParseID(t *testing.T, v any) int64 {
	t.Helper()
	s, _ := v.(string)
	parsed, err := id.Parse(s)
	if err != nil {
		t.Fatalf("not a snowflake: %v", v)
	}
	return parsed
}
