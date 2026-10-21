package spaces

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// nebulaPut is one object the upload handler stored.
type nebulaPut struct{ key, contentType string }

// uploadNebulaStub is a Nebula that accepts uploads (201, as nebula.Put requires) and
// deletes, reporting each on its own channel. The other stub in member_profile_test.go only
// answers 204, which an upload treats as a failure.
func uploadNebulaStub(t *testing.T) (*config.Config, <-chan nebulaPut, <-chan string) {
	t.Helper()
	puts := make(chan nebulaPut, 8)
	deleted := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.URL.Path, "/v1/")
		switch req.Method {
		case http.MethodPut:
			_, _ = io.Copy(io.Discard, req.Body)
			puts <- nebulaPut{key: key, contentType: req.Header.Get("Content-Type")}
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			deleted <- key
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{}
	cfg.Nebula.BaseURL = srv.URL
	cfg.Nebula.PublicURL = "https://cdn.test"
	cfg.Nebula.UploadSecret = "secret"
	cfg.Nebula.AvatarMaxBytes = 8
	cfg.Nebula.BannerMaxBytes = 0 // falls back to the avatar limit, as users/upload.go does
	return cfg, puts, deleted
}

// uploadApp mounts the two upload routes as routes/spaces.go does, with Nebula configured.
func uploadApp(r *profileRepo, users *profileUsers, cfg *config.Config, as *int64) *fiber.App {
	h := NewHandler(NewService(r, nil, users, nil, cfg), cfg)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(auth.LocalsKeyUser, users.users[*as])
		return c.Next()
	})
	app.Post("/spaces/:id/members/@me/avatar", h.PostMemberAvatar)
	app.Post("/spaces/:id/members/@me/banner", h.PostMemberBanner)
	return app
}

