package spaces

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// rolesApp serves the role routes as routes/spaces.go mounts them, signed in as *as.
func rolesApp(s *Service, as *int64) *fiber.App {
	h := NewHandler(s, nil)
	users := s.userRepo.(*profileUsers)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(auth.LocalsKeyUser, users.users[*as])
		return c.Next()
	})
	app.Post("/spaces/:id/roles", h.CreateRole)
	app.Patch("/spaces/:id/roles", h.ReorderRoles)
	app.Patch("/spaces/:id/roles/:roleId", h.UpdateRole)
	app.Put("/spaces/:id/members/:userId/roles", h.PutMemberRoles)
	app.Put("/spaces/:id/members/:userId/roles/:roleId", h.PutMemberRole)
	app.Delete("/spaces/:id/members/:userId/roles/:roleId", h.DeleteMemberRole)
	return app
}

// roleHTTP is a fresh role fixture behind rolesApp: do sends a request as *as.
type roleHTTP struct {
	repo *orderRepo
	as   int64
	app  *fiber.App
}

func newRoleHTTP() *roleHTTP {
	h := &roleHTTP{repo: roleFixtureRepo(), as: rfOwnerID}
	s, _ := roleService(h.repo)
	h.app = rolesApp(s, &h.as)
	return h
}

func (h *roleHTTP) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := h.app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func errorOf(body string) string {
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Error
}

