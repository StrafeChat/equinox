package users

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// GET /users/@me is one of the user's own views of their presence: it carries what their
// devices report, even while invisible - but not to a third-party application, which no
// scope entitles to the game.
func TestMeCarriesOwnActivity(t *testing.T) {
	store := stargate.NewMemoryActivityStore()
	prev := stargate.CurrentActivityStore()
	stargate.SetActivityStore(store)
	t.Cleanup(func() { stargate.SetActivityStore(prev) })
	_, _ = store.SetConn(context.Background(), 7, 70, auth.Activity{Type: "playing", Name: "Minecraft", StartedAt: 1}, time.Now().UnixMilli())

	me := func(p auth.UserPresence, scopes []string) auth.PublicPresence {
		t.Helper()
		app := fiber.New()
		app.Get("/users/@me", func(c fiber.Ctx) error {
			c.Locals(auth.LocalsKeyUser, &auth.User{ID: 7, Username: "gamer", Presence: p})
			if scopes != nil {
				c.Locals(middleware.LocalsKeyScopes, scopes)
			}
			return c.Next()
		}, NewHandler(nil, nil, nil, nil).Me)
		res, err := app.Test(httptest.NewRequest("GET", "/users/@me", nil))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body struct {
			Presence auth.PublicPresence `json:"presence"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Presence
	}

	if p := me(auth.UserPresence{Online: true, Status: "online"}, nil); len(p.Activities) != 1 || p.Activities[0].Name != "Minecraft" {
		t.Errorf("signed in: %+v", p)
	}
	if p := me(auth.UserPresence{Online: true, Status: "invisible"}, nil); p.Status != "invisible" || len(p.Activities) != 1 {
		t.Errorf("invisible, own view: %+v", p)
	}
	if p := me(auth.UserPresence{Online: false, Status: "online"}, nil); len(p.Activities) != 0 {
		t.Errorf("offline: %+v", p)
	}
	if p := me(auth.UserPresence{Online: true, Status: "online"}, []string{"identify"}); p.Status != "online" || len(p.Activities) != 0 {
		t.Errorf("an OAuth2 application: %+v", p)
	}
}