// multipartFile is a request body with one "file" part (or, with field "", no file at all).
func multipartFile(t *testing.T, field, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if field != "" {
		part, err := w.CreateFormFile(field, filename)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(content)
	} else if err := w.WriteField("note", "no file here"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

// expectNoPut fails if anything reached Nebula: the refusals must come before the upload.
func expectNoPut(t *testing.T, puts <-chan nebulaPut) {
	t.Helper()
	select {
	case p := <-puts:
		t.Fatalf("unexpected upload %+v", p)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPostMemberImage(t *testing.T) {
	type result struct {
		code int
		body map[string]interface{}
	}
	post := func(t *testing.T, app *fiber.App, path, field, filename string, content []byte) result {
		t.Helper()
		body, ctype := multipartFile(t, field, filename, content)
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Content-Type", ctype)
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]interface{}
		_ = json.NewDecoder(res.Body).Decode(&out)
		return result{res.StatusCode, out}
	}
	setup := func(t *testing.T) (*profileRepo, *config.Config, <-chan nebulaPut, <-chan string, *fiber.App, *int64) {
		t.Helper()
		r := profileFixture()
		users := &profileUsers{users: map[int64]*auth.User{}}
		for uid := range r.members {
			users.users[uid] = &auth.User{ID: uid, Username: "user" + strings.Repeat("x", int(uid)), Avatar: "https://cdn.test/v1/avatars/global.png"}
		}
		users.users[99] = &auth.User{ID: 99, Username: "stranger"}
		cfg, puts, deleted := uploadNebulaStub(t)
		as := int64(pfPlain)
		return r, cfg, puts, deleted, uploadApp(r, users, cfg, &as), &as
	}

	t.Run("upload sets the space avatar and releases the one it replaces", func(t *testing.T) {
		r, _, puts, deleted, app, _ := setup(t)
		oldKey, oldURL := profileBlob(pfPlain, "avatar-0123456789abcdef.png")
		r.members[pfPlain].Avatar = oldURL

		res := post(t, app, "/spaces/100/members/@me/avatar", "file", "me.PNG", []byte("png"))
		if res.code != 200 {
			t.Fatalf("upload: %d %v", res.code, res.body)
		}
		url, _ := res.body["space_avatar"].(string)
		if !regexp.MustCompile(`^https://cdn\.test/v1/spaces/members/100/5/avatar-[0-9a-f]{16}\.png$`).MatchString(url) {
			t.Errorf("space_avatar %q", url)
		}
		// The global keys stay the global profile.
		if res.body["avatar"] != "https://cdn.test/v1/avatars/global.png" {
			t.Errorf("global avatar %v", res.body["avatar"])
		}
		p := <-puts
		if "https://cdn.test/v1/"+p.key != url || p.contentType != "image/png" {
			t.Errorf("stored %+v for %q", p, url)
		}
		if r.members[pfPlain].Avatar != url {
			t.Errorf("row avatar %q", r.members[pfPlain].Avatar)
		}
		expectBlobDeletes(t, deleted, oldKey)
	})

	t.Run("a banner without an extension is a png, within the avatar limit", func(t *testing.T) {
		r, _, puts, deleted, app, _ := setup(t)
		res := post(t, app, "/spaces/100/members/@me/banner", "file", "banner", []byte("12345678"))
		if res.code != 200 {
			t.Fatalf("banner: %d %v", res.code, res.body)
		}
		p := <-puts
		if !memberBlobNameRe.MatchString(strings.TrimPrefix(p.key, memberBlobPrefix(pfSpace, pfPlain))) || !strings.HasPrefix(p.key, memberBlobPrefix(pfSpace, pfPlain)+"banner-") || !strings.HasSuffix(p.key, ".png") {
			t.Errorf("banner key %q", p.key)
		}
		if res.body["space_banner"] != "https://cdn.test/v1/"+p.key || r.members[pfPlain].Banner == "" {
			t.Errorf("space_banner %v", res.body["space_banner"])
		}
		// Nothing was set before, so nothing is released.
		expectBlobDeletes(t, deleted)

		res = post(t, app, "/spaces/100/members/@me/banner", "file", "big.png", []byte("123456789"))
		if res.code != 400 || res.body["error"] != "invalid file size" {
			t.Errorf("oversized banner: %d %v", res.code, res.body)
		}
		expectNoPut(t, puts)
	})

	t.Run("refusals come before anything is stored", func(t *testing.T) {
		_, _, puts, _, app, as := setup(t)
		cases := []struct {
			name, field, filename string
			content               []byte
			wantCode              int
			wantErr               string
		}{
			{"no file field", "", "", nil, 400, "missing file field"},
			{"empty file", "file", "a.png", []byte{}, 400, "invalid file size"},
			{"too big", "file", "a.png", []byte("123456789"), 400, "invalid file size"},
			{"unsupported type", "file", "a.bmp", []byte("bmp"), 400, "unsupported image type (use jpg, png, gif, or webp)"},
		}
		for _, tc := range cases {
			res := post(t, app, "/spaces/100/members/@me/avatar", tc.field, tc.filename, tc.content)
			if res.code != tc.wantCode || res.body["error"] != tc.wantErr {
				t.Errorf("%s: %d %v", tc.name, res.code, res.body)
			}
		}
		// A stranger cannot fill this instance's CDN by naming a space they are not in.
		*as = 99
		if res := post(t, app, "/spaces/100/members/@me/avatar", "file", "a.png", []byte("png")); res.code != 403 || res.body["error"] != ErrNotMember.Error() {
			t.Errorf("stranger: %d %v", res.code, res.body)
		}
		expectNoPut(t, puts)
	})

	t.Run("a failed profile write deletes the new image", func(t *testing.T) {
		r, _, puts, deleted, app, _ := setup(t)
		// The member leaves between the upload and the profile write.
		r.beforeWrite = func() { delete(r.members, pfPlain) }
		res := post(t, app, "/spaces/100/members/@me/avatar", "file", "a.webp", []byte("webp"))
		if res.code != 404 || res.body["error"] != ErrMemberNotFound.Error() {
			t.Fatalf("got %d %v", res.code, res.body)
		}
		p := <-puts
		if p.contentType != "image/webp" {
			t.Errorf("content type %q", p.contentType)
		}
		// Nothing points at it, so it does not stay on the CDN.
		expectBlobDeletes(t, deleted, p.key)
	})
}
