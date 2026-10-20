package routes

import (
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/gifs"
)

// SetupGifsRoutes wires the GIF picker's search, which this server makes on the client's
// behalf so the providers' API keys stay here. Signed-in accounts only (bots included):
// each call spends this instance's provider quota, so strangers get nothing and every
// account has its own budget.
func SetupGifsRoutes(d Deps) {
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)

	cfg := d.Config.Gifs
	// Whether each key is present, never what it is.
	logger.Info("gifs", "giphy=%s heypster=%s", setOrUnset(cfg.GiphyAPIKey), setOrUnset(cfg.HeypsterAPIKey))

	h := gifs.NewHandler(gifs.NewService(cfg, d.Redis, d.Config.Database.Redis.CachePrefix))

	d.App.Get("/gifs/search", requireAuth, perUserLimiter(60, time.Minute), h.Search)
	d.App.Get("/gifs/trending", requireAuth, perUserLimiter(60, time.Minute), h.Trending)
}

func setOrUnset(v string) string {
	if v == "" {
		return "unset"
	}
	return "set"
}