func TestRoleHandlers(t *testing.T) {
	t.Run("PATCH /spaces/:id/roles", func(t *testing.T) {
		h := newRoleHTTP()
		do := func(method, path, body string) (int, string) { return h.do(t, method, path, body) }
		code, body := do("PATCH", "/spaces/500/roles", `[{"id":"51","position":3},{"id":"53","position":1}]`)
		if code != 200 {
			t.Fatalf("reorder: %d %s", code, body)
		}
		var roles []struct {
			ID       string `json:"id"`
			Position int    `json:"position"`
		}
		if err := json.Unmarshal([]byte(body), &roles); err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(roles))
		for _, role := range roles {
			got = append(got, fmt.Sprintf("%s@%d", role.ID, role.Position))
		}
		if strings.Join(got, " ") != "50@0 53@1 52@2 51@3" {
			t.Errorf("roles %v", got)
		}

		cases := []struct {
			as         int64
			body       string
			code       int
			error      string
			errorStart string
		}{
			{rfOwnerID, `[{"id":"51","position":`, 400, "invalid JSON", ""},
			{rfOwnerID, `{"id":"51","position":1}`, 400, "invalid JSON", ""},
			{rfOwnerID, `[{"id":51,"position":1}]`, 400, "invalid JSON", ""},
			{rfOwnerID, `[]`, 400, ErrInvalidRolePositions.Error(), ""},
			{rfOwnerID, `null`, 400, ErrInvalidRolePositions.Error(), ""},
			{rfOwnerID, `[{"id":"51","position":1},{"id":"51","position":2}]`, 400, ErrInvalidRolePositions.Error(), ""},
			{rfOwnerID, `[{"id":"x","position":1}]`, 400, "invalid role id in list", ""},
			{rfOwnerID, `[{"id":"77","position":1}]`, 400, "unknown role id", ""},
			{rfOwnerID, `[{"id":"51","position":0}]`, 400, "", "role position must be from 1 to "},
			{rfOwnerID, `[{"id":"51"}]`, 400, "", "role position must be from 1 to "},
			{rfOwnerID, `[{"id":"50","position":1}]`, 400, "cannot move @everyone", ""},
			{rfModID, `[{"id":"53","position":1}]`, 403, ErrRoleHierarchy.Error(), ""},
			{rfModID, `[{"id":"51","position":2}]`, 403, ErrRoleHierarchy.Error(), ""},
			{rfMemberID, `[{"id":"51","position":1}]`, 403, "missing permission", ""},
		}
		// On a fresh fixture: the reorder above put admin below mod.
		h = newRoleHTTP()
		for _, tc := range cases {
			h.as = tc.as
			code, body := do("PATCH", "/spaces/500/roles", tc.body)
			msg := errorOf(body)
			if code != tc.code || (tc.error != "" && msg != tc.error) || !strings.HasPrefix(msg, tc.errorStart) {
				t.Errorf("as %d %s: %d %s, want %d %q%s", tc.as, tc.body, code, body, tc.code, tc.error, tc.errorStart)
			}
		}
	})

	t.Run("one member role at a time", func(t *testing.T) {
		h := newRoleHTTP()
		h.as = rfModID
		do := func(method, path, body string) (int, string) { return h.do(t, method, path, body) }
		r := h.repo
		cases := []struct {
			method, path string
			code         int
			error        string
		}{
			{"PUT", "/spaces/500/members/5/roles/51", 204, ""},
			{"PUT", "/spaces/500/members/5/roles/51", 204, ""}, // already held
			{"DELETE", "/spaces/500/members/5/roles/51", 204, ""},
			{"DELETE", "/spaces/500/members/5/roles/51", 204, ""}, // not held
			{"PUT", "/spaces/500/members/1/roles/51", 204, ""},    // the owner
			{"PUT", "/spaces/500/members/2/roles/51", 204, ""},    // a member who outranks the actor
			{"PUT", "/spaces/500/members/@me/roles/51", 204, ""},
			{"PUT", "/spaces/500/members/5/roles/77", 404, "role not found"},
			{"PUT", "/spaces/500/members/5/roles/50", 400, ErrEveryoneRole.Error()},
			{"PUT", "/spaces/500/members/5/roles/52", 403, ErrRoleHierarchy.Error()},
			{"DELETE", "/spaces/500/members/2/roles/53", 403, ErrRoleHierarchy.Error()},
			{"PUT", "/spaces/500/members/99/roles/51", 403, ErrNotMember.Error()},
			{"PUT", "/spaces/500/members/5/roles/abc", 400, "invalid role id"},
			{"PUT", "/spaces/500/members/abc/roles/51", 400, "invalid user id"},
			{"PUT", "/spaces/999/members/5/roles/51", 404, "space not found"},
		}
		for _, tc := range cases {
			code, body := do(tc.method, tc.path, "")
			if code != tc.code || (tc.error != "" && errorOf(body) != tc.error) {
				t.Errorf("%s %s: %d %s, want %d %q", tc.method, tc.path, code, body, tc.code, tc.error)
			}
		}
		if !hasRoleID(r.roleIDsOf(rfSpace, rfOwnerID), rfMember) || !hasRoleID(r.roleIDsOf(rfSpace, rfModID), rfMember) {
			t.Error("the owner's or the actor's own grant did not land")
		}
	})

	t.Run("the full list reaches the owner too", func(t *testing.T) {
		h := newRoleHTTP()
		h.as = rfModID
		do := func(method, path, body string) (int, string) { return h.do(t, method, path, body) }
		if code, body := do("PUT", "/spaces/500/members/1/roles", `{"role_ids":[]}`); code != 204 {
			t.Errorf("mod edits the owner's roles: %d %s", code, body)
		}
		if code, body := do("PUT", "/spaces/500/members/1/roles", `{"role_ids":["53"]}`); code != 403 || errorOf(body) != ErrRoleHierarchy.Error() {
			t.Errorf("mod gives the owner admin: %d %s", code, body)
		}
	})

	t.Run("create at 1, and a position must be at least 1", func(t *testing.T) {
		h := newRoleHTTP()
		do := func(method, path, body string) (int, string) { return h.do(t, method, path, body) }
		r := h.repo
		code, body := do("POST", "/spaces/500/roles", `{"name":"Fresh"}`)
		var role struct {
			Position int `json:"position"`
		}
		_ = json.Unmarshal([]byte(body), &role)
		if code != 201 || role.Position != 1 {
			t.Errorf("create: %d %s", code, body)
		}
		if code, body := do("PATCH", "/spaces/500/roles/51", `{"position":0}`); code != 400 || !strings.HasPrefix(errorOf(body), "role position must be from 1 to ") {
			t.Errorf("position 0: %d %s", code, body)
		}
		if code, body := do("PATCH", "/spaces/500/roles/50", `{"position":2}`); code != 400 || errorOf(body) != "cannot move @everyone" {
			t.Errorf("move @everyone: %d %s", code, body)
		}
		assertUniquePositions(t, r, rfSpace)
	})
}

func TestRoleOrderHTTPErrors(t *testing.T) {
	cases := []struct {
		err    error
		status int
	}{
		{ErrCannotMoveEveryone, 400},
		{ErrInvalidRolePosition, 400},
		{ErrInvalidRolePositions, 400},
		{ErrEveryoneRole, 400},
		{ErrManagedRole, 400},
		{ErrRoleHierarchy, 403},
		{ErrRoleNotFound, 404},
		{ErrTooManyRoles, 400},
	}
	for _, tc := range cases {
		status, msg, ok := HTTPError(tc.err)
		if !ok || status != tc.status || msg != tc.err.Error() {
			t.Errorf("%v: got (%d, %q, %v), want %d", tc.err, status, msg, ok, tc.status)
		}
	}
}
